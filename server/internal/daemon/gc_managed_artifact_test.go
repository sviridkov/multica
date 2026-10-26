package daemon

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/daemon/execenv"
)

// The managed Codex cache (codex-home/.sandbox-bin, a ~285 MiB copy of the
// Codex binary) used to be reclaimed only on the issue decision path. Every
// other kind held it for as long as its parent record said "keep the
// directory" — unbounded for a chat session, which stays "active" with no time
// limit. See #6782, and #5654 for the original issue-path fix.
//
// sandboxBinRel is the exact managed subpath, spelled out here so a change to
// ManagedReclaimableArtifactSubpaths has to face these tests.
const sandboxBinRel = "codex-home/.sandbox-bin"

// chatGCMux serves a chat gc-check that reports the given status.
func chatGCMux(chatID, status string) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc(fmt.Sprintf("/api/daemon/chat-sessions/%s/gc-check", chatID), func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status":     status,
			"updated_at": time.Now().Add(-100 * 24 * time.Hour),
		})
	})
	return mux
}

func assertGone(t *testing.T, taskDir, rel string) {
	t.Helper()
	if _, err := os.Stat(filepath.Join(taskDir, rel)); !os.IsNotExist(err) {
		t.Fatalf("%s should have been reclaimed, stat err = %v", rel, err)
	}
}

func assertKept(t *testing.T, taskDir string, rels ...string) {
	t.Helper()
	for _, rel := range rels {
		if _, err := os.Stat(filepath.Join(taskDir, rel)); err != nil {
			t.Fatalf("%s must be preserved: %v", rel, err)
		}
	}
}

// TestManagedArtifact_IdleActiveChatReclaimsSandboxBin is the #6782 regression
// test: an active chat session idle past GCArtifactTTL gives the managed Codex
// cache back, while everything the session owns survives.
func TestManagedArtifact_IdleActiveChatReclaimsSandboxBin(t *testing.T) {
	t.Parallel()
	chatID := "aaaaaaaa-0000-0000-0000-000000000001"
	d := newGCTestDaemon(t, chatGCMux(chatID, "active"))

	meta := &execenv.GCMeta{
		Kind:          execenv.GCKindChat,
		ChatSessionID: chatID,
		WorkspaceID:   "ws",
		CompletedAt:   time.Now().Add(-24 * time.Hour), // 2x the 12h artifact TTL
	}
	taskDir := createTaskDir(t, d.cfg.WorkspacesRoot, "ws", "idle-chat", meta)
	writeFile(t, filepath.Join(taskDir, sandboxBinRel, "codex"), 4096)
	// Everything below must survive: session data, the audit trail, and a
	// user-owned directory that merely shares the managed basename.
	writeFile(t, filepath.Join(taskDir, "logs/run.log"), 32)
	writeFile(t, filepath.Join(taskDir, "output/result.md"), 32)
	writeFile(t, filepath.Join(taskDir, "codex-home/auth.json"), 32)
	writeFile(t, filepath.Join(taskDir, "workdir/repo/.sandbox-bin/user-owned"), 32)

	stats := &gcStats{byPattern: map[string]int{}}
	action := d.shouldCleanTaskDir(context.Background(), taskDir)
	if action != gcActionCleanManagedArtifacts {
		t.Fatalf("want gcActionCleanManagedArtifacts, got %d", action)
	}
	d.applyGCAction(context.Background(), taskDir, action, stats)

	assertGone(t, taskDir, sandboxBinRel)
	assertKept(t, taskDir,
		"logs/run.log",
		"output/result.md",
		"codex-home/auth.json",
		"workdir/repo/.sandbox-bin/user-owned",
		".gc_meta.json",
	)
	if stats.artifactRemoved != 1 {
		t.Fatalf("artifact_removed = %d, want 1", stats.artifactRemoved)
	}
	if got := stats.byPattern[managedArtifactPatternPrefix+sandboxBinRel]; got != 1 {
		t.Fatalf("managed pattern count = %d, want 1", got)
	}
}

// A chat session that completed recently keeps its cache — resuming inside the
// TTL must not pay for a re-provision.
func TestManagedArtifact_FreshActiveChatKeepsSandboxBin(t *testing.T) {
	t.Parallel()
	chatID := "aaaaaaaa-0000-0000-0000-000000000002"
	d := newGCTestDaemon(t, chatGCMux(chatID, "active"))

	taskDir := createTaskDir(t, d.cfg.WorkspacesRoot, "ws", "fresh-chat", &execenv.GCMeta{
		Kind:          execenv.GCKindChat,
		ChatSessionID: chatID,
		WorkspaceID:   "ws",
		CompletedAt:   time.Now().Add(-1 * time.Hour), // well inside the 12h TTL
	})
	writeFile(t, filepath.Join(taskDir, sandboxBinRel, "codex"), 4096)

	if got := d.shouldCleanTaskDir(context.Background(), taskDir); got != gcActionSkip {
		t.Fatalf("want gcActionSkip inside the TTL, got %d", got)
	}
	assertKept(t, taskDir, sandboxBinRel+"/codex")
}

// MULTICA_GC_ARTIFACT_TTL=0 disables the managed reclaim along with the rest of
// artifact cleanup — the documented opt-out.
func TestManagedArtifact_ArtifactTTLZeroDisablesFallback(t *testing.T) {
	t.Parallel()
	chatID := "aaaaaaaa-0000-0000-0000-000000000003"
	d := newGCTestDaemon(t, chatGCMux(chatID, "active"))
	d.cfg.GCArtifactTTL = 0

	taskDir := createTaskDir(t, d.cfg.WorkspacesRoot, "ws", "no-ttl", &execenv.GCMeta{
		Kind:          execenv.GCKindChat,
		ChatSessionID: chatID,
		WorkspaceID:   "ws",
		CompletedAt:   time.Now().Add(-365 * 24 * time.Hour),
	})
	writeFile(t, filepath.Join(taskDir, sandboxBinRel, "codex"), 4096)

	if got := d.shouldCleanTaskDir(context.Background(), taskDir); got != gcActionSkip {
		t.Fatalf("want gcActionSkip with artifact TTL disabled, got %d", got)
	}
	assertKept(t, taskDir, sandboxBinRel+"/codex")
}

