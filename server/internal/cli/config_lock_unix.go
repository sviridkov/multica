//go:build !windows

package cli

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// The sidecar is never removed: unlinking it would let two writers lock
// different inodes. Closing the descriptor releases the advisory lock.
func lockCLIConfig(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, fmt.Errorf("open CLI config lock: %w", err)
	}
	if err = unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
			return nil, ErrCLIConfigConflict
		}
		return nil, fmt.Errorf("lock CLI config: %w", err)
	}
	return f, nil
}
