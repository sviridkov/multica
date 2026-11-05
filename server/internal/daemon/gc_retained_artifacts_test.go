package daemon

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/daemon/execenv"
)

// A root kept for its result must not pin regenerable artifacts forever when
// the daemon misses the window between the artifact and completed-task TTLs.
func TestRetainedTaskArtifactsAcrossLifecycle(t *testing.T) {
	for _, tc := range []struct {
		name        string
		age         time.Duration
		artifactTTL time.Duration
		terminal    bool
		orphan      bool
		missingMeta bool
		wantAction  gcAction
		wantRemoved int
		wantBytes   int64
	}{
		{"before-completed-ttl", 48 * time.Hour, 12 * time.Hour, false, false, false, gcActionCleanArtifacts, 2, 6144},
		{"after-completed-ttl", 96 * time.Hour, 12 * time.Hour, false, false, false, gcActionClean, 2, 6144},
		{"terminal", 96 * time.Hour, 12 * time.Hour, true, false, false, gcActionClean, 2, 6144},
		{"orphan", 96 * time.Hour, 12 * time.Hour, false, true, false, gcActionOrphan, 1, 4096},
		{"orphan-without-meta", 96 * time.Hour, 12 * time.Hour, false, true, true, gcActionOrphan, 1, 4096},
		{"completed-before-artifact-ttl", 96 * time.Hour, 120 * time.Hour, false, false, false, gcActionClean, 0, 0},
		{"terminal-before-artifact-ttl", 6 * time.Hour, 12 * time.Hour, true, false, false, gcActionClean, 0, 0},
		{"orphan-before-artifact-ttl", 6 * time.Hour, 12 * time.Hour, false, true, false, gcActionOrphan, 0, 0},
		{"artifacts-disabled", 96 * time.Hour, 0, false, false, false, gcActionClean, 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mux := http.NewServeMux()
			mux.HandleFunc("/api/daemon/issues/issue1/gc-check", func(w http.ResponseWriter, r *http.Request) {
				if tc.orphan {
					w.WriteHeader(http.StatusNotFound)
					return
				}
				status := "in_progress"
				if tc.terminal {
					status = "done"
				}
				json.NewEncoder(w).Encode(map[string]any{"status": status, "updated_at": time.Now().Add(-96 * time.Hour)})
			})
			d := newGCTestDaemon(t, mux)
			d.cfg.GCCompletedTaskTTL = 72 * time.Hour
			d.cfg.GCTTL = 24 * time.Hour
			d.cfg.GCOrphanTTL = time.Hour
			d.cfg.GCArtifactTTL = tc.artifactTTL
			meta := &execenv.GCMeta{WorkspaceID: "ws1", TaskID: "task1", IssueID: "issue1", CompletedAt: time.Now().Add(-tc.age)}
			if tc.missingMeta {
				meta = nil
			}
			root := createTaskDir(t, d.cfg.WorkspacesRoot, "ws1", "task1", meta)
			cache := filepath.Join(root, "codex-home", ".sandbox-bin", "codex")
			artifact := filepath.Join(root, "workdir", "node_modules", "dependency")
			result := filepath.Join(root, "output", "result.md")
			writeFile(t, cache, 4096)
			writeFile(t, artifact, 2048)
			writeFile(t, result, 37)
			before, err := os.ReadFile(result)
			if err != nil {
				t.Fatal(err)
			}
			old := time.Now().Add(-tc.age)
			if err := os.Chtimes(root, old, old); err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()
			action := d.shouldCleanTaskDir(ctx, root)
			if action != tc.wantAction {
				t.Fatalf("action=%d, want %d", action, tc.wantAction)
			}
			stats := &gcStats{}
			if n := d.applyGCAction(ctx, root, action, stats); n != 0 || stats.cleaned != 0 || stats.orphaned != 0 || stats.skipped != 1 {
				t.Fatalf("result root reclaimed: n=%d stats=%+v", n, stats)
			}
			if stats.artifactRemoved != tc.wantRemoved || stats.bytesReclaimed != tc.wantBytes {
				t.Errorf("artifacts=%d bytes=%d, want %d/%d", stats.artifactRemoved, stats.bytesReclaimed, tc.wantRemoved, tc.wantBytes)
			}
			for path, removed := range map[string]bool{cache: tc.wantRemoved > 0, artifact: tc.wantRemoved == 2} {
				_, err := os.Stat(path)
				if removed && !os.IsNotExist(err) || !removed && err != nil {
					t.Errorf("%s removed=%v: %v", path, removed, err)
				}
			}
			if after, err := os.ReadFile(result); err != nil || string(after) != string(before) {
				t.Fatalf("result changed: %v", err)
			}
			if tc.wantRemoved > 0 {
				stats = &gcStats{}
				d.applyGCAction(ctx, root, d.shouldCleanTaskDir(ctx, root), stats)
				if stats.artifactRemoved != 0 || stats.bytesReclaimed != 0 {
					t.Fatalf("repeated cleanup counted again: %+v", stats)
				}
			}
		})
	}
}