// Metadata with no completed_at stays with the per-kind legacy handling rather
// than being reclaimed off an unrelated clock.
func TestManagedArtifact_ZeroCompletedAtIsLeftAlone(t *testing.T) {
	t.Parallel()
	chatID := "aaaaaaaa-0000-0000-0000-000000000004"
	d := newGCTestDaemon(t, chatGCMux(chatID, "active"))

	taskDir := createTaskDir(t, d.cfg.WorkspacesRoot, "ws", "legacy-chat", &execenv.GCMeta{
		Kind:          execenv.GCKindChat,
		ChatSessionID: chatID,
		WorkspaceID:   "ws",
	})
	writeFile(t, filepath.Join(taskDir, sandboxBinRel, "codex"), 4096)

	if got := d.shouldCleanTaskDir(context.Background(), taskDir); got != gcActionSkip {
		t.Fatalf("want gcActionSkip for zero completed_at, got %d", got)
	}
	assertKept(t, taskDir, sandboxBinRel+"/codex")
}

// The fallback is a one-way upgrade from skip: it must never weaken a decision
// that already removes more.
func TestManagedArtifact_NeverDowngradesStrongerActions(t *testing.T) {
	t.Parallel()
	d := newGCTestDaemon(t, http.NewServeMux())
	meta := &execenv.GCMeta{
		Kind:        execenv.GCKindChat,
		WorkspaceID: "ws",
		CompletedAt: time.Now().Add(-365 * 24 * time.Hour),
	}

	for _, action := range []gcAction{gcActionClean, gcActionOrphan, gcActionCleanArtifacts, gcActionCleanManagedArtifacts} {
		if got := d.applyManagedArtifactFallback("dir", meta, action); got != action {
			t.Fatalf("action %d was rewritten to %d", action, got)
		}
	}
}

// The same hole existed for the other non-issue kinds. These reach a terminal
// state quickly in practice, so they are bounded either way — but a run that
// sits non-terminal should not pin the cache.
func TestManagedArtifact_NonTerminalAutopilotAndQuickCreate(t *testing.T) {
	t.Parallel()
	runID := "bbbbbbbb-0000-0000-0000-000000000001"
	taskID := "bbbbbbbb-0000-0000-0000-000000000002"

	mux := http.NewServeMux()
	mux.HandleFunc(fmt.Sprintf("/api/daemon/autopilot-runs/%s/gc-check", runID), func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "running"})
	})
	mux.HandleFunc(fmt.Sprintf("/api/daemon/tasks/%s/gc-check", taskID), func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "running"})
	})
	d := newGCTestDaemon(t, mux)

	cases := []struct {
		name string
		meta *execenv.GCMeta
	}{
		{"autopilot-run", &execenv.GCMeta{Kind: execenv.GCKindAutopilotRun, AutopilotRunID: runID, WorkspaceID: "ws"}},
		{"quick_create", &execenv.GCMeta{Kind: execenv.GCKindQuickCreate, TaskID: taskID, WorkspaceID: "ws"}},
	}
	for _, tc := range cases {
		tc.meta.CompletedAt = time.Now().Add(-24 * time.Hour)
		taskDir := createTaskDir(t, d.cfg.WorkspacesRoot, "ws", tc.name, tc.meta)
		writeFile(t, filepath.Join(taskDir, sandboxBinRel, "codex"), 4096)

		action := d.shouldCleanTaskDir(context.Background(), taskDir)
		if action != gcActionCleanManagedArtifacts {
			t.Fatalf("%s: want gcActionCleanManagedArtifacts, got %d", tc.name, action)
		}
		d.applyGCAction(context.Background(), taskDir, action, &gcStats{byPattern: map[string]int{}})
		assertGone(t, taskDir, sandboxBinRel)
		assertKept(t, taskDir, ".gc_meta.json")
	}
}

// A task actively running on the directory keeps its cache — the fallback sits
// behind the isActiveEnvRoot short-circuit.
func TestManagedArtifact_ActiveEnvRootKeepsSandboxBin(t *testing.T) {
	t.Parallel()
	chatID := "aaaaaaaa-0000-0000-0000-000000000005"
	d := newGCTestDaemon(t, chatGCMux(chatID, "active"))

	taskDir := createTaskDir(t, d.cfg.WorkspacesRoot, "ws", "running-chat", &execenv.GCMeta{
		Kind:          execenv.GCKindChat,
		ChatSessionID: chatID,
		WorkspaceID:   "ws",
		CompletedAt:   time.Now().Add(-365 * 24 * time.Hour),
	})
	writeFile(t, filepath.Join(taskDir, sandboxBinRel, "codex"), 4096)

	d.markActiveEnvRoot(taskDir)
	if got := d.shouldCleanTaskDir(context.Background(), taskDir); got != gcActionSkip {
		t.Fatalf("want gcActionSkip for an active env root, got %d", got)
	}
	assertKept(t, taskDir, sandboxBinRel+"/codex")
}

// local_directory tasks keep their envRoot forever, so the managed cache is the
// only thing the GC may ever take from them. The fallback must still apply.
func TestManagedArtifact_LocalDirectoryChatReclaimsSandboxBin(t *testing.T) {
	t.Parallel()
	chatID := "aaaaaaaa-0000-0000-0000-000000000006"
	d := newGCTestDaemon(t, chatGCMux(chatID, "active"))

	taskDir := createTaskDir(t, d.cfg.WorkspacesRoot, "ws", "local-chat", &execenv.GCMeta{
		Kind:           execenv.GCKindChat,
		ChatSessionID:  chatID,
		WorkspaceID:    "ws",
		LocalDirectory: true,
		CompletedAt:    time.Now().Add(-24 * time.Hour),
	})
	writeFile(t, filepath.Join(taskDir, sandboxBinRel, "codex"), 4096)
	writeFile(t, filepath.Join(taskDir, "logs/run.log"), 32)

	action := d.shouldCleanTaskDir(context.Background(), taskDir)
	if action != gcActionCleanManagedArtifacts {
		t.Fatalf("want gcActionCleanManagedArtifacts, got %d", action)
	}
	d.applyGCAction(context.Background(), taskDir, action, &gcStats{byPattern: map[string]int{}})
	assertGone(t, taskDir, sandboxBinRel)
	assertKept(t, taskDir, "logs/run.log", ".gc_meta.json")
}

