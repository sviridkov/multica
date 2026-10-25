//go:build windows

package daemon

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

func TestManagedArtifact_RootReplacedByJunction(t *testing.T) {
	t.Parallel()
	testManagedArtifactRootReplacement(t, func(t *testing.T, path string) {
		outside := t.TempDir()
		writeFile(t, filepath.Join(outside, "owner-notes"), 31)
		before := codexCacheSnapshot(t, outside)
		createJunction(t, outside, path)
		t.Cleanup(func() {
			if after := codexCacheSnapshot(t, outside); !reflect.DeepEqual(before, after) {
				t.Error("removal changed junction target")
			}
		})
	})
}

func TestManagedArtifact_WindowsDirectoryRemoval(t *testing.T) {
	t.Parallel()
	t.Run("empty directory including long Unicode path", func(t *testing.T) {
		for _, leaf := range []string{"cache", strings.Repeat("long-directory/", 24) + "кэш"} {
			t.Run(leaf, func(t *testing.T) {
				path := filepath.Join(t.TempDir(), filepath.FromSlash(leaf))
				if err := os.MkdirAll(path, 0o755); err != nil {
					t.Fatal(err)
				}
				if err := removeEmptyManagedDirectory(path); err != nil {
					t.Fatal(err)
				}
				if _, err := os.Lstat(path); !os.IsNotExist(err) {
					t.Fatalf("empty directory remains: %v", err)
				}
			})
		}
	})
	t.Run("sharing conflict retains directory", func(t *testing.T) {
		path := t.TempDir()
		name, err := windows.UTF16PtrFromString(path)
		if err != nil {
			t.Fatal(err)
		}
		handle, err := windows.CreateFile(name, windows.GENERIC_READ, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE,
			nil, windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
		if err != nil {
			t.Fatal(err)
		}
		removeErr := removeEmptyManagedDirectory(path)
		closeErr := windows.CloseHandle(handle)
		if removeErr == nil || closeErr != nil {
			t.Fatalf("remove=%v close=%v, want sharing failure and successful close", removeErr, closeErr)
		}
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("busy directory was removed: %v", err)
		}
		if err := removeEmptyManagedDirectory(path); err != nil {
			t.Fatalf("directory still busy after close: %v", err)
		}
	})
	t.Run("held handle prevents replacement", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "cache")
		if err := os.Mkdir(path, 0o755); err != nil {
			t.Fatal(err)
		}
		handle, err := openManagedDirectoryForRemoval(path)
		if err != nil {
			t.Fatal(err)
		}
		renameErr := os.Rename(path, path+".replaced")
		closeErr := windows.CloseHandle(handle)
		if renameErr == nil || closeErr != nil {
			t.Fatalf("rename=%v close=%v, want rename failure and successful close", renameErr, closeErr)
		}
		if err := os.Rename(path, path+".released"); err != nil {
			t.Fatalf("handle was not released: %v", err)
		}
	})
	t.Run("failure releases handle without deferred deletion", func(t *testing.T) {
		for _, kind := range []string{"file", "nonempty directory", "junction"} {
			t.Run(kind, func(t *testing.T) {
				path := filepath.Join(t.TempDir(), "cache")
				switch kind {
				case "file":
					writeFile(t, path, 23)
				case "nonempty directory":
					writeFile(t, filepath.Join(path, "owner-notes"), 23)
				case "junction":
					createJunction(t, t.TempDir(), path)
				}
				if err := removeEmptyManagedDirectory(path); err == nil {
					t.Fatal("removal accepted protected leaf")
				}
				if err := os.Rename(path, path+".released"); err != nil {
					t.Fatalf("failed removal retained a handle or deleted the leaf: %v", err)
				}
			})
		}
	})
}
