//go:build cachebrokerlab && darwin

package daemon

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

type nativeRuntimeDomain struct {
	id              runtimeIdentity
	census, profile string
}

func nativeRuntimeIdentity(id runtimeIdentity) string { return fmt.Sprintf("ruid:%d", id.UID) }
func newNativeRuntimeDomain(t *testing.T, cfg nativeLab, id runtimeIdentity, root string) *nativeRuntimeDomain {
	t.Helper()
	if id.UID == 0 || id.GID == 0 || cfg.Census == "" {
		t.Fatal("missing isolated runtime identity/census")
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	// Only the measured fixture executable may fork/exec. No external launch
	// broker, network, privilege transition, task injection or control IPC.
	profile := fmt.Sprintf(`(version 1)
(deny default)
(allow file-read*)
(allow sysctl-read)
(allow process-fork)
(allow process-exec (literal %q))
(allow signal (target same-sandbox))
(allow file-write* (subpath %q) (subpath %q))
`, exe, filepath.Join(root, "codex-home"), filepath.Join(root, "workspace"))
	path := filepath.Join(cfg.Root, "sandbox-"+strconv.FormatUint(uint64(id.UID), 10)+".sb")
	if err = os.WriteFile(path, []byte(profile), 0644); err != nil {
		t.Fatal(err)
	}
	d := &nativeRuntimeDomain{id: id, census: cfg.Census, profile: path}
	if n, err := d.Active(); err != nil || n != 0 {
		t.Fatalf("fresh UID is not empty: %d %v", n, err)
	}
	return d
}
func (d *nativeRuntimeDomain) Start(cmd *exec.Cmd) error {
	exe, args := cmd.Path, append([]string(nil), cmd.Args[1:]...)
	cmd.Path = "/usr/bin/sandbox-exec"
	cmd.Args = append([]string{cmd.Path, "-f", d.profile, exe}, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: d.id.UID, Gid: d.id.GID, Groups: []uint32{d.id.GID}}}
	return cmd.Start()
}
func (d *nativeRuntimeDomain) Active() (uint32, error) {
	data, err := exec.Command(d.census, strconv.FormatUint(uint64(d.id.UID), 10)).Output()
	if err != nil {
		return 0, fmt.Errorf("native RUID census unavailable: %w", err)
	}
	n, err := strconv.ParseUint(strings.TrimSpace(string(data)), 10, 32)
	return uint32(n), err
}
func nativeRuntimeHome(t *testing.T, root string, id runtimeIdentity) {
	t.Helper()
	for _, path := range []string{filepath.Join(root, "codex-home"), filepath.Join(root, "codex-home/.tmp"), filepath.Join(root, "codex-home/cache")} {
		if err := os.MkdirAll(path, 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.Chown(path, 0, int(id.GID)); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, 0770|os.ModeSticky); err != nil {
			t.Fatal(err)
		}
	}
	work := filepath.Join(root, "workspace")
	if err := os.Mkdir(work, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chown(work, int(id.UID), int(id.GID)); err != nil {
		t.Fatal(err)
	}
}
func nativeRuntimeDetach(cmd *exec.Cmd) { cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true} }
func nativeRuntimeEscapeProbe(t *testing.T) {
	t.Helper()
	if err := syscall.Setuid(0); err == nil {
		t.Fatal("runtime changed protected identity")
	}
	for _, exe := range []string{"/usr/bin/true", "/bin/launchctl", "/usr/bin/crontab"} {
		if err := exec.Command(exe).Run(); err == nil {
			t.Fatal("unallowlisted execution escaped sandbox", exe)
		}
	}
}

func nativeRuntimeFailQuery(d *nativeRuntimeDomain) func() {
	old := d.census
	d.census = old + ".missing"
	return func() { d.census = old }
}
