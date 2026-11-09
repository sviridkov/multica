//go:build !windows

package daemon

import (
	"golang.org/x/sys/unix"
	"os"
)

// Directory-only removal keeps a file substituted after inspection intact.
func removeGCEmptyDirectory(parent *os.Root, name string) error {
	dir, err := parent.Open(".")
	if err != nil {
		return err
	}
	defer dir.Close()
	return unix.Unlinkat(int(dir.Fd()), name, unix.AT_REMOVEDIR)
}
