package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/daemon/execenv"
)

// Capture file bytes, modes and links, including the index, refs and all other
// Git metadata. Preservation must not rewrite the index to inspect hidden WIP.
func codexCacheSnapshot(t *testing.T, root string) map[string]string {
	t.Helper()
	files := map[string]string{}
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		value := info.Mode().String()
		switch {
		case info.Mode()&os.ModeSymlink != 0:
			target, err := os.Readlink(path)
			if err != nil {
				return err
			}
			value += ":" + target
		case info.Mode().IsRegular():
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			value += fmt.Sprintf(":%x", sha256.Sum256(data))
		}
		files[rel] = value
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

// These cases used to pass the format/Git heuristics and lose local data.
// Exercise both GC routes through their normal ownership, TTL and lock gates.
func TestManagedArtifact_PreservesUnprovenCodexData(t *testing.T) {
	t.Parallel()
	upstream := codexMarketplaceUpstream(t)
	marketplace := func(mutate func(*testing.T, string)) func(*testing.T, string) {
		return func(t *testing.T, taskDir string) {
			mutate(t, installCodexMarketplace(t, taskDir, "alpha", upstream))
		}
	}
	catalog := func(content string) func(*testing.T, string) {
		return func(t *testing.T, taskDir string) {
			writeRemoteCatalog(t, filepath.Join(taskDir, filepath.FromSlash(remoteCatalogRel)), remoteCatalogFile, content)
		}
	}
	cases := []struct {
		name  string
		plant func(*testing.T, string)
	}{
		{"assume unchanged WIP", marketplace(func(t *testing.T, clone string) {
			runGitForGC(t, clone, "update-index", "--assume-unchanged", "plugins/alpha/SKILL.md")
			writeFile(t, filepath.Join(clone, "plugins", "alpha", "SKILL.md"), 9)
		})},
		{"skip worktree WIP", marketplace(func(t *testing.T, clone string) {
			runGitForGC(t, clone, "update-index", "--skip-worktree", "plugins/alpha/SKILL.md")
			writeFile(t, filepath.Join(clone, "plugins", "alpha", "SKILL.md"), 9)
		})},
		{"local annotated tag on upstream commit", marketplace(func(t *testing.T, clone string) {
			runGitForGC(t, clone, "-c", "tag.gpgSign=false", "tag", "-a", "owner-notes", "-m", "unique local annotation", "HEAD")
		})},
		{"packed tag only commit without reflog", marketplace(func(t *testing.T, clone string) {
			runGitForGC(t, clone, "checkout", "--quiet", "--detach")
			writeFile(t, filepath.Join(clone, "local.md"), 17)
			runGitForGC(t, clone, "add", "local.md")
			runGitForGC(t, clone, "-c", "commit.gpgsign=false", "commit", "--quiet", "-m", "local work")
			runGitForGC(t, clone, "tag", "local-only")
			runGitForGC(t, clone, "checkout", "--quiet", "main")
			runGitForGC(t, clone, "reflog", "expire", "--expire=now", "--all")
			runGitForGC(t, clone, "pack-refs", "--all", "--prune")
		})},
		{"unknown Git metadata", marketplace(func(t *testing.T, clone string) {
			writeFile(t, filepath.Join(clone, ".git", "owner-notes.txt"), 19)
		})},
		{"custom Git hook", marketplace(func(t *testing.T, clone string) {
			if err := os.WriteFile(filepath.Join(clone, ".git", "hooks", "pre-commit"), []byte("#!/bin/sh\n# unique local hook\nexit 0\n"), 0o755); err != nil {
				t.Fatal(err)
			}
		})},
		{"credential in Git config", marketplace(func(t *testing.T, clone string) {
			runGitForGC(t, clone, "config", "http.https://example.invalid/.extraheader", "Authorization: Bearer SYNTHETIC_TEST_ONLY")
		})},
		{"unknown plugin credentials and notes", catalog(`{"schema_version":1,"plugins":[{"release":{"access_token":"SYNTHETIC_TEST_ONLY"},"owner_notes":"unique local data"}]}`)},
		{"unsupported catalog version", catalog(`{"schema_version":99,"plugins":[]}`)},
		{"null catalog plugin", catalog(`{"schema_version":1,"plugins":[null]}`)},
	}
	for _, tc := range cases {
		for _, route := range []string{"direct", "walk"} {
			t.Run(tc.name+"/"+route, func(t *testing.T) {
				t.Parallel()
				const id = "eeeeeeee-0000-0000-0000-000000000001"
				mux := chatGCMux(id, "active")
				mux.HandleFunc("/api/daemon/issues/"+id+"/gc-check", func(w http.ResponseWriter, r *http.Request) {
					_ = json.NewEncoder(w).Encode(map[string]any{"status": "in_progress", "updated_at": time.Now()})
				})
				d := newGCTestDaemon(t, mux)
				meta := &execenv.GCMeta{Kind: execenv.GCKindChat, ChatSessionID: id, WorkspaceID: "ws", CompletedAt: time.Now().Add(-24 * time.Hour)}
				wantAction := gcActionCleanManagedArtifacts
				if route == "walk" {
					meta.Kind, meta.IssueID = execenv.GCKindIssue, id
					wantAction = gcActionCleanArtifacts
				}
				taskDir := createTaskDir(t, d.cfg.WorkspacesRoot, "ws", "preserve", meta)
				tc.plant(t, taskDir)
				root := filepath.Join(taskDir, "codex-home")
				before := codexCacheSnapshot(t, root)
				action := d.shouldCleanTaskDir(context.Background(), taskDir)
				if action != wantAction {
					t.Fatalf("action = %d, want %d", action, wantAction)
				}
				stats := &gcStats{byPattern: map[string]int{}}
				d.applyGCAction(context.Background(), taskDir, action, stats)
				if stats.artifactRemoved != 0 || stats.bytesReclaimed != 0 {
					t.Errorf("unproven local data removed: %+v", stats)
				}
				if after := codexCacheSnapshot(t, root); !reflect.DeepEqual(before, after) {
					t.Fatal("GC changed cache contents, file modes, links or Git metadata")
				}
			})
		}
	}
}

func TestManagedArtifact_EmptyCachePopulatedBeforeRemoval(t *testing.T) {
	t.Parallel()
	for _, rel := range []string{marketplacesRel, remoteCatalogRel} {
		t.Run(rel, func(t *testing.T) {
			dir := t.TempDir()
			if reason := managedArtifactKeepReason(context.Background(), rel, dir); reason != "" {
				t.Fatalf("empty cache kept: %s", reason)
			}
			// A writer arrives after inspection. The removal boundary must
			// refuse the now-populated directory without touching its data.
			writeFile(t, filepath.Join(dir, "new-content"), 23)
			before := codexCacheSnapshot(t, dir)
			if err := removeManagedArtifact(rel, dir); err == nil {
				t.Fatal("removal accepted content written after inspection")
			}
			if after := codexCacheSnapshot(t, dir); !reflect.DeepEqual(before, after) {
				t.Fatal("removal changed new cache contents")
			}
		})
	}
}

func TestManagedArtifact_PluginCacheInspectionFailsClosed(t *testing.T) {
	t.Parallel()
	for _, rel := range []string{marketplacesRel, remoteCatalogRel} {
		t.Run(rel, func(t *testing.T) {
			dir := t.TempDir()
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			if reason := managedArtifactKeepReason(ctx, rel, dir); reason == "" {
				t.Fatal("cancelled inspection accepted empty cache")
			}
			if reason := managedArtifactKeepReason(context.Background(), rel, filepath.Join(dir, "missing")); reason == "" {
				t.Fatal("failed read accepted cache")
			}
		})
	}
}
