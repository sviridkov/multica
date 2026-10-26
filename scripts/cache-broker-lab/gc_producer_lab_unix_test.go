//go:build cachebrokerlab && darwin

package daemon

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func nativeProducerQuotaError(err error) bool {
	return errors.Is(err, syscall.ENOSPC) || errors.Is(err, syscall.EDQUOT)
}

func nativeStorageAvailable(t *testing.T, path string) int64 {
	t.Helper()
	var fs syscall.Statfs_t
	if err := syscall.Statfs(path, &fs); err != nil {
		t.Fatal(err)
	}
	return int64(fs.Bavail) * int64(fs.Bsize)
}

func nativeProducerDiagnostics(d *nativeRuntimeDomain) string {
	out, err := exec.Command("/bin/ps", "-U", strconv.FormatUint(uint64(d.id.UID), 10), "-o", "pid=,ppid=,ruid=,uid=,stat=,comm=").CombinedOutput()
	return fmt.Sprintf("RUID=%d ps_error=%v processes=%s", d.id.UID, err, out)
}

func nativeProducerVolume(t *testing.T, cfg nativeLab) string {
	t.Helper()
	root, err := os.MkdirTemp(cfg.Root, "quota-")
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Chmod(root, 0755); err != nil {
		t.Fatal(err)
	}
	image, mount := filepath.Join(root, "staging.dmg"), filepath.Join(root, "volume")
	if err = os.Mkdir(mount, 0755); err != nil {
		t.Fatal(err)
	}
	volumeDevice := ""
	for _, args := range [][]string{{"create", "-size", "128m", "-fs", "APFS", "-type", "UDIF", "-nospotlight", "-volname", "CacheProducerStage", image}, {"attach", "-plist", "-nobrowse", "-owners", "on", "-mountpoint", mount, image}} {
		command := exec.Command("/usr/bin/hdiutil", args...)
		var diagnostics bytes.Buffer
		command.Stderr = &diagnostics
		out, err := command.Output()
		if err != nil {
			t.Fatalf("bounded staging volume: %v %s %s", err, out, diagnostics.String())
		}
		if diagnostics.Len() != 0 {
			t.Logf("hdiutil %s: %s", args[0], diagnostics.String())
		}
		if args[0] == "attach" {
			extract := exec.Command("/usr/bin/plutil", "-extract", "system-entities.0.dev-entry", "raw", "-o", "-", "-")
			extract.Stdin = bytes.NewReader(out)
			device, err := extract.CombinedOutput()
			if err != nil {
				t.Fatalf("attached image device: %v %s", err, device)
			}
			volumeDevice = strings.TrimSpace(string(device))
			number, err := strconv.ParseUint(strings.TrimPrefix(volumeDevice, "/dev/disk"), 10, 32)
			if err != nil || number == 0 || volumeDevice != fmt.Sprintf("/dev/disk%d", number) {
				t.Fatal("unexpected attached image device", volumeDevice)
			}
		}
	}
	if err = os.Chown(mount, 0, 0); err != nil {
		t.Fatal(err)
	}
	if err = os.Chmod(mount, 0755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		for attempt := 1; ; attempt++ {
			// Detach can unmount first, then fail to eject with EBUSY. Keep the
			// original image device; its mount path no longer names the device.
			out, err := exec.CommandContext(ctx, "/usr/bin/hdiutil", "detach", volumeDevice).CombinedOutput()
			if err == nil {
				if attempt > 1 {
					t.Logf("QUOTA_DETACH settled_after_attempts=%d forced=false image_retained=true", attempt)
				}
				return
			}
			var status *exec.ExitError
			if ctx.Err() != nil || !errors.As(err, &status) || status.ExitCode() != int(syscall.EBUSY) {
				t.Errorf("detach disposable quota image (image retained): %v %s", err, out)
				return
			}
			// A busy volume remains mounted and intact. Retry only the ordinary
			// detach; never force unmount or discard retained staging data.
			t.Logf("QUOTA_DETACH_BUSY attempt=%d forced=false image_retained=true", attempt)
			select {
			case <-ctx.Done():
				t.Errorf("disposable quota image remained busy (image retained): %v %s", ctx.Err(), out)
				return
			case <-time.After(100 * time.Millisecond):
			}
		}
	})
	t.Logf("STAGING_QUOTA filesystem=APFS device=%s fixed_image_bytes=134217728 task_results_separate=true", volumeDevice)
	return mount
}
func nativeProducerStage(t *testing.T, volume string, id runtimeIdentity) string {
	t.Helper()
	path, err := os.MkdirTemp(volume, "stage-")
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Chown(path, int(id.UID), int(id.GID)); err != nil {
		t.Fatal(err)
	}
	return path
}
func nativeProducerReleaseOwner(t *testing.T, path string, id runtimeIdentity) {
	t.Helper()
	if err := os.Chown(path, int(id.UID), int(id.GID)); err != nil {
		t.Fatal(err)
	}
}
func newNativeProducerDomain(t *testing.T, cfg nativeLab, c producerConfig) *nativeProducerDomain {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	// Git's protected helpers may execute, but the inherited sandbox denies
	// external launch IPC and confines stored output to this bounded acquisition.
	profile := fmt.Sprintf(`(version 1)
(deny default)
(allow file-read*)
(allow sysctl-read)
(allow process-fork)
(allow process-exec)
(allow signal (target same-sandbox))
(allow file-write* (subpath %q))
(allow file-write-data (literal "/dev/null"))
(allow network-inbound (local ip "localhost:*"))
(allow network-outbound (remote ip "localhost:*"))
`, c.Stage)
	// sandbox-exec reads this after dropping UID; only the profile is public.
	path := filepath.Join(cfg.Root, "producer-sandbox-"+filepath.Base(c.Control)+".sb")
	if err = os.WriteFile(path, []byte(profile), 0644); err != nil {
		t.Fatal(err)
	}
	d := &nativeRuntimeDomain{id: c.Identity, census: cfg.Census, profile: path}
	if n, err := d.Active(); err != nil || n != 0 {
		t.Fatal("producer identity not fresh", n, err)
	}
	return &nativeProducerDomain{inner: d, config: c, exe: exe}
}
func nativeProducerCrashWatch(t *testing.T, cfg nativeLab, c producerConfig) func() {
	t.Helper()
	return func() {
		d := &nativeRuntimeDomain{id: c.Identity, census: cfg.Census}
		if n, err := d.Active(); err != nil || n == 0 {
			t.Fatal("expected detached producer to survive supervisor crash", n, err)
		}
		t.Log("PRODUCER_CRASH_NATIVE detached_writer_survived=true; recovery must retain")
	}
}
func nativeProducerDrain(t *testing.T, cfg nativeLab, c producerConfig) {
	t.Helper()
	d := &nativeRuntimeDomain{id: c.Identity, census: cfg.Census}
	deadline := time.Now().Add(20 * time.Second)
	for {
		n, err := d.Active()
		if err != nil {
			t.Fatal(err)
		}
		if n == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("orphan not drained", nativeProducerDiagnostics(d))
		}
		time.Sleep(20 * time.Millisecond)
	}
}
