//go:build cachebrokerlab && windows

package main

import (
	"errors"
	"fmt"
	"os/exec"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

type windowsAcquisition struct {
	job   windows.Handle
	token windows.Token
}
type acquisitionAccounting struct {
	User, Kernel, PeriodUser, PeriodKernel int64
	Faults, Total, Active, Terminated      uint32
}

func newAcquisitionDomain(_ config, slot acquisitionSlot, _ string) (acquisitionDomain, string, error) {
	u, err := windows.UTF16PtrFromString(slot.Identity.User)
	if err != nil {
		return nil, "", err
	}
	domain, err := windows.UTF16PtrFromString(".")
	if err != nil {
		return nil, "", err
	}
	pw, err := windows.UTF16PtrFromString(slot.Identity.Password)
	if err != nil {
		return nil, "", err
	}
	var token windows.Token
	ok, _, callErr := windows.NewLazySystemDLL("advapi32.dll").NewProc("LogonUserW").Call(uintptr(unsafe.Pointer(u)), uintptr(unsafe.Pointer(domain)), uintptr(unsafe.Pointer(pw)), 2, 0, uintptr(unsafe.Pointer(&token)))
	if ok == 0 {
		return nil, "", fmt.Errorf("ordinary producer logon: %w", callErr)
	}
	d := &windowsAcquisition{token: token}
	fail := func(err error) (acquisitionDomain, string, error) { return nil, "", errors.Join(err, d.Close()) }
	user, err := token.GetTokenUser()
	if err != nil {
		return fail(err)
	}
	if slot.Identity.SID == "" || user.User.Sid.String() != slot.Identity.SID || token.IsElevated() {
		return fail(errors.New("unexpected ordinary acquisition token"))
	}
	d.job, err = windows.CreateJobObject(nil, nil)
	if err != nil {
		return fail(err)
	}
	limits := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	limits.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err = windows.SetInformationJobObject(d.job, windows.JobObjectExtendedLimitInformation, uintptr(unsafe.Pointer(&limits)), uint32(unsafe.Sizeof(limits))); err != nil {
		return fail(err)
	}
	if err = windows.SetHandleInformation(d.job, windows.HANDLE_FLAG_INHERIT, 0); err != nil {
		return fail(err)
	}
	return d, slot.Identity.SID, nil
}
func (d *windowsAcquisition) Start(cmd *exec.Cmd) error {
	if err := acquisitionCommand(cmd); err != nil {
		return err
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Token: syscall.Token(d.token), CreationFlags: windows.CREATE_SUSPENDED, HideWindow: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	fail := func(err error) error { return errors.Join(err, cmd.Process.Kill(), cmd.Wait()) }
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
func (d *windowsAcquisition) Active() (uint32, error) {
	var info acquisitionAccounting
	err := windows.QueryInformationJobObject(d.job, windows.JobObjectBasicAccountingInformation, uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info)), nil)
	return info.Active, err
}
func (d *windowsAcquisition) Close() error {
	var err error
	if d.job != 0 {
		err = windows.CloseHandle(d.job)
		d.job = 0
	}
	if d.token != 0 {
		err = errors.Join(err, d.token.Close())
		d.token = 0
	}
	return err
}
func ordinaryAcquisitionIdentity() (string, error) {
	token := windows.GetCurrentProcessToken()
	user, err := token.GetTokenUser()
	if err != nil {
		return "", err
	}
	if token.IsElevated() || user.User.Sid.String() == "S-1-5-18" {
		return "", errors.New("acquisition launcher must be ordinary")
	}
	return user.User.Sid.String(), nil
}
