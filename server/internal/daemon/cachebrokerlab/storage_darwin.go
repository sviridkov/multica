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
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	data, err := exec.CommandContext(ctx, "/usr/sbin/diskutil", "info", "-plist", path).Output()
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
	var fs syscall.Statfs_t
	if err = syscall.Statfs(path, &fs); err != nil {
		return storageReading{}, err
	}
	return storageReading{Identity: identity, Capacity: int64(fs.Blocks) * int64(fs.Bsize), Available: int64(fs.Bavail) * int64(fs.Bsize)}, nil
}
