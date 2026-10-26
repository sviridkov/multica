//go:build darwin || linux

package cachebrokerlab

import (
	"fmt"
	"os"
	"syscall"
)

func nativeVolume(path string) (volumeIdentity, error) {
	st, err := os.Stat(path)
	if err != nil {
		return volumeIdentity{}, err
	}
	s, ok := st.Sys().(*syscall.Stat_t)
	if !ok {
		return volumeIdentity{}, fmt.Errorf("missing volume identity")
	}
	var fs syscall.Statfs_t
	if err = syscall.Statfs(path, &fs); err != nil {
		return volumeIdentity{}, err
	}
	return volumeIdentity{ID: fmt.Sprint(s.Dev), Capacity: int64(fs.Blocks) * int64(fs.Bsize)}, nil
}

func nativeStageEntry(path string, st os.FileInfo, identity string) error {
	s, ok := st.Sys().(*syscall.Stat_t)
	if !ok || s.Uid == 0 || identity != fmt.Sprintf("ruid:%d", s.Uid) || st.Mode().Perm()&0022 != 0 || (!st.IsDir() && (!st.Mode().IsRegular() || s.Nlink != 1)) || st.Mode()&(os.ModeSetuid|os.ModeSetgid) != 0 {
		return fmt.Errorf("unsupported staging owner/type/link: %s", path)
	}
	return noExtraMetadata(path)
}