func TestRetainedTaskArtifactsFailClosed(t *testing.T) {
	for _, problem := range []string{"foreign-owner", "malformed-meta", "foreign-meta", "unknown-meta-kind", "unknown-meta-field", "local-directory", "owner-link", "workdir-link", "cache-link", "root-link", "active-after-decision", "held-lock", "released-lock"} {
		t.Run(problem, func(t *testing.T) {
			d := newGCTestDaemon(t, http.NewServeMux())
			root := createTaskDir(t, d.cfg.WorkspacesRoot, "ws1", "task1", &execenv.GCMeta{WorkspaceID: "ws1", TaskID: "task1", CompletedAt: time.Now().Add(-96 * time.Hour)})
			cache := filepath.Join(root, "codex-home", ".sandbox-bin", "codex")
			writeFile(t, cache, 4096)
			result := filepath.Join(root, "output", "result.md")
			writeFile(t, result, 37)
			switch problem {
			case "foreign-owner":
				if err := os.WriteFile(filepath.Join(root, ".task_owner"), []byte(`{"workspace_id":"ws1","task_id":"foreign"}`), 0600); err != nil {
					t.Fatal(err)
				}
			case "malformed-meta", "foreign-meta", "unknown-meta-kind", "unknown-meta-field", "local-directory":
				body := map[string]string{
					"malformed-meta":     `broken`,
					"foreign-meta":       `{"workspace_id":"foreign","completed_at":"2020-01-01T00:00:00Z"}`,
					"unknown-meta-kind":  `{"workspace_id":"ws1","kind":"future","completed_at":"2020-01-01T00:00:00Z"}`,
					"unknown-meta-field": `{"workspace_id":"ws1","future_result":"keep","completed_at":"2020-01-01T00:00:00Z"}`,
					"local-directory":    `{"workspace_id":"ws1","local_directory":true,"completed_at":"2020-01-01T00:00:00Z"}`,
				}[problem]
				if err := os.WriteFile(filepath.Join(root, ".gc_meta.json"), []byte(body), 0600); err != nil {
					t.Fatal(err)
				}
			case "owner-link", "workdir-link", "cache-link", "root-link":
				target := filepath.Join(t.TempDir(), "target")
				path := root
				switch problem {
				case "owner-link":
					path = filepath.Join(root, ".task_owner")
				case "workdir-link":
					path = filepath.Join(root, "workdir")
					if err := os.Mkdir(path, 0700); err != nil {
						t.Fatal(err)
					}
				case "cache-link":
					path = filepath.Join(root, "codex-home")
					cache = filepath.Join(target, ".sandbox-bin", "codex")
				case "root-link":
					cache = filepath.Join(target, "codex-home", ".sandbox-bin", "codex")
					result = filepath.Join(target, "output", "result.md")
				}
				if err := os.Rename(path, target); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, path); err != nil {
					t.Skipf("symlinks unavailable: %v", err)
				}
			case "active-after-decision":
				d.markActiveEnvRoot(root)
				defer d.unmarkActiveEnvRoot(root)
			case "held-lock", "released-lock":
				base, err := os.OpenRoot(d.cfg.WorkspacesRoot)
				if err != nil {
					t.Fatal(err)
				}
				defer base.Close()
				claim, _, err := execenv.LockEnvRootForReuse(base, filepath.Join("ws1", "task1"), root)
				if err != nil || claim == nil {
					t.Fatalf("claim: %v", err)
				}
				defer claim.Release()
				if problem == "released-lock" {
					claim.Release()
				}
			}
			stats := &gcStats{}
			if n := d.applyGCAction(context.Background(), root, gcActionClean, stats); n != 0 || stats.cleaned != 0 || stats.skipped != 1 {
				t.Fatalf("root changed: n=%d stats=%+v", n, stats)
			}
			if problem == "released-lock" {
				if _, err := os.Stat(cache); !os.IsNotExist(err) || stats.bytesReclaimed != 4096 || stats.artifactRemoved != 1 {
					t.Fatalf("released execution kept eligible cache: %v, %+v", err, stats)
				}
				if _, err := os.Stat(filepath.Join(root, ".task_lock")); err != nil {
					t.Fatalf("persistent lock removed: %v", err)
				}
			} else if info, err := os.Stat(cache); err != nil || info.Size() != 4096 || stats.bytesReclaimed != 0 || stats.artifactRemoved != 0 {
				t.Fatalf("unsafe fallback: cache=%v stats=%+v", err, stats)
			}
			if info, err := os.Stat(result); err != nil || info.Size() != 37 {
				t.Fatalf("result changed: %v", err)
			}
		})
	}
}

