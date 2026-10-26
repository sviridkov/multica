//go:build darwin || linux

package cachebrokerlab

import (
	"errors"
	"golang.org/x/sys/unix"
	"os"
	"path/filepath"
)

func lockService(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

func privateCredential(path string) error {
	if err := protectedPath(path); err != nil {
		return err
	}
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if info.Mode().Perm()&0077 != 0 {
		return errors.New("credential is readable outside broker identity")
	}
	return nil
}

func replaceServiceFile(from, to string) error {
	if err := os.Rename(from, to); err != nil {
		return err
	}
	f, err := os.Open(filepath.Dir(to))
	if err != nil {
		return err
	}
	return errors.Join(f.Sync(), f.Close())
}