// Once the cache is gone, a long-lived active chat must stop re-deciding to
// reclaim it. completed_at never moves again for a session that stays active,
// so without a presence check the GC would take an env-root reservation and run
// a removal pass every cycle, forever, for a directory with nothing to give.
func TestManagedArtifact_SecondCycleIsANoOp(t *testing.T) {
	t.Parallel()
	chatID := "aaaaaaaa-0000-0000-0000-000000000007"
	d := newGCTestDaemon(t, chatGCMux(chatID, "active"))

	taskDir := createTaskDir(t, d.cfg.WorkspacesRoot, "ws", "repeat-chat", &execenv.GCMeta{
		Kind:          execenv.GCKindChat,
		ChatSessionID: chatID,
		WorkspaceID:   "ws",
		CompletedAt:   time.Now().Add(-24 * time.Hour),
	})
	writeFile(t, filepath.Join(taskDir, sandboxBinRel, "codex"), 4096)

	first := d.shouldCleanTaskDir(context.Background(), taskDir)
	if first != gcActionCleanManagedArtifacts {
		t.Fatalf("first cycle: want gcActionCleanManagedArtifacts, got %d", first)
	}
	d.applyGCAction(context.Background(), taskDir, first, &gcStats{byPattern: map[string]int{}})
	assertGone(t, taskDir, sandboxBinRel)

	for cycle := 2; cycle <= 3; cycle++ {
		if got := d.shouldCleanTaskDir(context.Background(), taskDir); got != gcActionSkip {
			t.Fatalf("cycle %d: want gcActionSkip once the cache is gone, got %d", cycle, got)
		}
	}

	// A later Codex run re-provisions the cache; the GC must notice it again.
	writeFile(t, filepath.Join(taskDir, sandboxBinRel, "codex"), 4096)
	if got := d.shouldCleanTaskDir(context.Background(), taskDir); got != gcActionCleanManagedArtifacts {
		t.Fatalf("want reclaim after re-provision, got %d", got)
	}
}

// The direct-path removal replaced a tree walk that refused to descend through
// symlinks and junctions. It has to refuse at every component, not just the
// leaf — codex-home links the user's real ~/.codex content into itself.
func TestManagedArtifact_DirectRemovalDoesNotFollowLinks(t *testing.T) {
	t.Parallel()
	d := newGCTestDaemon(t, http.NewServeMux())

	for _, tc := range []struct {
		name     string
		linkPath string
	}{
		{name: "leaf", linkPath: sandboxBinRel},
		{name: "parent", linkPath: "codex-home"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			taskDir := t.TempDir()
			outside := t.TempDir()
			keepFile := filepath.Join(outside, "keep")
			writeFile(t, keepFile, 10)
			// Mirror the shape that makes this dangerous: the link target is
			// the user's real ~/.codex, which has a .sandbox-bin of its own.
			// Traversing the link would resolve the managed path onto it.
			userSandboxBin := filepath.Join(outside, ".sandbox-bin", "user-owned")
			writeFile(t, userSandboxBin, 10)

			linkPath := filepath.Join(taskDir, filepath.FromSlash(tc.linkPath))
			if err := os.MkdirAll(filepath.Dir(linkPath), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(outside, linkPath); err != nil {
				t.Skipf("symlink not supported: %v", err)
			}

			removed, bytes, _ := d.cleanManagedTaskArtifacts(context.Background(), taskDir)
			if removed != 0 || bytes != 0 {
				t.Fatalf("removed=%d bytes=%d, want 0 through a link", removed, bytes)
			}
			for _, keep := range []string{keepFile, userSandboxBin} {
				if _, err := os.Stat(keep); err != nil {
					t.Fatalf("link target was touched: %v", err)
				}
			}
		})
	}
}

// A repository directory that merely shares the managed leaf name is not the
// managed path and must survive the direct removal.
func TestManagedArtifact_DirectRemovalIgnoresSameNamedUserDir(t *testing.T) {
	t.Parallel()
	d := newGCTestDaemon(t, http.NewServeMux())

	taskDir := t.TempDir()
	writeFile(t, filepath.Join(taskDir, "workdir/repo/.sandbox-bin/user-owned"), 32)
	writeFile(t, filepath.Join(taskDir, "codex-home/auth.json"), 32)

	if removed, _, _ := d.cleanManagedTaskArtifacts(context.Background(), taskDir); removed != 0 {
		t.Fatalf("removed=%d, want 0 when the managed path is absent", removed)
	}
	assertKept(t, taskDir, "workdir/repo/.sandbox-bin/user-owned", "codex-home/auth.json")
}

// When the batch issue check cannot answer (server error, scoped token), the
// task data stays but the regenerable cache is still reclaimed.
func TestManagedArtifact_UnreachableIssueStillReclaimsCache(t *testing.T) {
	t.Parallel()
	issueID := "cccccccc-0000-0000-0000-000000000001"

	mux := http.NewServeMux()
	mux.HandleFunc("/api/daemon/workspaces/ws/issues/gc-checks", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	})
	mux.HandleFunc(fmt.Sprintf("/api/daemon/issues/%s/gc-check", issueID), func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	})
	d := newGCTestDaemon(t, mux)

	meta := &execenv.GCMeta{
		Kind:        execenv.GCKindIssue,
		IssueID:     issueID,
		WorkspaceID: "ws",
		CompletedAt: time.Now().Add(-24 * time.Hour),
	}
	taskDir := createTaskDir(t, d.cfg.WorkspacesRoot, "ws", "unreachable-issue", meta)
	writeFile(t, filepath.Join(taskDir, sandboxBinRel, "codex"), 4096)
	writeFile(t, filepath.Join(taskDir, "output/result.md"), 32)

	stats := &gcStats{byPattern: map[string]int{}}
	d.gcWorkspaceIssues(context.Background(), "ws", []issueGCCandidate{{taskDir: taskDir, meta: meta}}, stats)

	assertGone(t, taskDir, sandboxBinRel)
	assertKept(t, taskDir, "output/result.md", ".gc_meta.json")
}

