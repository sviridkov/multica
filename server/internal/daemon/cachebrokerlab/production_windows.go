package cachebrokerlab

import (
	"fmt"
	"golang.org/x/sys/windows"
	"os"
	"unsafe"
)

func renameNewTree(from, to string) error {
	a, err := windows.UTF16PtrFromString(from)
	if err != nil {
		return err
	}
	b, err := windows.UTF16PtrFromString(to)
	if err != nil {
		return err
	}
	return windows.MoveFileEx(a, b, windows.MOVEFILE_WRITE_THROUGH)
}

func nativeVolume(path string) (volumeIdentity, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return volumeIdentity{}, err
	}
	var volume [windows.MAX_PATH + 1]uint16
	if err = windows.GetVolumePathName(p, &volume[0], uint32(len(volume))); err != nil {
		return volumeIdentity{}, err
	}
	var total, free, available uint64
	if err = windows.GetDiskFreeSpaceEx(&volume[0], &available, &total, &free); err != nil {
		return volumeIdentity{}, err
	}
	var serial, component, flags uint32
	if err = windows.GetVolumeInformation(&volume[0], nil, 0, &serial, &component, &flags, nil, 0); err != nil {
		return volumeIdentity{}, err
	}
	return volumeIdentity{ID: fmt.Sprint(serial), Capacity: int64(total)}, nil
}

func nativeStageEntry(path string, st os.FileInfo, identity string) error {
	if !st.IsDir() && !st.Mode().IsRegular() {
		return fmt.Errorf("unsupported staging type")
	}
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return err
	}
	owner, _, err := sd.Owner()
	if err != nil {
		return err
	}
	if trustedSID(owner) || owner.String() != identity {
		return fmt.Errorf("unexpected staging owner")
	}
	acl, _, err := sd.DACL()
	if err != nil {
		return err
	}
	if acl == nil {
		return fmt.Errorf("null staging DACL")
	}
	for i := uint32(0); i < uint32(acl.AceCount); i++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err = windows.GetAce(acl, i, &ace); err != nil {
			return err
		}
		if ace.Header.AceFlags&windows.INHERIT_ONLY_ACE != 0 || ace.Header.AceType == windows.ACCESS_DENIED_ACE_TYPE {
			continue
		}
		if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE {
			return fmt.Errorf("unsupported staging ACE")
		}
		sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		if sid.String() == identity || trustedSID(sid) {
			continue
		}
		if ace.Mask&windows.ACCESS_MASK(windows.GENERIC_ALL|windows.GENERIC_WRITE|windows.DELETE|windows.WRITE_DAC|windows.WRITE_OWNER|windows.FILE_WRITE_DATA|windows.FILE_APPEND_DATA|windows.FILE_WRITE_EA|windows.FILE_WRITE_ATTRIBUTES|fileDeleteChild) != 0 {
			return fmt.Errorf("foreign staging writer")
		}
	}
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	h, err := windows.CreateFile(p, windows.FILE_READ_ATTRIBUTES, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil, windows.OPEN_EXISTING, windows.FILE_FLAG_OPEN_REPARSE_POINT|windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(h)
	var info windows.ByHandleFileInformation
	if err = windows.GetFileInformationByHandle(h, &info); err != nil {
		return err
	}
	if info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 || (!st.IsDir() && info.NumberOfLinks != 1) {
		return fmt.Errorf("staging reparse/hardlink")
	}
	return noStreams(path)
}
