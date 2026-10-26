package cachebrokerlab

import (
	"errors"
	"fmt"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
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
		// Identify the containing filesystem, including nested mounts.
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
			identity, err = readUUID(path)
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

func nativeVolumeUUID(path string) (string, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return "", err
	}
	defer unix.Close(fd)
	attrs := unix.Attrlist{Bitmapcount: unix.ATTR_BIT_MAP_COUNT, Volattr: unix.ATTR_VOL_INFO | unix.ATTR_VOL_UUID}
	// fgetattrlist returns a packed uint32 length followed by uuid_t (16 bytes).
	var result struct {
		Length uint32
		UUID   [16]byte
	}
	_, _, errno := syscall.Syscall6(unix.SYS_FGETATTRLIST, uintptr(fd), uintptr(unsafe.Pointer(&attrs)), uintptr(unsafe.Pointer(&result)), unsafe.Sizeof(result), 0, 0)
	if errno != 0 {
		return "", errno
	}
	if result.Length != uint32(unsafe.Sizeof(result)) || result.UUID == [16]byte{} {
		return "", errors.New("missing persistent filesystem UUID")
	}
	id := result.UUID
	return fmt.Sprintf("%X-%X-%X-%X-%X", id[:4], id[4:6], id[6:8], id[8:10], id[10:]), nil
}
