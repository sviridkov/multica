package cachebrokerlab

import (
	"bytes"
	"context"
	"errors"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

func nativeStorageVolume(path string) (storageReading, error) {
	var fs syscall.Statfs_t
	if err := syscall.Statfs(path, &fs); err != nil {
		return storageReading{}, err
	}
	// diskutil accepts a device or mount root, not a directory inside it.
	// Resolve the actual containing filesystem, including nested mounts.
	device := make([]byte, 0, len(fs.Mntfromname))
	for _, c := range fs.Mntfromname {
		if c == 0 {
			break
		}
		device = append(device, byte(c))
	}
	if !strings.HasPrefix(string(device), "/dev/") {
		return storageReading{}, errors.New("storage path is not on a native disk")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	data, err := exec.CommandContext(ctx, "/usr/sbin/diskutil", "info", "-plist", string(device)).Output()
	if err != nil {
		return storageReading{}, err
	}
	cmd := exec.CommandContext(ctx, "/usr/bin/plutil", "-extract", "VolumeUUID", "raw", "-o", "-", "-")
	cmd.Stdin = bytes.NewReader(data)
	id, err := cmd.Output()
	if err != nil {
		return storageReading{}, err
	}
	identity := strings.TrimSpace(string(id))
	if len(identity) != 36 {
		return storageReading{}, errors.New("missing persistent filesystem UUID")
	}
	var after syscall.Statfs_t
	if err = syscall.Statfs(path, &after); err != nil {
		return storageReading{}, err
	}
	if after.Fsid != fs.Fsid || after.Mntfromname != fs.Mntfromname || after.Blocks != fs.Blocks || after.Bsize != fs.Bsize {
		return storageReading{}, errors.New("filesystem changed during identity query")
	}
	return storageReading{Identity: identity, Capacity: int64(after.Blocks) * int64(after.Bsize), Available: int64(after.Bavail) * int64(after.Bsize)}, nil
}
