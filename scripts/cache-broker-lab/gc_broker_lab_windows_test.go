//go:build cachebrokerlab && windows

package daemon

import (
	"fmt"
	"golang.org/x/sys/windows"
	"os"
	"os/exec"
	"syscall"
	"testing"
	"unsafe"
)

func nativePermissionAttack(path string) error {
	// A null DACL would allow all principals; the unprivileged child must be denied WRITE_DAC.
	return windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION, nil, nil, nil, nil)
}

func nativeExtraMetadata(path string) error {
	return os.WriteFile(path+":cache-lab", []byte("preserve this stream"), 0600)
}

func nativeDirectoryLink(link, target string) error {
	out, err := exec.Command("cmd.exe", "/c", "mklink", "/J", link, target).CombinedOutput()
	if err != nil {
		return fmt.Errorf("create fixture junction: %w: %s", err, out)
	}
	return nil
}

func nativeWidenPermissions(path string) error {
	out, err := exec.Command("icacls.exe", path, "/grant", "*S-1-5-32-545:W").CombinedOutput()
	if err != nil {
		return fmt.Errorf("widen fixture DACL: %w: %s", err, out)
	}
	return nil
}

func nativePrivileged() bool {
	u, e := windows.GetCurrentProcessToken().GetTokenUser()
	return e != nil || windows.GetCurrentProcessToken().IsElevated() || u.User.Sid.IsWellKnown(windows.WinLocalSystemSid)
}
func nativeIdentity() string {
	u, e := windows.GetCurrentProcessToken().GetTokenUser()
	if e != nil {
		return e.Error()
	}
	return u.User.Sid.String()
}
func nativeBaseEnv() []string {
	return []string{"SystemRoot=" + os.Getenv("SystemRoot"), "WINDIR=" + os.Getenv("SystemRoot"), "PATH=" + os.Getenv("SystemRoot") + "\\System32"}
}
func nativeAttackCommand(t *testing.T, cfg nativeLab, root, role string) *exec.Cmd {
	t.Helper()
	u, _ := windows.UTF16PtrFromString(cfg.TaskUser)
	domain, _ := windows.UTF16PtrFromString(".")
	pw, _ := windows.UTF16PtrFromString(cfg.TaskPassword)
	var token windows.Token
	ok, _, err := windows.NewLazySystemDLL("advapi32.dll").NewProc("LogonUserW").Call(uintptr(unsafe.Pointer(u)), uintptr(unsafe.Pointer(domain)), uintptr(unsafe.Pointer(pw)), 2, 0, uintptr(unsafe.Pointer(&token)))
	if ok == 0 {
		t.Fatal(err)
	}
	t.Cleanup(func() { token.Close() })
	exe, e := os.Executable()
	if e != nil {
		t.Fatal(e)
	}
	cmd := exec.Command(exe, "-test.run=^TestBrokerAttackChild$", "-test.v")
	cmd.SysProcAttr = &syscall.SysProcAttr{Token: syscall.Token(token)}
	cmd.Dir = root
	cmd.Env = append(nativeBaseEnv(), "MULTICA_BROKER_ATTACK_ROOT="+root, "MULTICA_BROKER_PRODUCER="+cfg.Producer, "MULTICA_BROKER_ATTACK_ROLE="+role)
	return cmd
}
