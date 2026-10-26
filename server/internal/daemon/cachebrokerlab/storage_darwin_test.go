package cachebrokerlab

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestNativeVolumeUUIDReadsDescendantMetadata(t *testing.T) {
	root := t.TempDir()
	got, err := nativeVolumeUUID(root)
	if err != nil {
		t.Fatal(err)
	}
	var fs syscall.Statfs_t
	if err := syscall.Statfs(root, &fs); err != nil {
		t.Fatal(err)
	}
	device := make([]byte, 0, len(fs.Mntfromname))
	for _, c := range fs.Mntfromname {
		if c == 0 {
			break
		}
		device = append(device, byte(c))
	}
	info, err := exec.Command("/usr/sbin/diskutil", "info", "-plist", string(device)).Output()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("/usr/bin/plutil", "-extract", "VolumeUUID", "raw", "-o", "-", "-")
	cmd.Stdin = bytes.NewReader(info)
	want, err := cmd.Output()
	if err != nil || got != strings.TrimSpace(string(want)) {
		t.Fatalf("native metadata differs from diskutil: got %q, want %q, error %v", got, want, err)
	}
	if _, err := nativeVolumeUUID(filepath.Join(root, "absent")); err == nil {
		t.Fatal("missing path supplied a UUID")
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(root, link); err != nil {
		t.Fatal(err)
	}
	if _, err := nativeVolumeUUID(link); err == nil {
		t.Fatal("UUID query followed a symlink")
	}
}

func TestNativeStorageProbeQueriesEachMountOncePerCheck(t *testing.T) {
	root := t.TempDir()
	child := filepath.Join(root, "slot")
	if err := os.Mkdir(child, 0700); err != nil {
		t.Fatal(err)
	}
	queries := 0
	readUUID := func(device string) (string, error) {
		queries++
		return nativeVolumeUUID(device)
	}
	probe := darwinStorageProbe(readUUID)
	first, err := probe(root)
	if err != nil {
		t.Fatal(err)
	}
	second, err := probe(child)
	if err != nil {
		t.Fatal(err)
	}
	if first.Identity != second.Identity || queries != 1 {
		t.Fatal("one storage check repeated a native UUID query", queries, first, second)
	}
	if _, err := darwinStorageProbe(readUUID)(child); err != nil {
		t.Fatal(err)
	}
	if queries != 2 {
		t.Fatal("a new check reused old identity authority", queries)
	}
}

func TestNativeStorageProbeDoesNotCacheQueryFailure(t *testing.T) {
	root := t.TempDir()
	calls := 0
	want := errors.New("native UUID query failed")
	probe := darwinStorageProbe(func(string) (string, error) {
		calls++
		if calls == 1 {
			return "", want
		}
		return fmt.Sprintf("verified-query-%d", calls), nil
	})
	if _, err := probe(root); !errors.Is(err, want) {
		t.Fatal(err)
	}
	reading, err := probe(root)
	if err != nil || reading.Identity != "verified-query-2" || calls != 2 {
		t.Fatal("failed query established cached authority", reading, calls, err)
	}
}

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
