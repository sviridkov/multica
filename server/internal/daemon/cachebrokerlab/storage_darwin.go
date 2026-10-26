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
	return newNativeStorageProbe()(path)
}

func newNativeStorageProbe() storageProbe {
	return darwinStorageProbe(nativeVolumeUUID)
}

func darwinStorageProbe(readUUID func(string) (string, error)) storageProbe {
	type mount struct {
		device string
		id     syscall.Fsid
	}
	// This map lives for one StorageLayout.check, never across mutations.
	// Every path still gets fresh statfs membership/capacity/free-space reads.
	identities := map[mount]string{}
	return func(path string) (storageReading, error) {
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
		key := mount{device: string(device), id: fs.Fsid}
		identity, found := identities[key]
		if !found {
			var err error
			identity, err = readUUID(key.device)
			if err != nil {
				return storageReading{}, err
			}
		}
		var after syscall.Statfs_t
		if err := syscall.Statfs(path, &after); err != nil {
			return storageReading{}, err
		}
		if after.Fsid != fs.Fsid || after.Mntfromname != fs.Mntfromname || after.Blocks != fs.Blocks || after.Bsize != fs.Bsize {
			return storageReading{}, errors.New("filesystem changed during identity query")
		}
		identities[key] = identity
		return storageReading{Identity: identity, Capacity: int64(after.Blocks) * int64(after.Bsize), Available: int64(after.Bavail) * int64(after.Bsize)}, nil
	}
}

func nativeVolumeUUID(device string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	data, err := exec.CommandContext(ctx, "/usr/sbin/diskutil", "info", "-plist", device).Output()
	if err != nil {
		return "", err
	}
	cmd := exec.CommandContext(ctx, "/usr/bin/plutil", "-extract", "VolumeUUID", "raw", "-o", "-", "-")
	cmd.Stdin = bytes.NewReader(data)
	id, err := cmd.Output()
	if err != nil {
		return "", err
	}
	identity := strings.TrimSpace(string(id))
	if len(identity) != 36 {
		return "", errors.New("missing persistent filesystem UUID")
	}
	return identity, nil
}
