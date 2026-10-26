package cachebrokerlab

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

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
