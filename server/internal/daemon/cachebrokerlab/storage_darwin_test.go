package cachebrokerlab

import (
	"os"
	"path/filepath"
	"testing"
)

func TestNativeStorageVolumeAcceptsDescendantPath(t *testing.T) {
	root := t.TempDir()
	child := filepath.Join(root, "nested", "slot")
	if err := os.MkdirAll(child, 0700); err != nil {
		t.Fatal(err)
	}
	reading, err := nativeStorageVolume(child)
	if err != nil {
		t.Fatal("cannot resolve a directory inside its mounted filesystem:", err)
	}
	parent, err := nativeStorageVolume(root)
	if err != nil {
		t.Fatal(err)
	}
	if reading.Identity == "" || reading.Identity != parent.Identity || reading.Capacity <= 0 || reading.Capacity != parent.Capacity {
		t.Fatal("descendant has inconsistent filesystem identity/capacity", reading, parent)
	}
	if _, err := nativeStorageVolume(filepath.Join(child, "absent")); err == nil {
		t.Fatal("missing path acquired storage authority")
	}
}
