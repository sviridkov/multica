//go:build cachebrokerlab && windows

package daemon

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

type nativeRuntimeDomain struct {
	job   windows.Handle
	token windows.Token
}
type runtimeJobAccounting struct {
	User, Kernel, PeriodUser, PeriodKernel int64
	Faults, Total, Active, Terminated      uint32
}

func nativeRuntimeIdentity(id runtimeIdentity) string { return id.SID }
func newNativeRuntimeDomain(t *testing.T, cfg nativeLab, id runtimeIdentity, root string) *nativeRuntimeDomain {
	t.Helper()
	u, _ := windows.UTF16PtrFromString(id.User)
	domain, _ := windows.UTF16PtrFromString(".")
	pw, _ := windows.UTF16PtrFromString(id.Password)
	var token windows.Token
	ok, _, e := windows.NewLazySystemDLL("advapi32.dll").NewProc("LogonUserW").Call(uintptr(unsafe.Pointer(u)), uintptr(unsafe.Pointer(domain)), uintptr(unsafe.Pointer(pw)), 2, 0, uintptr(unsafe.Pointer(&token)))
	if ok == 0 {
		t.Fatal(e)
	}
	user, err := token.GetTokenUser()
	if err != nil || user.User.Sid.String() != id.SID || token.IsElevated() {
		t.Fatal("unexpected task token", err)
	}
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	limits := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	limits.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err = windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation, uintptr(unsafe.Pointer(&limits)), uint32(unsafe.Sizeof(limits))); err != nil {
		t.Fatal(err)
	}
	if err = windows.SetHandleInformation(job, windows.HANDLE_FLAG_INHERIT, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { windows.CloseHandle(job); token.Close() })
	return &nativeRuntimeDomain{job: job, token: token}
}
func (d *nativeRuntimeDomain) Start(cmd *exec.Cmd) error {
	cmd.SysProcAttr = &syscall.SysProcAttr{Token: syscall.Token(d.token), CreationFlags: windows.CREATE_SUSPENDED, HideWindow: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	fail := func(err error) error { cmd.Process.Kill(); cmd.Wait(); return err }
	h, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE|windows.PROCESS_SUSPEND_RESUME, false, uint32(cmd.Process.Pid))
	if err != nil {
		return fail(err)
	}
	defer windows.CloseHandle(h)
	if err = windows.AssignProcessToJobObject(d.job, h); err != nil {
		return fail(err)
	}
	status, _, _ := windows.NewLazySystemDLL("ntdll.dll").NewProc("NtResumeProcess").Call(uintptr(h))
	if status != 0 {
		return fail(fmt.Errorf("resume failed: %#x", status))
	}
	return nil
}
func (d *nativeRuntimeDomain) Active() (uint32, error) {
	var info runtimeJobAccounting
	err := windows.QueryInformationJobObject(d.job, windows.JobObjectBasicAccountingInformation, uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info)), nil)
	return info.Active, err
}
func nativeRuntimeHome(t *testing.T, root string, id runtimeIdentity) {
	t.Helper()
	for _, path := range []string{filepath.Join(root, "codex-home"), filepath.Join(root, "codex-home/.tmp"), filepath.Join(root, "codex-home/cache")} {
		if err := os.MkdirAll(path, 0755); err != nil {
			t.Fatal(err)
		}
		sd, err := windows.SecurityDescriptorFromString("D:P(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)(A;OICI;GRGX;;;" + id.SID + ")(A;;0x6;;;" + id.SID + ")(A;OICIIO;FA;;;CO)")
		if err != nil {
			t.Fatal(err)
		}
		acl, _, err := sd.DACL()
		if err != nil {
			t.Fatal(err)
		}
		if err = windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, acl, nil); err != nil {
			t.Fatal(err)
		}
	}
	work := filepath.Join(root, "workspace")
	if err := os.Mkdir(work, 0755); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("icacls.exe", work, "/grant:r", "*"+id.SID+":(OI)(CI)F").CombinedOutput()
	if err != nil {
		t.Fatalf("task workspace ACL: %v %s", err, out)
	}
}
func nativeRuntimeDetach(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_NEW_PROCESS_GROUP | windows.DETACHED_PROCESS}
}
func nativeRuntimeEscapeProbe(t *testing.T) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe, "-test.run=^$")
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_BREAKAWAY_FROM_JOB}
	if err = cmd.Run(); err == nil {
		t.Fatal("task broke out of supervisor Job")
	}
}

func nativeRuntimeFailQuery(d *nativeRuntimeDomain) func() {
	old := d.job
	d.job = windows.InvalidHandle
	return func() { d.job = old }
}