// The managed reclaim used to share cleanTaskArtifacts' tree walk and now
// addresses its paths directly. The two must agree on exactly what they remove
// — this pins that equivalence against the walk, which is still live for the
// basename patterns, so a future managed subpath or a change to either side
// cannot silently drift them apart.
func TestManagedArtifact_DirectRemovalMatchesTreeWalk(t *testing.T) {
	t.Parallel()
	upstream := codexMarketplaceUpstream(t)

	fixtures := map[string]func(t *testing.T, dir string){
		"populated Codex plugin caches without proof": func(t *testing.T, dir string) {
			installCodexMarketplace(t, dir, "alpha", upstream)
			writeRemoteCatalog(t, filepath.Join(dir, filepath.FromSlash(remoteCatalogRel)), remoteCatalogFile, remoteCatalogFixture)
			writeFile(t, filepath.Join(dir, "codex-home/cache/codex_apps_tools/tools.json"), 16)
		},
		"kept Codex plugin caches": func(t *testing.T, dir string) {
			clone := installCodexMarketplace(t, dir, "alpha", upstream)
			writeFile(t, filepath.Join(clone, "notes.md"), 8)
			writeRemoteCatalog(t, filepath.Join(dir, filepath.FromSlash(remoteCatalogRel)), "auth.json", `{"access_token":"secret"}`)
		},
		"same-named project caches only": func(t *testing.T, dir string) {
			writeFile(t, filepath.Join(dir, "workdir/repo/.tmp/marketplaces/notes.md"), 16)
			writeFile(t, filepath.Join(dir, "workdir/repo/codex-home/cache/remote_plugin_catalog", remoteCatalogFile), 16)
		},
		"managed present": func(t *testing.T, dir string) {
			writeFile(t, filepath.Join(dir, sandboxBinRel, "codex"), 4096)
			writeFile(t, filepath.Join(dir, "codex-home/auth.json"), 16)
		},
		"managed absent": func(t *testing.T, dir string) {
			writeFile(t, filepath.Join(dir, "codex-home/auth.json"), 16)
		},
		"user-owned same name only": func(t *testing.T, dir string) {
			writeFile(t, filepath.Join(dir, "workdir/repo/.sandbox-bin/keep"), 16)
		},
		"both managed and user-owned": func(t *testing.T, dir string) {
			writeFile(t, filepath.Join(dir, sandboxBinRel, "codex"), 4096)
			writeFile(t, filepath.Join(dir, "workdir/repo/.sandbox-bin/keep"), 16)
		},
		"nested content and .git": func(t *testing.T, dir string) {
			writeFile(t, filepath.Join(dir, sandboxBinRel, "a/b/c/codex"), 2048)
			writeFile(t, filepath.Join(dir, sandboxBinRel, ".git/objects/x"), 16)
			writeFile(t, filepath.Join(dir, "workdir/repo/.git/objects/y"), 16)
		},
		"codex-home is a regular file": func(t *testing.T, dir string) {
			writeFile(t, filepath.Join(dir, "codex-home"), 16)
		},
		"sandbox-bin is a regular file": func(t *testing.T, dir string) {
			writeFile(t, filepath.Join(dir, sandboxBinRel), 16)
		},
		"leaf symlink": func(t *testing.T, dir string) {
			outside := t.TempDir()
			writeFile(t, filepath.Join(outside, ".sandbox-bin/x"), 16)
			if err := os.MkdirAll(filepath.Join(dir, "codex-home"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(outside, filepath.Join(dir, sandboxBinRel)); err != nil {
				t.Skipf("symlink not supported: %v", err)
			}
		},
		"parent symlink": func(t *testing.T, dir string) {
			outside := t.TempDir()
			writeFile(t, filepath.Join(outside, ".sandbox-bin/x"), 16)
			if err := os.Symlink(outside, filepath.Join(dir, "codex-home")); err != nil {
				t.Skipf("symlink not supported: %v", err)
			}
		},
		"empty task dir": func(t *testing.T, dir string) {},
	}

	d := newGCTestDaemon(t, http.NewServeMux())
	walkMatcher := newArtifactMatcher(nil, execenv.ManagedReclaimableArtifactSubpaths())

	for name, setup := range fixtures {
		t.Run(name, func(t *testing.T) {
			walked, direct := t.TempDir(), t.TempDir()
			setup(t, walked)
			setup(t, direct)

			wRemoved, wBytes, wPattern := d.cleanTaskArtifactsMatching(context.Background(), walked, walkMatcher)
			dRemoved, dBytes, dPattern := d.cleanManagedTaskArtifacts(context.Background(), direct)

			if wRemoved != dRemoved || wBytes != dBytes || fmt.Sprint(wPattern) != fmt.Sprint(dPattern) {
				t.Fatalf("walk=(%d,%d,%v) direct=(%d,%d,%v)", wRemoved, wBytes, wPattern, dRemoved, dBytes, dPattern)
			}
			if w, dd := survivingTree(t, walked), survivingTree(t, direct); fmt.Sprint(w) != fmt.Sprint(dd) {
				t.Fatalf("survivors differ:\n walk=%v\n direct=%v", w, dd)
			}
		})
	}
}

// survivingTree lists every path left under root, relative to it, without
// following links out of the tree.
func survivingTree(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	_ = filepath.WalkDir(root, func(p string, e os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		rel, relErr := filepath.Rel(root, p)
		if relErr != nil {
			return nil
		}
		out = append(out, rel)
		if e.Type()&os.ModeSymlink != 0 {
			return filepath.SkipDir
		}
		return nil
	})
	return out
}

// Codex writes the two plugin caches below itself, and a person or a plugin
// can add to them, so the managed reclaim takes one only when it can prove
// Codex writes the same content again.
const (
	codexMarketplaceInstallRecord = ".codex-marketplace-install.json"
	marketplacesRel               = "codex-home/.tmp/marketplaces"
	remoteCatalogRel              = "codex-home/cache/remote_plugin_catalog"
	remoteCatalogFile             = "66ddc0fe5e30fa5e.json"
)

// remoteCatalogFixture has the shape Codex serializes: a schema version, the
// fetch time and the plugin list.
const remoteCatalogFixture = `{
  "schema_version": 1,
  "fetched_at": "2026-10-20T19:27:00Z",
  "plugins": [{"id": "plugin-1", "name": "alpha", "scope": "GLOBAL", "release": {"version": "1.0.0"}}]
}`

// codexMarketplaceUpstream returns a repository to install marketplaces from.
// Like real marketplaces it ships a tracked symlink (AGENTS.md -> CLAUDE.md),
// except on Windows, where Git checks links out as plain files by default.
func codexMarketplaceUpstream(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	writeFile(t, filepath.Join(repo, ".claude-plugin", "marketplace.json"), 64)
	writeFile(t, filepath.Join(repo, "plugins", "alpha", "SKILL.md"), 128)
	writeFile(t, filepath.Join(repo, "CLAUDE.md"), 32)
	if runtime.GOOS != "windows" {
		if err := os.Symlink("CLAUDE.md", filepath.Join(repo, "AGENTS.md")); err != nil {
			t.Fatal(err)
		}
	}
	runGitForGC(t, repo, "-c", "init.defaultBranch=main", "init", "--quiet")
	runGitForGC(t, repo, "add", "-A")
	runGitForGC(t, repo, "-c", "commit.gpgsign=false", "commit", "--quiet", "-m", "marketplace")
	return repo
}

// installCodexMarketplace installs upstream into taskDir the way Codex
// installs a configured Git marketplace.
func installCodexMarketplace(t *testing.T, taskDir, name, upstream string) string {
	t.Helper()
	return installCodexMarketplaceAt(t, filepath.Join(taskDir, filepath.FromSlash(marketplacesRel)), name, upstream)
}

// installCodexMarketplaceAt clones upstream below the install root, writes
// Codex's install record for the checkout and leaves the empty staging
// directory Codex keeps next to its clones.
func installCodexMarketplaceAt(t *testing.T, root, name, upstream string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(root, ".staging"), 0o755); err != nil {
		t.Fatal(err)
	}
	clone := filepath.Join(root, name)
	runGitForGC(t, "", "clone", "--quiet", upstream, clone)
	recordCodexInstall(t, clone, upstream)
	return clone
}

// recordCodexInstall writes Codex's install record for the current checkout.
func recordCodexInstall(t *testing.T, clone, source string) {
	t.Helper()
	record, err := json.Marshal(map[string]any{
		"source_type":  "git",
		"source":       source,
		"ref_name":     nil,
		"sparse_paths": []string{},
		"revision":     runGitForGC(t, clone, "rev-parse", "HEAD"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(clone, codexMarketplaceInstallRecord), record, 0o644); err != nil {
		t.Fatal(err)
	}
}

func writeRemoteCatalog(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// Empty roots can be removed through either lifecycle route. Populated caches
// stay even when their layout matches Codex. Recreating a cache does not prove
// its provenance; the same rule applies on the next cycle.
func TestManagedArtifact_CodexPluginCacheLifecycle(t *testing.T) {
	t.Parallel()
	upstream := codexMarketplaceUpstream(t)
	const id = "dddddddd-0000-0000-0000-000000000001"
	mux := chatGCMux(id, "active")
	mux.HandleFunc("/api/daemon/issues/"+id+"/gc-check", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "in_progress", "updated_at": time.Now()})
	})
	d := newGCTestDaemon(t, mux)
	for _, route := range []string{"direct", "walk"} {
		for _, populated := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/populated=%t", route, populated), func(t *testing.T) {
				meta := &execenv.GCMeta{Kind: execenv.GCKindChat, ChatSessionID: id, WorkspaceID: "ws", CompletedAt: time.Now().Add(-24 * time.Hour)}
				wantAction := gcActionCleanManagedArtifacts
				if route == "walk" {
					meta.Kind, meta.IssueID = execenv.GCKindIssue, id
					wantAction = gcActionCleanArtifacts
				}
				taskDir := createTaskDir(t, d.cfg.WorkspacesRoot, "ws", fmt.Sprintf("%s-%t", route, populated), meta)
				kept := []string{
					"codex-home/config.toml", "codex-home/auth.json",
					"codex-home/.tmp/plugins/curated/SKILL.md",
					"codex-home/cache/codex_apps_tools/tools.json",
					"codex-home/plugins/cache/alpha/plugin.json",
					"workdir/repo/.tmp/marketplaces/notes.md",
					"workdir/repo/cache/remote_plugin_catalog/" + remoteCatalogFile,
					"workdir/repo/codex-home/.tmp/marketplaces/notes.md",
					"output/result.md", "logs/run.log",
				}
				for _, rel := range kept {
					writeFile(t, filepath.Join(taskDir, filepath.FromSlash(rel)), 16)
				}
				for cycle := 1; cycle <= 2; cycle++ {
					if populated {
						installCodexMarketplace(t, taskDir, "alpha", upstream)
						writeRemoteCatalog(t, filepath.Join(taskDir, filepath.FromSlash(remoteCatalogRel)), remoteCatalogFile, remoteCatalogFixture)
					} else {
						plantEmptyCodexCaches(t, taskDir, "")
					}
					before := codexCacheSnapshot(t, filepath.Join(taskDir, "codex-home"))
					action := d.shouldCleanTaskDir(context.Background(), taskDir)
					if action != wantAction {
						t.Fatalf("action = %d, want %d", action, wantAction)
					}
					stats := &gcStats{byPattern: map[string]int{}}
					d.applyGCAction(context.Background(), taskDir, action, stats)
					assertKept(t, taskDir, append(kept, ".gc_meta.json")...)
					if populated {
						if stats.artifactRemoved != 0 || stats.bytesReclaimed != 0 {
							t.Fatalf("populated cache removed: %+v", stats)
						}
						if after := codexCacheSnapshot(t, filepath.Join(taskDir, "codex-home")); !reflect.DeepEqual(before, after) {
							t.Fatal("populated cache changed")
						}
					} else {
						if stats.artifactRemoved != 2 || stats.bytesReclaimed != 0 {
							t.Fatalf("empty-root stats: %+v", stats)
						}
						for _, rel := range []string{marketplacesRel, remoteCatalogRel} {
							assertGone(t, taskDir, rel)
							if stats.byPattern[managedArtifactPatternPrefix+rel] != 1 {
								t.Fatalf("missing removal count for %s", rel)
							}
						}
					}
					// Without recreation, a second cycle makes no changes.
					stats = &gcStats{byPattern: map[string]int{}}
					d.applyGCAction(context.Background(), taskDir, action, stats)
					if stats.artifactRemoved != 0 || stats.bytesReclaimed != 0 {
						t.Fatalf("repeat cycle changed caches: %+v", stats)
					}
					if populated {
						// Replace disposable fixtures to model a later install.
						for _, rel := range []string{marketplacesRel, remoteCatalogRel} {
							if err := os.RemoveAll(filepath.Join(taskDir, filepath.FromSlash(rel))); err != nil {
								t.Fatal(err)
							}
						}
					}
				}
			})
		}
	}
}