func TestRetainedTaskArtifactsRevalidateControlIdentity(t *testing.T) {
	for _, change := range []string{"owner-replaced", "meta-recent", "meta-created", "root-replaced"} {
		t.Run(change, func(t *testing.T) {
			d := newGCTestDaemon(t, http.NewServeMux())
			d.cfg.GCOrphanTTL = time.Hour
			meta := &execenv.GCMeta{WorkspaceID: "ws1", CompletedAt: time.Now().Add(-96 * time.Hour)}
			if change == "meta-created" {
				meta = nil
			}
			root := createTaskDir(t, d.cfg.WorkspacesRoot, "ws1", "task1", meta)
			writeFile(t, filepath.Join(root, "codex-home", ".sandbox-bin", "codex"), 4096)
			old := time.Now().Add(-96 * time.Hour)
			if err := os.Chtimes(root, old, old); err != nil {
				t.Fatal(err)
			}
			plan, err := inspectTaskRoot(d.cfg.WorkspacesRoot, root)
			if err != nil {
				t.Fatal(err)
			}
			defer plan.close()
			kept := root
			switch change {
			case "owner-replaced":
				path := filepath.Join(root, ".task_owner")
				if err := os.Rename(path, path+".old"); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, plan.files[".task_owner"].data, 0600); err != nil {
					t.Fatal(err)
				}
			case "meta-recent", "meta-created":
				if err := execenv.WriteGCMeta(root, execenv.GCMeta{Kind: execenv.GCKindIssue, WorkspaceID: "ws1"}, d.logger); err != nil {
					t.Fatal(err)
				}
			case "root-replaced":
				kept = root + "-old"
				if err := os.Rename(root, kept); err != nil {
					t.Fatal(err)
				}
				createTaskDir(t, d.cfg.WorkspacesRoot, "ws1", "task1", meta)
				writeFile(t, filepath.Join(root, "codex-home", ".sandbox-bin", "codex"), 8192)
			}
			stats := &gcStats{}
			d.cleanRetainedTaskArtifacts(context.Background(), root, gcActionClean, plan, stats)
			if stats.bytesReclaimed != 0 || stats.artifactRemoved != 0 {
				t.Fatalf("changed root reclaimed: %+v", stats)
			}
			if info, err := os.Stat(filepath.Join(kept, "codex-home", ".sandbox-bin", "codex")); err != nil || info.Size() != 4096 {
				t.Fatalf("original cache changed: %v", err)
			}
			if change == "root-replaced" {
				if info, err := os.Stat(filepath.Join(root, "codex-home", ".sandbox-bin", "codex")); err != nil || info.Size() != 8192 {
					t.Fatalf("replacement cache changed: %v", err)
				}
			}
		})
	}
}

