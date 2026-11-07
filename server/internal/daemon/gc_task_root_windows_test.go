package daemon

import (
	"context"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestWholeTaskGCRejectsWindowsJunction(t *testing.T) {
	d := newGCTestDaemon(t, http.NewServeMux())
	root := createTaskDir(t, d.cfg.WorkspacesRoot, "ws1", "task1", nil)
	target := t.TempDir()
	writeFile(t, filepath.Join(target, "result.md"), 41)
	link := filepath.Join(root, "workdir")
	if output, err := exec.Command("cmd", "/c", "mklink", "/J", link, target).CombinedOutput(); err != nil {
		t.Skipf("junction creation unavailable: %v %s", err, output)
	}
	defer os.Remove(link)
	if _, removed := d.cleanTaskDir(context.Background(), root, gcActionClean, &gcStats{}); removed {
		t.Fatal("junction-containing root removed")
	}
	if info, err := os.Stat(filepath.Join(target, "result.md")); err != nil || info.Size() != 41 {
		t.Fatalf("junction target changed: %v", err)
	}
}
