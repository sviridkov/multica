package cachebrokerlab

import (
	"fmt"
	"golang.org/x/sys/windows"
)

func newNativeStorageProbe() storageProbe { return nativeStorageVolume }

func nativeStorageVolume(path string) (storageReading, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return storageReading{}, err
	}
	var root, name [windows.MAX_PATH + 1]uint16
	if err = windows.GetVolumePathName(p, &root[0], uint32(len(root))); err != nil {
		return storageReading{}, err
	}
	if err = windows.GetVolumeNameForVolumeMountPoint(&root[0], &name[0], uint32(len(name))); err != nil {
		return storageReading{}, err
	}
	var serial, component, flags uint32
	if err = windows.GetVolumeInformation(&root[0], nil, 0, &serial, &component, &flags, nil, 0); err != nil {
		return storageReading{}, err
	}
	var available, total, free uint64
	if err = windows.GetDiskFreeSpaceEx(&root[0], &available, &total, &free); err != nil {
		return storageReading{}, err
	}
	return storageReading{Identity: fmt.Sprintf("%s:%d", windows.UTF16ToString(name[:]), serial), Capacity: int64(total), Available: int64(available)}, nil
}
