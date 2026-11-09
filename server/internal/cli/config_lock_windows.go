package cli

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/windows"
)

func lockCLIConfig(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, fmt.Errorf("open CLI config lock: %w", err)
	}
	if err = windows.LockFileEx(windows.Handle(f.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, &windows.Overlapped{}); err != nil {
		f.Close()
		if errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
			return nil, ErrCLIConfigConflict
		}
		return nil, fmt.Errorf("lock CLI config: %w", err)
	}
	return f, nil
}
