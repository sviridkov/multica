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

func TestWholeTaskGCProtectsWork(t *testing.T) {
	for _, action := range []gcAction{gcActionClean, gcActionOrphan} {
		for _, content := range []string{"workdir/untracked.txt", "output/result.md", "scratch/evidence.json", "workdir/nested/.git/HEAD", ".git", ".task_lock", "unknown"} {
			t.Run(content+"/"+string(rune('0'+action)), func(t *testing.T) {
				d := newGCTestDaemon(t, http.NewServeMux())
				root := createTaskDir(t, d.cfg.WorkspacesRoot, "ws1", "task1", nil)
				path := filepath.Join(root, content)
				writeFile(t, path, 37)
				before, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				stats := &gcStats{}
				if n := d.applyGCAction(context.Background(), root, action, stats); n != 0 || stats.bytesReclaimed != 0 || stats.skipped != 1 {
					t.Fatalf("work was reclaimed: n=%d stats=%+v", n, stats)
				}
				after, err := os.ReadFile(path)
				if err != nil || string(after) != string(before) {
					t.Fatalf("work changed: %q, %v", after, err)
				}
				if _, err := os.Stat(filepath.Join(root, ".task_owner")); err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}

func TestWholeTaskGCProtectsGitHistory(t *testing.T) {
	for _, state := range []string{"clean-pushed", "dirty", "untracked", "unpushed", "stash", "local-ref", "linked-worktree", "nested"} {
		t.Run(state, func(t *testing.T) {
			d := newGCTestDaemon(t, http.NewServeMux())
			root := createTaskDir(t, d.cfg.WorkspacesRoot, "ws1", "task1", nil)
			repo := filepath.Join(root, "workdir")
			if state == "nested" {
				repo = filepath.Join(repo, "nested")
			}
			source := createGCGitRepo(t)
			if state == "linked-worktree" {
				runGitForGC(t, "", "-C", source, "worktree", "add", "-b", "retained", repo)
			} else {
				runGitForGC(t, "", "clone", source, repo)
			}
			switch state {
			case "dirty", "stash":
				writeFile(t, filepath.Join(repo, "README.md"), 17)
				runGitForGC(t, "", "-C", repo, "add", ".")
				if state == "stash" {
					runGitForGC(t, "", "-C", repo, "stash", "push")
				}
			case "untracked":
				writeFile(t, filepath.Join(repo, "evidence.txt"), 29)
			case "unpushed":
				runGitForGC(t, "", "-C", repo, "-c", "user.name=Test", "-c", "user.email=test@example.invalid", "commit", "--allow-empty", "-m", "local")
			case "local-ref":
				runGitForGC(t, "", "-C", repo, "branch", "local-result")
			}
			before := runGitForGC(t, "", "-C", repo, "show-ref")
			status := runGitForGC(t, "", "-C", repo, "status", "--porcelain")
			if _, removed := d.cleanTaskDir(root); removed {
				t.Fatal("Git checkout was deleted")
			}
			if got := runGitForGC(t, "", "-C", repo, "show-ref"); got != before {
				t.Fatalf("refs changed: %s", got)
			}
			if got := runGitForGC(t, "", "-C", repo, "status", "--porcelain"); got != status {
				t.Fatalf("work changed: %s", got)
			}
		})
	}
}

func TestWholeTaskGCSafeEmptyLifecycle(t *testing.T) {
	for _, route := range []string{"terminal", "completed", "orphan"} {
		t.Run(route, func(t *testing.T) {
			mux := http.NewServeMux()
			mux.HandleFunc("/api/daemon/issues/issue1/gc-check", func(w http.ResponseWriter, r *http.Request) {
				status := "in_progress"
				if route == "terminal" {
					status = "done"
				}
				json.NewEncoder(w).Encode(map[string]any{"status": status, "updated_at": time.Now().Add(-48 * time.Hour)})
			})
			d := newGCTestDaemon(t, mux)
			d.cfg.GCTTL = time.Hour
			d.cfg.GCCompletedTaskTTL = time.Hour
			d.cfg.GCOrphanTTL = 0
			var meta *execenv.GCMeta
			if route != "orphan" {
				meta = &execenv.GCMeta{WorkspaceID: "ws1", TaskID: "task1", IssueID: "issue1", CompletedAt: time.Now().Add(-48 * time.Hour)}
			}
			root := createTaskDir(t, d.cfg.WorkspacesRoot, "ws1", "task1", meta)
			for _, name := range []string{"workdir", "logs", "output"} {
				if err := os.Mkdir(filepath.Join(root, name), 0700); err != nil {
					t.Fatal(err)
				}
			}
			action := d.shouldCleanTaskDir(context.Background(), root)
			want := gcActionClean
			if route == "orphan" {
				want = gcActionOrphan
			}
			if action != want {
				t.Fatalf("action=%d, want %d", action, want)
			}
			stats := &gcStats{}
			if n := d.applyGCAction(context.Background(), root, action, stats); n != 1 || stats.bytesReclaimed <= 0 {
				t.Fatalf("empty root not reclaimed: n=%d stats=%+v", n, stats)
			}
			if _, err := os.Stat(root); !os.IsNotExist(err) {
				t.Fatalf("root remains: %v", err)
			}
		})
	}
}

func TestWholeTaskGCRejectsUncertainMetadata(t *testing.T) {
	for _, body := range []string{`broken`, `{"workspace_id":"foreign"}`, `{"workspace_id":"ws1","task_id":"stale"}`, `{"workspace_id":"ws1","local_directory":true}`, `{"workspace_id":"ws1","kind":"unknown"}`, `{"workspace_id":"ws1","future_result":"keep"}`} {
		t.Run(body, func(t *testing.T) {
			d := newGCTestDaemon(t, http.NewServeMux())
			root := createTaskDir(t, d.cfg.WorkspacesRoot, "ws1", "task1", nil)
			if err := os.WriteFile(filepath.Join(root, ".gc_meta.json"), []byte(body), 0600); err != nil {
				t.Fatal(err)
			}
			if _, removed := d.cleanTaskDir(root); removed {
				t.Fatal("uncertain metadata deleted")
			}
		})
	}
}

func TestWholeTaskGCRevalidatesBeforeRemoval(t *testing.T) {
	for _, change := range []string{"new-work", "owner", "meta", "directory-identity", "task-identity", "workspace-identity"} {
		t.Run(change, func(t *testing.T) {
			d := newGCTestDaemon(t, http.NewServeMux())
			root := createTaskDir(t, d.cfg.WorkspacesRoot, "ws1", "task1", &execenv.GCMeta{WorkspaceID: "ws1"})
			if err := os.Mkdir(filepath.Join(root, "workdir"), 0700); err != nil {
				t.Fatal(err)
			}
			plan, err := inspectEmptyTaskRoot(d.cfg.WorkspacesRoot, root)
			if err != nil {
				t.Fatal(err)
			}
			defer plan.close()
			kept := root
			switch change {
			case "new-work":
				writeFile(t, filepath.Join(root, "workdir", "arrived.txt"), 31)
			case "owner":
				if err := os.WriteFile(filepath.Join(root, ".task_owner"), []byte(`{"workspace_id":"ws1","task_id":"foreign"}`), 0600); err != nil {
					t.Fatal(err)
				}
			case "meta":
				if err := os.WriteFile(filepath.Join(root, ".gc_meta.json"), []byte(`{"workspace_id":"ws1","local_directory":true}`), 0600); err != nil {
					t.Fatal(err)
				}
			case "directory-identity":
				if err := os.Rename(filepath.Join(root, "workdir"), filepath.Join(root, "moved")); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(filepath.Join(root, "workdir"), 0700); err != nil {
					t.Fatal(err)
				}
			case "task-identity":
				kept = root + "-old"
				if err := os.Rename(root, kept); err != nil {
					t.Fatal(err)
				}
				createTaskDir(t, d.cfg.WorkspacesRoot, "ws1", "task1", nil)
			case "workspace-identity":
				if err := os.Rename(filepath.Dir(root), filepath.Dir(root)+"-old"); err != nil {
					t.Fatal(err)
				}
				kept = filepath.Join(filepath.Dir(root)+"-old", "task1")
				createTaskDir(t, d.cfg.WorkspacesRoot, "ws1", "task1", nil)
			}
			if n, err := plan.remove(); err == nil || n != 0 {
				t.Fatalf("changed root removed: bytes=%d err=%v", n, err)
			}
			if _, err := os.Stat(filepath.Join(kept, ".task_owner")); err != nil {
				t.Fatalf("original ownership lost: %v", err)
			}
			if change == "new-work" {
				if info, err := os.Stat(filepath.Join(root, "workdir", "arrived.txt")); err != nil || info.Size() != 31 {
					t.Fatalf("late work lost: %v", err)
				}
			}
		})
	}
}

func TestWholeTaskGCRefusesLinksAndOutsidePaths(t *testing.T) {
	for _, kind := range []string{"outside", "workspace-link", "task-link", "workdir-link", "owner-link", "empty-unknown-dir"} {
		t.Run(kind, func(t *testing.T) {
			d := newGCTestDaemon(t, http.NewServeMux())
			outside := t.TempDir()
			root := createTaskDir(t, d.cfg.WorkspacesRoot, "ws1", "task1", nil)
			target := filepath.Join(outside, "target")
			if err := os.Mkdir(target, 0700); err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "outside":
				root = createTaskDir(t, outside, "ws1", "task1", nil)
			case "workspace-link", "task-link":
				path := root
				if kind == "workspace-link" {
					path = filepath.Dir(root)
				}
				target = filepath.Join(outside, "original")
				if err := os.Rename(path, target); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, path); err != nil {
					t.Skipf("symlinks unavailable: %v", err)
				}
			case "workdir-link":
				if err := os.Symlink(target, filepath.Join(root, "workdir")); err != nil {
					t.Skipf("symlinks unavailable: %v", err)
				}
			case "owner-link":
				target = filepath.Join(outside, "owner")
				if err := os.Rename(filepath.Join(root, ".task_owner"), target); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, filepath.Join(root, ".task_owner")); err != nil {
					t.Skipf("symlinks unavailable: %v", err)
				}
			case "empty-unknown-dir":
				if err := os.Mkdir(filepath.Join(root, "unknown"), 0700); err != nil {
					t.Fatal(err)
				}
			}
			if _, removed := d.cleanTaskDir(root); removed {
				t.Fatal("uncertain path was removed")
			}
			if _, err := os.Lstat(root); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(target); err != nil {
				t.Fatalf("outside target changed: %v", err)
			}
		})
	}
}

func TestWholeTaskGCEmptyRemovalDoesNotConsumeLateFiles(t *testing.T) {
	root, err := os.OpenRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if err := root.Mkdir("workdir", 0700); err != nil {
		t.Fatal(err)
	}
	if err := root.WriteFile("workdir/result", []byte("arrived after inspection"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := removeGCEmptyDirectory(root, "workdir"); err == nil {
		t.Fatal("nonempty directory removed")
	}
	if err := root.WriteFile("replacement", []byte("file replacing a directory"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := removeGCEmptyDirectory(root, "replacement"); err == nil {
		t.Fatal("replacement file removed")
	}
	for _, name := range []string{"workdir/result", "replacement"} {
		if _, err := root.ReadFile(name); err != nil {
			t.Fatal(err)
		}
	}
}

func TestWholeTaskGCRetainsHeldAndReleasedLock(t *testing.T) {
	d := newGCTestDaemon(t, http.NewServeMux())
	claim, err := execenv.ClaimEnvRoot(execenv.RootDirParams{WorkspacesRoot: d.cfg.WorkspacesRoot, WorkspaceID: "ws1", TaskID: "task1"})
	if err != nil {
		t.Fatal(err)
	}
	defer claim.Release()
	if _, removed := d.cleanTaskDir(claim.RootDir()); removed {
		t.Fatal("held lock root removed")
	}
	claim.Release()
	if _, removed := d.cleanTaskDir(claim.RootDir()); removed {
		t.Fatal("released lock is still retained conservatively")
	}
}

func TestWholeTaskGCReservationBlocksResume(t *testing.T) {
	d := newGCTestDaemon(t, http.NewServeMux())
	root := createTaskDir(t, d.cfg.WorkspacesRoot, "ws1", "task1", nil)
	release, ok := d.reserveEnvRootForGC(root)
	if !ok {
		t.Fatal("reservation failed")
	}
	resumed := make(chan struct{})
	go func() { d.markActiveEnvRoot(root); close(resumed) }()
	select {
	case <-resumed:
		release()
		t.Fatal("resume entered during reservation")
	case <-time.After(20 * time.Millisecond):
	}
	if _, removed := d.cleanTaskDir(root); !removed {
		release()
		t.Fatal("safe reserved root not removed")
	}
	release()
	select {
	case <-resumed:
	case <-time.After(5 * time.Second):
		t.Fatal("resume did not wake after reservation")
	}
	defer d.unmarkActiveEnvRoot(root)
	createTaskDir(t, d.cfg.WorkspacesRoot, "ws1", "task1", nil)
	if n := d.applyGCAction(context.Background(), root, gcActionOrphan, &gcStats{}); n != 0 {
		t.Fatal("resumed root removed")
	}
}
