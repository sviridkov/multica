//go:build !windows

package daemon

import (
	"os"

	"golang.org/x/sys/unix"
)

func removeEmptyManagedDirectory(path string) error {
	// Unlike os.Remove, rmdir never unlinks a regular file or leaf symlink.
	if err := unix.Rmdir(path); err != nil {
		return &os.PathError{Op: "rmdir", Path: path, Err: err}
	}
	return nil
}
