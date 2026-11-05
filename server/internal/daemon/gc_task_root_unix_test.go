//go:build !windows

package daemon

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/multica-ai/multica/server/internal/daemon/execenv"
)

func TestWholeTaskGCInspectionErrorPreservesRoot(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses permission checks")
	}
	d := newGCTestDaemon(t, http.NewServeMux())
	root := createTaskDir(t, d.cfg.WorkspacesRoot, "ws1", "task1", nil)
	if err := os.Chmod(root, 0); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(root, 0700)
	if _, removed := d.cleanTaskDir(context.Background(), root, gcActionClean, &gcStats{}); removed {
		t.Fatal("unreadable root removed")
	}
}

func TestWholeTaskGCRemovalErrorRestoresMetadata(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses permission checks")
	}
	d := newGCTestDaemon(t, http.NewServeMux())
	root := createTaskDir(t, d.cfg.WorkspacesRoot, "ws1", "task1", &execenv.GCMeta{WorkspaceID: "ws1"})
	before := map[string]string{}
	for _, name := range []string{".task_owner", ".gc_meta.json"} {
		data, err := os.ReadFile(filepath.Join(root, name))
		if err != nil {
			t.Fatal(err)
		}
		before[name] = string(data)
	}
	// Metadata is writable in the task; only removing the task from its parent fails.
	if err := os.Chmod(filepath.Dir(root), 0500); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(filepath.Dir(root), 0700)
	if n, removed := d.cleanTaskDir(context.Background(), root, gcActionClean, &gcStats{}); removed || n != 0 {
		t.Fatalf("failed removal counted: %d %v", n, removed)
	}
	for name, want := range before {
		data, err := os.ReadFile(filepath.Join(root, name))
		if err != nil || string(data) != want {
			t.Fatalf("control %s lost: %q %v", name, data, err)
		}
	}
}
