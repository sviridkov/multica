package daemon

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"testing"

	"github.com/multica-ai/multica/server/internal/daemon/execenv"
)

// Replace the leaf after the route's last path validation, then invoke the real
// removal operation, which must enforce its own type and emptiness contract.
func testManagedArtifactRootReplacement(t *testing.T, replace func(*testing.T, string)) {
	t.Helper()
	for _, rel := range []string{marketplacesRel, remoteCatalogRel} {
		for _, route := range []string{"direct", "walk"} {
			t.Run(rel+"/"+route, func(t *testing.T) {
				d := newGCTestDaemon(t, http.NewServeMux())
				taskDir := t.TempDir()
				target := filepath.Join(taskDir, filepath.FromSlash(rel))
				if err := os.MkdirAll(target, 0o755); err != nil {
					t.Fatal(err)
				}
				calls := 0
				remove := func(gotRel, path string) error {
					calls++
					if filepath.ToSlash(gotRel) != rel || path != target {
						t.Fatalf("unexpected removal: %q %q", gotRel, path)
					}
					if err := os.Rename(path, path+".original"); err != nil {
						t.Fatal(err)
					}
					replace(t, path)
					before := codexCacheSnapshot(t, taskDir)
					err := removeManagedArtifact(gotRel, path)
					if err == nil {
						t.Error("removal accepted a substituted root")
					}
					if after := codexCacheSnapshot(t, taskDir); !reflect.DeepEqual(before, after) {
						t.Error("removal changed the substituted root or its contents")
					}
					return err
				}
				var removed int
				var reclaimed int64
				var patterns map[string]int
				if route == "direct" {
					removed, reclaimed, patterns = d.cleanManagedTaskArtifactsWithRemoval(context.Background(), taskDir, remove)
				} else {
					matcher := newArtifactMatcher(nil, execenv.ManagedReclaimableArtifactSubpaths())
					removed, reclaimed, patterns = d.cleanTaskArtifactsMatchingWithRemoval(context.Background(), taskDir, matcher, remove)
				}
				if calls != 1 {
					t.Fatalf("removal calls = %d, want 1", calls)
				}
				if removed != 0 || reclaimed != 0 || len(patterns) != 0 {
					t.Errorf("reported successful removal: removed=%d bytes=%d patterns=%v", removed, reclaimed, patterns)
				}
			})
		}
	}
}

func TestManagedArtifact_RootReplacedByFile(t *testing.T) {
	t.Parallel()
	testManagedArtifactRootReplacement(t, func(t *testing.T, path string) {
		if err := os.WriteFile(path, []byte("unique user content written after validation"), 0o600); err != nil {
			t.Fatal(err)
		}
	})
}

func TestManagedArtifact_RootReplacedByNonemptyDirectory(t *testing.T) {
	t.Parallel()
	testManagedArtifactRootReplacement(t, func(t *testing.T, path string) {
		writeFile(t, filepath.Join(path, "owner-notes"), 23)
	})
}

func TestManagedArtifact_RootReplacedBySymlink(t *testing.T) {
	t.Parallel()
	testManagedArtifactRootReplacement(t, func(t *testing.T, path string) {
		outside := t.TempDir()
		writeFile(t, filepath.Join(outside, "owner-notes"), 31)
		before := codexCacheSnapshot(t, outside)
		if err := os.Symlink(outside, path); err != nil {
			if runtime.GOOS == "windows" {
				t.Skipf("symlink unavailable; junction coverage is mandatory: %v", err)
			}
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if after := codexCacheSnapshot(t, outside); !reflect.DeepEqual(before, after) {
				t.Error("removal changed symlink target")
			}
		})
	})
}

func TestTaskSize_ExcludesRetainedCodexPluginCaches(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		patterns []string
	}{
		{"exact paths", nil},
		{"overlapping basenames", []string{"marketplaces", "remote_plugin_catalog"}},
	} {
		for _, route := range []string{"direct", "walk"} {
			t.Run(route+"/"+tc.name, func(t *testing.T) {
				d := newGCTestDaemon(t, http.NewServeMux())
				taskDir := t.TempDir()
				writeFile(t, filepath.Join(taskDir, filepath.FromSlash(marketplacesRel), "owner-notes"), 100)
				writeFile(t, filepath.Join(taskDir, filepath.FromSlash(remoteCatalogRel), "catalog.json"), 200)
				writeFile(t, filepath.Join(taskDir, sandboxBinRel, "codex"), 40)
				matcher := newArtifactMatcher(tc.patterns, execenv.ManagedReclaimableArtifactSubpaths())
				total, artifacts := taskSize(taskDir, matcher)
				if total != 340 || artifacts != 40 {
					t.Errorf("size=(%d,%d), want (340,40): retained plugin bytes are not reclaimable", total, artifacts)
				}
				var removed int
				var reclaimed int64
				if route == "direct" {
					removed, reclaimed, _ = d.cleanManagedTaskArtifacts(context.Background(), taskDir)
				} else {
					removed, reclaimed, _ = d.cleanTaskArtifactsMatching(context.Background(), taskDir, matcher)
				}
				if removed != 1 || reclaimed != artifacts {
					t.Errorf("removed=%d reclaimed=%d, want 1 and reported bytes=%d", removed, reclaimed, artifacts)
				}
				total, artifacts = taskSize(taskDir, matcher)
				if total != 300 || artifacts != 0 {
					t.Errorf("after cleanup size=(%d,%d), want (300,0)", total, artifacts)
				}
			})
		}
	}
}
