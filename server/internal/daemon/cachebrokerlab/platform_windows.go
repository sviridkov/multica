package cachebrokerlab

import (
	"fmt"
	"golang.org/x/sys/windows"
	"os"
	"path/filepath"
	"strings"
	"unsafe"
)

const fileDeleteChild windows.ACCESS_MASK = 0x40

func protectedPath(path string) error {
	token := windows.GetCurrentProcessToken()
	user, err := token.GetTokenUser()
	if err != nil {
		return err
	}
	if !user.User.Sid.IsWellKnown(windows.WinLocalSystemSid) {
		return fmt.Errorf("isolated LocalSystem broker identity required")
	}
	for current := filepath.Clean(path); ; current = filepath.Dir(current) {
		st, err := os.Lstat(current)
		if err != nil {
			return err
		}
		if st.Mode()&(os.ModeSymlink|os.ModeIrregular) != 0 {
			return fmt.Errorf("linked protected component: %s", current)
		}
		if err := protectedACLForPath(current, current == filepath.Dir(current)); err != nil {
			return err
		}
		if current == filepath.Dir(current) {
			break
		}
	}
	return nil
}
func protectedACL(path string) error { return protectedACLForPath(path, false) }
func protectedACLForPath(path string, volumeRoot bool) error {
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return err
	}
	owner, _, err := sd.Owner()
	if err != nil {
		return err
	}
	if !trustedSID(owner) {
		return fmt.Errorf("unexpected owner: %s", path)
	}
	acl, _, err := sd.DACL()
	if err != nil {
		return err
	}
	if acl == nil {
		return fmt.Errorf("null DACL: %s", path)
	}
	for i := uint32(0); i < uint32(acl.AceCount); i++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(acl, i, &ace); err != nil {
			return err
		}
		if ace.Header.AceFlags&windows.INHERIT_ONLY_ACE != 0 {
			continue
		}
		if ace.Header.AceType == windows.ACCESS_DENIED_ACE_TYPE {
			continue
		}
		if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE {
			return fmt.Errorf("unsupported ACE: %s", path)
		}
		sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		if trustedSID(sid) {
			continue
		}
		mutating := windows.ACCESS_MASK(windows.GENERIC_ALL | windows.GENERIC_WRITE | windows.DELETE | windows.WRITE_DAC | windows.WRITE_OWNER | windows.FILE_WRITE_DATA | windows.FILE_APPEND_DATA | windows.FILE_WRITE_EA | windows.FILE_WRITE_ATTRIBUTES | fileDeleteChild)
		if volumeRoot {
			mutating = windows.GENERIC_ALL | windows.GENERIC_WRITE | windows.DELETE | windows.WRITE_DAC | windows.WRITE_OWNER | fileDeleteChild
		}
		if ace.Mask&mutating != 0 {
			return fmt.Errorf("untrusted mutating ACE: %s", path)
		}
	}
	return nil
}

func trustedSID(sid *windows.SID) bool {
	return sid.IsWellKnown(windows.WinLocalSystemSid) || sid.IsWellKnown(windows.WinBuiltinAdministratorsSid) || sid.String() == "S-1-5-80-956008885-3418522649-1831038044-1853292631-2271478464" // TrustedInstaller
}

type streamData struct {
	Size int64
	Name [296]uint16
}

func noStreams(path string) error {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	k := windows.NewLazySystemDLL("kernel32.dll")
	var data streamData
	h, _, e := k.NewProc("FindFirstStreamW").Call(uintptr(unsafe.Pointer(p)), 0, uintptr(unsafe.Pointer(&data)), 0)
	if h == ^uintptr(0) {
		if e == windows.ERROR_HANDLE_EOF {
			return nil
		}
		return e
	}
	defer windows.FindClose(windows.Handle(h))
	for {
		if windows.UTF16ToString(data.Name[:]) != "::$DATA" {
			return fmt.Errorf("alternate data stream: %s", path)
		}
		ok, _, e := k.NewProc("FindNextStreamW").Call(h, uintptr(unsafe.Pointer(&data)))
		if ok == 0 {
			if e == windows.ERROR_HANDLE_EOF {
				return nil
			}
			return e
		}
	}
}
func entryMetadata(path string, info os.FileInfo) (string, error) {
	if err := protectedACL(path); err != nil {
		return "", err
	}
	if err := noStreams(path); err != nil {
		return "", err
	}
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return "", err
	}
	h, err := windows.CreateFile(p, windows.FILE_READ_ATTRIBUTES, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil, windows.OPEN_EXISTING, windows.FILE_FLAG_OPEN_REPARSE_POINT|windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		return "", err
	}
	defer windows.CloseHandle(h)
	var id windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(h, &id); err != nil {
		return "", err
	}
	if id.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 || (!info.IsDir() && id.NumberOfLinks != 1) {
		return "", fmt.Errorf("reparse point or hardlink: %s", path)
	}
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.GROUP_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%d:%d:%d:%d:%d:%d:%s", id.VolumeSerialNumber, id.FileIndexHigh, id.FileIndexLow, id.NumberOfLinks, id.FileAttributes, info.ModTime().UnixNano(), sd.String()), nil
}
func replaceJournal(root string) error {
	from, err := windows.UTF16PtrFromString(filepath.Join(root, "journal.new"))
	if err != nil {
		return err
	}
	to, err := windows.UTF16PtrFromString(filepath.Join(root, "journal.json"))
	if err != nil {
		return err
	}
	return windows.MoveFileEx(from, to, windows.MOVEFILE_REPLACE_EXISTING|windows.MOVEFILE_WRITE_THROUGH)
}
func producerEnvironment(p Policy, kind string) []string {
	system := os.Getenv("SystemRoot")
	return []string{"SystemRoot=" + system, "WINDIR=" + system, "PATH=" + filepath.Dir(p.Git) + ";" + filepath.Join(system, "System32"), "TEMP=" + filepath.Join(p.Root, "tmp"), "TMP=" + filepath.Join(p.Root, "tmp"), "USERPROFILE=" + p.Root, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=" + filepath.Join(p.Root, "empty.gitconfig"), "GIT_TERMINAL_PROMPT=0", "GIT_CONFIG_COUNT=3", "GIT_CONFIG_KEY_0=url.file:///" + strings.TrimPrefix(filepath.ToSlash(p.Source), "/") + ".insteadOf", "GIT_CONFIG_VALUE_0=https://cache-lab.invalid/fixture.git", "GIT_CONFIG_KEY_1=protocol.file.allow", "GIT_CONFIG_VALUE_1=always", "GIT_CONFIG_KEY_2=core.hooksPath", "GIT_CONFIG_VALUE_2=" + filepath.Join(p.Root, "no-hooks"), "MULTICA_CACHE_LAB_ALLOW=1", "MULTICA_CACHE_LAB_HOME=" + filepath.Join(p.Root, "codex-home"), "MULTICA_CACHE_LAB_KIND=" + kind, "MULTICA_CACHE_LAB_OID=" + p.SourceOID}
}

func publishReadable(string) error { return nil }
