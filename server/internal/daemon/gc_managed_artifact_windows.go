//go:build windows

package daemon

import (
	"errors"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"
)

func removeEmptyManagedDirectory(path string) error {
	handle, err := openManagedDirectoryForRemoval(path)
	if err != nil {
		return &os.PathError{Op: "rmdir", Path: path, Err: err}
	}
	// FILE_DISPOSITION_INFO contains a single BOOLEAN byte. Windows rejects
	// nonempty directories; no pathname-based fallback may unlink a new leaf.
	deleteFile := byte(1)
	err = windows.SetFileInformationByHandle(handle, windows.FileDispositionInfo, &deleteFile, 1)
	err = errors.Join(err, windows.CloseHandle(handle))
	if err != nil {
		return &os.PathError{Op: "rmdir", Path: path, Err: err}
	}
	return nil
}

// The caller owns the returned handle. Opening the reparse point itself and
// denying sharing keeps the inspected leaf fixed until deletion or close.
func openManagedDirectoryForRemoval(path string) (windows.Handle, error) {
	path, err := filepath.Abs(path)
	if err != nil {
		return windows.InvalidHandle, err
	}
	// The Win32 call needs extended-length paths even where Go's os helpers
	// already handle long task roots. This does not resolve reparse points.
	if !strings.HasPrefix(path, extendedLengthPrefix) {
		if strings.HasPrefix(path, `\\`) {
			path = extendedLengthUNCPrefix + path[2:]
		} else {
			path = extendedLengthPrefix + path
		}
	}
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return windows.InvalidHandle, err
	}
	handle, err := windows.CreateFile(name, windows.DELETE|windows.FILE_READ_ATTRIBUTES, 0, nil,
		windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return windows.InvalidHandle, err
	}
	var info windows.ByHandleFileInformation
	err = windows.GetFileInformationByHandle(handle, &info)
	if err == nil && info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		err = errors.New("refusing to remove a reparse point")
	}
	if err == nil && info.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY == 0 {
		err = windows.ERROR_DIRECTORY
	}
	if err != nil {
		return windows.InvalidHandle, errors.Join(err, windows.CloseHandle(handle))
	}
	return handle, nil
}