// Install metadata and a clean checkout are not independent provenance.
// Sparse checkouts and fetched release tags must survive without index changes.
func TestManagedArtifact_PreservesCodexInstallShapesWithoutProof(t *testing.T) {
	t.Parallel()
	upstream := codexMarketplaceUpstream(t)
	runGitForGC(t, upstream, "branch", "stable")
	runGitForGC(t, upstream, "tag", "v1")
	runGitForGC(t, upstream, "checkout", "--quiet", "--detach")
	writeFile(t, filepath.Join(upstream, "release.md"), 8)
	runGitForGC(t, upstream, "add", "release.md")
	runGitForGC(t, upstream, "-c", "commit.gpgsign=false", "commit", "--quiet", "-m", "release")
	runGitForGC(t, upstream, "tag", "release")
	runGitForGC(t, upstream, "checkout", "--quiet", "main")
	runGitForGC(t, upstream, "config", "uploadpack.allowFilter", "true")

	for _, tc := range []struct {
		name    string
		install func(t *testing.T, root string)
	}{
		{"default branch", func(t *testing.T, root string) {
			installCodexMarketplaceAt(t, root, "alpha", upstream)
		}},
		{"branch ref", func(t *testing.T, root string) {
			clone := installCodexMarketplaceAt(t, root, "alpha", upstream)
			runGitForGC(t, clone, "checkout", "--quiet", "stable")
			recordCodexInstall(t, clone, upstream)
		}},
		{"tag ref", func(t *testing.T, root string) {
			clone := installCodexMarketplaceAt(t, root, "alpha", upstream)
			runGitForGC(t, clone, "checkout", "--quiet", "v1")
			recordCodexInstall(t, clone, upstream)
		}},
		{"tag ref no branch reaches", func(t *testing.T, root string) {
			clone := installCodexMarketplaceAt(t, root, "alpha", upstream)
			runGitForGC(t, clone, "checkout", "--quiet", "release")
			recordCodexInstall(t, clone, upstream)
		}},
		{"sparse partial clone", func(t *testing.T, root string) {
			clone := filepath.Join(root, "alpha")
			source := "file:///" + strings.TrimPrefix(filepath.ToSlash(upstream), "/")
			runGitForGC(t, "", "clone", "--quiet", "--filter=blob:none", "--no-checkout", source, clone)
			runGitForGC(t, clone, "sparse-checkout", "set", "plugins")
			runGitForGC(t, clone, "checkout", "--quiet", "HEAD")
			recordCodexInstall(t, clone, source)
		}},
		{"nothing installed yet", func(t *testing.T, root string) {
			if err := os.MkdirAll(filepath.Join(root, ".staging"), 0o755); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			root := filepath.Join(t.TempDir(), "marketplaces")
			tc.install(t, root)
			before := codexCacheSnapshot(t, root)
			if reason := managedArtifactKeepReason(context.Background(), marketplacesRel, root); !strings.Contains(reason, "no independent regenerability proof") {
				t.Fatalf("unproven install accepted: %q", reason)
			}
			if after := codexCacheSnapshot(t, root); !reflect.DeepEqual(before, after) {
				t.Fatal("inspection changed cache")
			}
		})
	}
}

