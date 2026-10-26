//go:build !darwin && !windows

package cachebrokerlab

import "errors"

func nativeStorageVolume(string) (storageReading, error) {
	return storageReading{}, errors.New("quota storage experiment requires native macOS or Windows")
}
