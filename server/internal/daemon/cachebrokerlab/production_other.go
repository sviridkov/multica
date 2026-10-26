//go:build !darwin && !linux && !windows

package cachebrokerlab

import (
	"errors"
	"os"
)

func renameNewTree(string, string) error { return errors.New("unsupported atomic publication") }

func nativeVolume(string) (volumeIdentity, error) {
	return volumeIdentity{}, errors.New("unsupported staging volume")
}
func nativeStageEntry(string, os.FileInfo, string) error {
	return errors.New("unsupported staging identity")
}
