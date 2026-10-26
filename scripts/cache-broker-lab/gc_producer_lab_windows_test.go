//go:build cachebrokerlab && windows

package daemon

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

func nativeProducerQuotaError(err error) bool {
	return errors.Is(err, windows.ERROR_DISK_FULL) || errors.Is(err, windows.ERROR_HANDLE_DISK_FULL) || errors.Is(err, windows.ERROR_DISK_QUOTA_EXCEEDED)
}

func nativeStorageAvailable(t *testing.T, path string) int64 {
	t.Helper()
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		t.Fatal(err)
	}
	var available, total, free uint64
	if err := windows.GetDiskFreeSpaceEx(p, &available, &total, &free); err != nil {
		t.Fatal(err)
	}
	return int64(available)
}

func nativeProducerDiagnostics(d *nativeRuntimeDomain) string {
	active, err := d.Active()
	return fmt.Sprintf("Job active=%d query_error=%v", active, err)
}

func nativeProducerVolume(t *testing.T, cfg nativeLab) string {
	t.Helper()
	root, err := os.MkdirTemp(cfg.Root, "quota-")
	if err != nil {
		t.Fatal(err)
	}
	image := filepath.Join(root, "staging.vhd")
	drive := ""
	for _, letter := range "QRSTUVWXYZ" {
		path := string(letter) + ":\\"
		if _, err := os.Stat(path); os.IsNotExist(err) {
			drive = string(letter)
			break
		}
	}
	if drive == "" {
		t.Fatal("no disposable drive letter")
	}
	script := filepath.Join(root, "diskpart.txt")
	body := fmt.Sprintf("create vdisk file=\"%s\" maximum=128 type=fixed\r\nselect vdisk file=\"%s\"\r\nattach vdisk\r\ncreate partition primary\r\nformat fs=ntfs label=CacheProducerStage quick\r\nassign letter=%s\r\n", image, image, drive)
	if err = os.WriteFile(script, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("diskpart.exe", "/s", script).CombinedOutput()
	if err != nil {
		t.Fatalf("fixed VHD: %v %s", err, out)
	}
	mount := drive + ":\\"
	if _, err = os.Stat(mount); err != nil {
		t.Fatalf("VHD was not mounted: %v %s", err, out)
	}
	// A freshly formatted NTFS root has explicit ACEs. Granting selected SIDs
	// does not remove those defaults, which would propagate into service state.
	descriptor, err := windows.SecurityDescriptorFromString("D:P(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)(A;OICI;GRGX;;;BU)")
	if err != nil {
		t.Fatal(err)
	}
	acl, _, err := descriptor.DACL()
	if err != nil {
		t.Fatal(err)
	}
	if err = windows.SetNamedSecurityInfo(mount, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, acl, nil); err != nil {
		t.Fatal("quota root ACL:", err)
	}
	t.Cleanup(func() {
		path := filepath.Join(root, "detach.txt")
		if err := os.WriteFile(path, []byte(fmt.Sprintf("select vdisk file=\"%s\"\r\ndetach vdisk\r\n", image)), 0600); err != nil {
			t.Error(err)
			return
		}
		if out, err := exec.Command("diskpart.exe", "/s", path).CombinedOutput(); err != nil {
			t.Errorf("detach disposable VHD: %v %s", err, out)
		}
	})
	t.Log("STAGING_QUOTA filesystem=NTFS fixed_image_bytes=134217728 task_results_separate=true")
	return mount
}
func nativeProducerRestorePrivilege(t *testing.T) {
	t.Helper()
	var token windows.Token
	if err := windows.OpenProcessToken(windows.CurrentProcess(), windows.TOKEN_ADJUST_PRIVILEGES|windows.TOKEN_QUERY, &token); err != nil {
		t.Fatal(err)
	}
	defer token.Close()
	name, err := windows.UTF16PtrFromString("SeRestorePrivilege")
	if err != nil {
		t.Fatal(err)
	}
	var luid windows.LUID
	if err = windows.LookupPrivilegeValue(nil, name, &luid); err != nil {
		t.Fatal(err)
	}
	priv := windows.Tokenprivileges{PrivilegeCount: 1}
	priv.Privileges[0].Luid = luid
	priv.Privileges[0].Attributes = windows.SE_PRIVILEGE_ENABLED
	if err = windows.AdjustTokenPrivileges(token, false, &priv, uint32(unsafe.Sizeof(priv)), nil, nil); err != nil {
		t.Fatal(err)
	}
}
func nativeProducerStage(t *testing.T, volume string, id runtimeIdentity) string {
	t.Helper()
	path, err := os.MkdirTemp(volume, "stage-")
	if err != nil {
		t.Fatal(err)
	}
	sd, err := windows.SecurityDescriptorFromString("D:P(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)(A;OICI;FA;;;" + id.SID + ")")
	if err != nil {
		t.Fatal(err)
	}
	acl, _, err := sd.DACL()
	if err != nil {
		t.Fatal(err)
	}
	nativeProducerRestorePrivilege(t)
	sid, err := windows.StringToSid(id.SID)
	if err != nil {
		t.Fatal(err)
	}
	if err = windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, sid, nil, acl, nil); err != nil {
		t.Fatal(err)
	}
	return path
}
func nativeProducerReleaseOwner(t *testing.T, path string, id runtimeIdentity) {
	t.Helper()
	nativeProducerRestorePrivilege(t)
	sid, err := windows.StringToSid(id.SID)
	if err != nil {
		t.Fatal(err)
	}
	if err = windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION, sid, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
}
func newNativeProducerDomain(t *testing.T, cfg nativeLab, c producerConfig) *nativeProducerDomain {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return &nativeProducerDomain{inner: newNativeRuntimeDomain(t, cfg, c.Identity, c.Stage), config: c, exe: exe}
}
func nativeProducerCrashWatch(t *testing.T, cfg nativeLab, c producerConfig) func() {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(c.Stage, "descendant-ready"))
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		t.Fatal(err)
	}
	h, err := windows.OpenProcess(windows.SYNCHRONIZE|windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { windows.CloseHandle(h) })
	return func() {
		state, err := windows.WaitForSingleObject(h, 5000)
		if err != nil || state != windows.WAIT_OBJECT_0 {
			t.Fatal("Job did not terminate descendant on supervisor crash", state, err)
		}
		t.Log("PRODUCER_CRASH_NATIVE kill_on_job_close_terminated_descendant=true; native proof still lost")
	}
}
func nativeProducerDrain(t *testing.T, cfg nativeLab, c producerConfig) {}