// Known local-only content stays under the same conservative provenance rule.
func TestCodexMarketplacesKeepReason_KeepsLocalOnlyContent(t *testing.T) {
	t.Parallel()
	upstream := codexMarketplaceUpstream(t)
	commit := func(t *testing.T, clone, rel string) {
		t.Helper()
		writeFile(t, filepath.Join(clone, filepath.FromSlash(rel)), 9)
		runGitForGC(t, clone, "add", "--", rel)
		runGitForGC(t, clone, "-c", "commit.gpgsign=false", "commit", "--quiet", "-m", "local work")
	}
	writeRecord := func(t *testing.T, clone string, fields map[string]any) {
		t.Helper()
		data, err := json.Marshal(fields)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(clone, codexMarketplaceInstallRecord), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	for _, tc := range []struct {
		name   string
		mutate func(t *testing.T, root, clone string)
	}{
		{"modified tracked file", func(t *testing.T, root, clone string) {
			writeFile(t, filepath.Join(clone, "plugins", "alpha", "SKILL.md"), 7)
		}},
		{"untracked file", func(t *testing.T, root, clone string) {
			writeFile(t, filepath.Join(clone, "notes.md"), 8)
		}},
		{"ignored file", func(t *testing.T, root, clone string) {
			exclude, err := os.OpenFile(filepath.Join(clone, ".git", "info", "exclude"), os.O_APPEND|os.O_WRONLY, 0)
			if err != nil {
				t.Fatal(err)
			}
			_, err = exclude.WriteString("*.log\n")
			if closeErr := exclude.Close(); err == nil {
				err = closeErr
			}
			if err != nil {
				t.Fatal(err)
			}
			writeFile(t, filepath.Join(clone, "debug.log"), 8)
		}},
		{"staged change", func(t *testing.T, root, clone string) {
			writeFile(t, filepath.Join(clone, "plugins", "alpha", "draft.md"), 8)
			runGitForGC(t, clone, "add", "plugins/alpha/draft.md")
		}},
		{"untracked link outside the clone", func(t *testing.T, root, clone string) {
			if err := os.Symlink(t.TempDir(), filepath.Join(clone, "escape")); err != nil {
				t.Skipf("symlink not supported: %v", err)
			}
		}},
		{"commit on the installed branch", func(t *testing.T, root, clone string) {
			commit(t, clone, "local.md")
		}},
		{"commit reset away", func(t *testing.T, root, clone string) {
			commit(t, clone, "local.md")
			runGitForGC(t, clone, "reset", "--hard", "--quiet", "HEAD~1")
		}},
		{"tag on an unpushed commit", func(t *testing.T, root, clone string) {
			runGitForGC(t, clone, "checkout", "--quiet", "--detach")
			commit(t, clone, "local.md")
			runGitForGC(t, clone, "tag", "wip")
			runGitForGC(t, clone, "checkout", "--quiet", "main")
		}},
		{"stash", func(t *testing.T, root, clone string) {
			writeFile(t, filepath.Join(clone, "plugins", "alpha", "SKILL.md"), 7)
			runGitForGC(t, clone, "stash", "push", "--quiet")
		}},
		{"branch without upstream", func(t *testing.T, root, clone string) {
			runGitForGC(t, clone, "branch", "scratch")
		}},
		{"linked worktree", func(t *testing.T, root, clone string) {
			runGitForGC(t, clone, "worktree", "add", "--quiet", "--detach", filepath.Join(t.TempDir(), "wt"))
		}},
		{"initialized submodule", func(t *testing.T, root, clone string) {
			sub := t.TempDir()
			writeFile(t, filepath.Join(sub, "README.md"), 8)
			runGitForGC(t, sub, "init", "--quiet")
			runGitForGC(t, sub, "add", "README.md")
			runGitForGC(t, sub, "-c", "commit.gpgsign=false", "commit", "--quiet", "-m", "sub")
			runGitForGC(t, clone, "-c", "protocol.file.allow=always", "submodule", "add", "--quiet", sub, "vendor/sub")
		}},
		{"unfinished merge", func(t *testing.T, root, clone string) {
			writeFile(t, filepath.Join(clone, ".git", "MERGE_HEAD"), 41)
		}},
		{"metadata outside the clone", func(t *testing.T, root, clone string) {
			moved := filepath.Join(t.TempDir(), "gitdir")
			if err := os.Rename(filepath.Join(clone, ".git"), moved); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(clone, ".git"), []byte("gitdir: "+moved+"\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		}},
		{"worktree configured elsewhere", func(t *testing.T, root, clone string) {
			runGitForGC(t, clone, "config", "core.worktree", t.TempDir())
		}},
		{"missing install record", func(t *testing.T, root, clone string) {
			if err := os.Remove(filepath.Join(clone, codexMarketplaceInstallRecord)); err != nil {
				t.Fatal(err)
			}
		}},
		{"install record with a foreign field", func(t *testing.T, root, clone string) {
			writeRecord(t, clone, map[string]any{
				"source_type": "git", "source": upstream, "ref_name": nil, "sparse_paths": []string{},
				"revision": runGitForGC(t, clone, "rev-parse", "HEAD"), "pinned": true,
			})
		}},
		{"local marketplace source", func(t *testing.T, root, clone string) {
			writeRecord(t, clone, map[string]any{
				"source_type": "local", "source": upstream, "ref_name": nil, "sparse_paths": []string{},
				"revision": runGitForGC(t, clone, "rev-parse", "HEAD"),
			})
		}},
		{"unfinished install", func(t *testing.T, root, clone string) {
			writeFile(t, filepath.Join(root, ".staging", "marketplace-upgrade-x", "SKILL.md"), 8)
		}},
		{"backup left by a failed activation", func(t *testing.T, root, clone string) {
			writeFile(t, filepath.Join(root, "marketplace-backup-x", "root", "SKILL.md"), 8)
		}},
		{"stray file", func(t *testing.T, root, clone string) {
			writeFile(t, filepath.Join(root, "notes.txt"), 8)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			root := filepath.Join(t.TempDir(), "marketplaces")
			clone := installCodexMarketplaceAt(t, root, "alpha", upstream)
			tc.mutate(t, root, clone)
			if reason := managedArtifactKeepReason(context.Background(), marketplacesRel, root); !strings.Contains(reason, "no independent regenerability proof") {
				t.Fatalf("unproven local content accepted: %q", reason)
			}
		})
	}
}

func TestManagedArtifact_PreservesNestedMarketplaceRepositories(t *testing.T) {
	t.Parallel()
	for _, gitfile := range []bool{false, true} {
		t.Run(fmt.Sprintf("gitfile=%t", gitfile), func(t *testing.T) {
			taskDir := t.TempDir()
			root := filepath.Join(taskDir, filepath.FromSlash(marketplacesRel))
			nested := filepath.Join(root, "alpha", "vendor", ".git")
			if gitfile {
				writeFile(t, nested, 16)
			} else if err := os.MkdirAll(nested, 0o755); err != nil {
				t.Fatal(err)
			}
			before := codexCacheSnapshot(t, root)
			d := newGCTestDaemon(t, nil)
			if removed, _, _ := d.cleanTaskArtifacts(context.Background(), taskDir, nil); removed != 0 {
				t.Fatalf("removed = %d", removed)
			}
			if after := codexCacheSnapshot(t, root); !reflect.DeepEqual(before, after) {
				t.Fatal("nested repository changed")
			}
		})
	}
}

func TestCodexRemotePluginCatalogKeepReason(t *testing.T) {
	t.Parallel()
	const other = "0123456789abcdef.json"
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, dir string)
		empty bool
	}{
		{"Codex cache files", func(t *testing.T, dir string) {
			writeRemoteCatalog(t, dir, remoteCatalogFile, remoteCatalogFixture)
			writeRemoteCatalog(t, dir, other, `{"schema_version":1,"plugins":[]}`)
		}, false},
		{"empty", func(t *testing.T, dir string) {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
		}, true},
		{"credential file", func(t *testing.T, dir string) {
			writeRemoteCatalog(t, dir, "auth.json", `{"access_token":"secret"}`)
		}, false},
		{"unknown field", func(t *testing.T, dir string) {
			writeRemoteCatalog(t, dir, other, `{"schema_version":1,"plugins":[],"access_token":"secret"}`)
		}, false},
		{"not JSON", func(t *testing.T, dir string) {
			writeRemoteCatalog(t, dir, other, "plugins")
		}, false},
		{"trailing data", func(t *testing.T, dir string) {
			writeRemoteCatalog(t, dir, other, remoteCatalogFixture+"{}")
		}, false},
		{"missing plugins", func(t *testing.T, dir string) {
			writeRemoteCatalog(t, dir, other, `{"schema_version":1}`)
		}, false},
		{"plugin that is not an object", func(t *testing.T, dir string) {
			writeRemoteCatalog(t, dir, other, `{"schema_version":1,"plugins":["alpha"]}`)
		}, false},
		{"directory with a cache file name", func(t *testing.T, dir string) {
			if err := os.MkdirAll(filepath.Join(dir, other), 0o755); err != nil {
				t.Fatal(err)
			}
		}, false},
		{"link with a cache file name", func(t *testing.T, dir string) {
			target := writeRemoteCatalog(t, t.TempDir(), remoteCatalogFile, remoteCatalogFixture)
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, filepath.Join(dir, other)); err != nil {
				t.Skipf("symlink not supported: %v", err)
			}
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := filepath.Join(t.TempDir(), "remote_plugin_catalog")
			tc.setup(t, dir)
			reason := managedArtifactKeepReason(context.Background(), remoteCatalogRel, dir)
			if tc.empty && reason != "" {
				t.Fatalf("empty cache kept: %s", reason)
			}
			if !tc.empty && !strings.Contains(reason, "no independent regenerability proof") {
				t.Fatalf("nonempty cache accepted: %q", reason)
			}
		})
	}
}

// Every cycle reports why a populated cache stays and its logical byte size.
func TestManagedArtifact_RetainedCodexPluginCachesAreReportedEachCycle(t *testing.T) {
	t.Parallel()
	upstream := codexMarketplaceUpstream(t)
	chatID := "dddddddd-0000-0000-0000-000000000003"
	d := newGCTestDaemon(t, chatGCMux(chatID, "active"))
	var logs bytes.Buffer
	d.logger = slog.New(slog.NewJSONHandler(&logs, nil))

	taskDir := createTaskDir(t, d.cfg.WorkspacesRoot, "ws", "kept-caches", &execenv.GCMeta{
		Kind:          execenv.GCKindChat,
		ChatSessionID: chatID,
		WorkspaceID:   "ws",
		CompletedAt:   time.Now().Add(-24 * time.Hour),
	})
	clone := installCodexMarketplace(t, taskDir, "alpha", upstream)
	writeFile(t, filepath.Join(clone, "notes.md"), 8)
	catalogDir := filepath.Join(taskDir, filepath.FromSlash(remoteCatalogRel))
	writeRemoteCatalog(t, catalogDir, "auth.json", `{"access_token":"secret"}`)

	for cycle := 1; cycle <= 2; cycle++ {
		action := d.shouldCleanTaskDir(context.Background(), taskDir)
		if action != gcActionCleanManagedArtifacts {
			t.Fatalf("cycle %d: action = %d, want gcActionCleanManagedArtifacts", cycle, action)
		}
		stats := &gcStats{byPattern: map[string]int{}}
		d.applyGCAction(context.Background(), taskDir, action, stats)
		if stats.artifactRemoved != 0 || stats.bytesReclaimed != 0 {
			t.Fatalf("cycle %d removed a kept cache: %+v", cycle, stats)
		}
		assertKept(t, taskDir, marketplacesRel+"/alpha/notes.md", marketplacesRel+"/alpha/.git/HEAD", remoteCatalogRel+"/auth.json")
	}

	reasons := map[string]int{}
	scanner := bufio.NewScanner(&logs)
	for scanner.Scan() {
		var entry struct {
			Msg           string `json:"msg"`
			Path          string `json:"path"`
			Reason        string `json:"reason"`
			RetainedBytes int64  `json:"retained_bytes"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &entry); err != nil {
			t.Fatal(err)
		}
		if entry.Msg != "gc: managed artifact retained" {
			continue
		}
		if entry.RetainedBytes <= 0 {
			t.Fatalf("retained %s reported %d bytes", entry.Path, entry.RetainedBytes)
		}
		reasons[entry.Reason]++
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	want := map[string]int{"nonempty plugin cache has no independent regenerability proof": 4}
	if !reflect.DeepEqual(reasons, want) {
		t.Fatalf("retention reasons = %v, want %v", reasons, want)
	}
}

// The new paths sit behind the same gates as every managed artifact: an
// explicit GCArtifactTTL=0, a task still inside the TTL and a task running on
// the directory all keep them.
func TestManagedArtifact_CodexPluginCachesRespectGates(t *testing.T) {
	t.Parallel()
	chatID := "dddddddd-0000-0000-0000-000000000004"

	for _, tc := range []struct {
		name      string
		completed time.Duration
		ttl       time.Duration
		active    bool
	}{
		{"artifact TTL disabled", 365 * 24 * time.Hour, 0, false},
		{"inside the TTL", time.Hour, 12 * time.Hour, false},
		{"running task", 365 * 24 * time.Hour, 12 * time.Hour, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d := newGCTestDaemon(t, chatGCMux(chatID, "active"))
			d.cfg.GCArtifactTTL = tc.ttl
			taskDir := createTaskDir(t, d.cfg.WorkspacesRoot, "ws", "gated", &execenv.GCMeta{
				Kind:          execenv.GCKindChat,
				ChatSessionID: chatID,
				WorkspaceID:   "ws",
				CompletedAt:   time.Now().Add(-tc.completed),
			})
			plantEmptyCodexCaches(t, taskDir, "")
			if tc.active {
				d.markActiveEnvRoot(taskDir)
			}

			action := d.shouldCleanTaskDir(context.Background(), taskDir)
			if action != gcActionSkip {
				t.Fatalf("action = %d, want gcActionSkip", action)
			}
			d.applyGCAction(context.Background(), taskDir, action, &gcStats{byPattern: map[string]int{}})
			assertKept(t, taskDir, marketplacesRel, remoteCatalogRel)
		})
	}
}

// Plant only the empty roots that would be reached through linkRel. All
// candidates pass the content check, so retention must come from the tested
// path, ownership or lifecycle gate, not an unrelated populated cache.
func plantEmptyCodexCaches(t *testing.T, dir, linkRel string) []string {
	t.Helper()
	var planted []string
	for _, rel := range []string{marketplacesRel, remoteCatalogRel} {
		suffix := rel
		if linkRel != "" {
			if rel == linkRel {
				suffix = ""
			} else if strings.HasPrefix(rel, linkRel+"/") {
				suffix = strings.TrimPrefix(rel, linkRel+"/")
			} else {
				continue
			}
		}
		path := filepath.Join(dir, filepath.FromSlash(suffix))
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatal(err)
		}
		if reason := managedArtifactKeepReason(context.Background(), rel, path); reason != "" {
			t.Fatalf("positive control kept: %s", reason)
		}
		planted = append(planted, path)
	}
	if len(planted) == 0 {
		t.Fatalf("no candidate below %q", linkRel)
	}
	return planted
}

// Every component of the two Codex cache paths is checked: a link at any of
// them leads outside the task, and nothing behind it may be removed.
func TestManagedArtifact_CodexPluginCachesDoNotFollowLinks(t *testing.T) {
	t.Parallel()
	d := newGCTestDaemon(t, http.NewServeMux())

	for _, linkRel := range []string{"codex-home", "codex-home/.tmp", marketplacesRel, "codex-home/cache", remoteCatalogRel} {
		t.Run(linkRel, func(t *testing.T) {
			taskDir, outside := t.TempDir(), t.TempDir()
			planted := plantEmptyCodexCaches(t, outside, linkRel)
			linkPath := filepath.Join(taskDir, filepath.FromSlash(linkRel))
			if err := os.MkdirAll(filepath.Dir(linkPath), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(outside, linkPath); err != nil {
				t.Skipf("symlink not supported: %v", err)
			}

			direct, _, _ := d.cleanManagedTaskArtifacts(context.Background(), taskDir)
			walked, _, _ := d.cleanTaskArtifacts(context.Background(), taskDir, nil)
			if direct != 0 || walked != 0 {
				t.Fatalf("removed direct=%d walked=%d through a link, want 0", direct, walked)
			}
			for _, path := range planted {
				if _, err := os.Stat(path); err != nil {
					t.Fatalf("link target was touched: %v", err)
				}
			}
		})
	}
}
