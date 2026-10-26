//go:build darwin || linux

package cachebrokerlab

import (
	"context"
	"fmt"
	"golang.org/x/sys/unix"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPublicationCopyRejectsLinksAndBoundsBytes(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("ordinary-owner unit fixture; privileged paths covered by native suite")
	}
	for _, kind := range []string{"regular", "symlink", "hardlink", "oversize", "unknown-destination"} {
		t.Run(kind, func(t *testing.T) {
			source, dest := t.TempDir(), t.TempDir()
			file := filepath.Join(source, "entry")
			if err := os.WriteFile(file, []byte("12345"), 0600); err != nil {
				t.Fatal(err)
			}
			// The host may stamp provenance on newly created temporary fixtures.
			// Remove only that test-owned metadata; the importer still rejects it.
			for _, path := range []string{source, file} {
				n, err := unix.Listxattr(path, nil)
				if err != nil {
					t.Fatal(err)
				}
				attrs := make([]byte, n)
				if _, err = unix.Listxattr(path, attrs); err != nil {
					t.Fatal(err)
				}
				for _, name := range strings.Split(string(attrs), "\x00") {
					if name != "" {
						if err = unix.Removexattr(path, name); err != nil {
							t.Fatal(err)
						}
					}
				}
				if remaining, e := unix.Listxattr(path, nil); e != nil {
					t.Fatal(e)
				} else if remaining != 0 {
					t.Skip("host retains protected provenance on ordinary fixtures; importer refuses it, positive import requires native VM")
				}
			}
			p := ProductionPlan{Stage: source, LogicalBytes: 5, MaxEntries: 4, Binding: RunBinding{Identity: fmt.Sprintf("ruid:%d", os.Geteuid())}}
			switch kind {
			case "symlink":
				if err := os.Symlink(file, filepath.Join(source, "link")); err != nil {
					t.Fatal(err)
				}
			case "hardlink":
				if err := os.Link(file, filepath.Join(source, "link")); err != nil {
					t.Fatal(err)
				}
			case "oversize":
				p.LogicalBytes = 4
			case "unknown-destination":
				if err := os.WriteFile(filepath.Join(dest, "entry"), []byte("keep"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			err := copyStage(context.Background(), source, dest, p)
			if kind == "regular" {
				if err != nil {
					t.Fatal(err)
				}
				data, e := os.ReadFile(filepath.Join(dest, "entry"))
				if e != nil || string(data) != "12345" {
					t.Fatal("copy differs", e)
				}
			} else if err == nil {
				t.Fatal("unsafe publication accepted")
			}
			if data, e := os.ReadFile(file); e != nil || string(data) != "12345" {
				t.Fatal("source changed", e)
			}
			if kind == "unknown-destination" {
				data, _ := os.ReadFile(filepath.Join(dest, "entry"))
				if string(data) != "keep" {
					t.Fatal("unknown destination overwritten")
				}
			}
		})
	}
}

func TestPublicationRenameDoesNotReplaceExistingEmptyDirectory(t *testing.T) {
	root := t.TempDir()
	source, dest := filepath.Join(root, "source"), filepath.Join(root, "destination")
	for _, p := range []string{source, dest} {
		if err := os.Mkdir(p, 0700); err != nil {
			t.Fatal(err)
		}
	}
	before, err := os.Stat(dest)
	if err != nil {
		t.Fatal(err)
	}
	if renameNewTree(source, dest) == nil {
		t.Fatal("unknown empty directory replaced")
	}
	after, err := os.Stat(dest)
	if err != nil || !os.SameFile(before, after) {
		t.Fatal("destination identity changed")
	}
}