func TestRetainedTaskArtifactsLegacyMetadataAge(t *testing.T) {
	for _, oldMeta := range []bool{false, true} {
		t.Run(map[bool]string{false: "recent-meta", true: "old-meta"}[oldMeta], func(t *testing.T) {
			d := newGCTestDaemon(t, http.NewServeMux())
			d.cfg.GCOrphanTTL = 72 * time.Hour
			root := createTaskDir(t, d.cfg.WorkspacesRoot, "ws1", "task1", &execenv.GCMeta{WorkspaceID: "ws1"})
			cache := filepath.Join(root, "codex-home", ".sandbox-bin", "codex")
			artifact := filepath.Join(root, "workdir", "node_modules", "dependency")
			writeFile(t, cache, 4096)
			writeFile(t, artifact, 2048)
			old := time.Now().Add(-96 * time.Hour)
			if err := os.Chtimes(root, old, old); err != nil {
				t.Fatal(err)
			}
			if oldMeta {
				if err := os.Chtimes(filepath.Join(root, ".gc_meta.json"), old, old); err != nil {
					t.Fatal(err)
				}
			}
			stats := &gcStats{}
			d.applyGCAction(context.Background(), root, gcActionClean, stats)
			wantBytes := int64(0)
			if oldMeta {
				wantBytes = 4096
			}
			if stats.bytesReclaimed != wantBytes {
				t.Fatalf("bytes=%d, want %d", stats.bytesReclaimed, wantBytes)
			}
			_, err := os.Stat(cache)
			if oldMeta && !os.IsNotExist(err) || !oldMeta && err != nil {
				t.Fatalf("cache removal old-meta=%v: %v", oldMeta, err)
			}
			if info, err := os.Stat(artifact); err != nil || info.Size() != 2048 {
				t.Fatalf("legacy root's basename artifact changed: %v", err)
			}
		})
	}
}

func TestRetainedTaskArtifactsPreserveGit(t *testing.T) {
	d := newGCTestDaemon(t, http.NewServeMux())
	root := createTaskDir(t, d.cfg.WorkspacesRoot, "ws1", "task1", &execenv.GCMeta{WorkspaceID: "ws1", CompletedAt: time.Now().Add(-96 * time.Hour)})
	repo := filepath.Join(root, "workdir")
	runGitForGC(t, "", "clone", createGCGitRepo(t), repo)
	tracked := filepath.Join(repo, "node_modules", "source.txt")
	writeFile(t, tracked, 31)
	runGitForGC(t, "", "-C", repo, "add", ".")
	runGitForGC(t, "", "-C", repo, "-c", "user.name=Test", "-c", "user.email=test@example.invalid", "commit", "-m", "local tracked result")
	writeFile(t, tracked, 37) // An uncommitted edit must survive too.
	nested := filepath.Join(repo, ".next", "checkout")
	runGitForGC(t, "", "clone", createGCGitRepo(t), nested)
	refs := runGitForGC(t, "", "-C", repo, "show-ref")
	status := runGitForGC(t, "", "-C", repo, "status", "--porcelain")
	cache := filepath.Join(root, "codex-home", ".sandbox-bin", "codex")
	writeFile(t, cache, 4096)
	stats := &gcStats{}
	d.applyGCAction(context.Background(), root, gcActionClean, stats)
	if stats.bytesReclaimed != 4096 || stats.artifactRemoved != 1 || stats.cleaned != 0 {
		t.Fatalf("wrong cleanup: %+v", stats)
	}
	if _, err := os.Stat(cache); !os.IsNotExist(err) {
		t.Fatalf("cache not reclaimed: %v", err)
	}
	if got := runGitForGC(t, "", "-C", repo, "show-ref"); got != refs {
		t.Fatal("local refs changed")
	}
	if got := runGitForGC(t, "", "-C", repo, "status", "--porcelain"); got != status {
		t.Fatal("working tree changed")
	}
	if info, err := os.Stat(tracked); err != nil || info.Size() != 37 {
		t.Fatalf("tracked edit changed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(nested, ".git", "HEAD")); err != nil {
		t.Fatalf("nested checkout changed: %v", err)
	}
}
