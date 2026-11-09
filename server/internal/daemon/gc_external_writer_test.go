package daemon

import (
	"context"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/daemon/execenv"
)

// A reservation coordinates this daemon, not arbitrary same-user processes.
// Replacement before revalidation is detected; this does not test or close the
// non-atomic gap between the last revalidation and unlink.
func TestGCReservationDoesNotExcludeExternalWriter(t *testing.T) {
	if os.Getenv("MULTICA_TEST_GC_EXTERNAL_WRITER") == "1" {
		root := os.Getenv("MULTICA_TEST_GC_EXTERNAL_ROOT")
		if err := execenv.WriteGCMeta(root, execenv.GCMeta{
			Kind: execenv.GCKindIssue, WorkspaceID: "ws1", TaskID: "task1",
			CompletedAt: time.Unix(123456789, 0),
		}, nil); err != nil {
			t.Fatal(err)
		}
		return
	}
	d := newGCTestDaemon(t, http.NewServeMux())
	root := createTaskDir(t, d.cfg.WorkspacesRoot, "ws1", "task1", &execenv.GCMeta{WorkspaceID: "ws1"})
	release, ok := d.reserveEnvRootForGC(root)
	if !ok {
		t.Fatal("could not reserve fixture root")
	}
	defer release()
	plan, err := inspectTaskRoot(d.cfg.WorkspacesRoot, root)
	if err != nil {
		t.Fatal(err)
	}
	defer plan.close()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, exe, "-test.run=^TestGCReservationDoesNotExcludeExternalWriter$")
	cmd.Env = append(os.Environ(), "MULTICA_TEST_GC_EXTERNAL_WRITER=1", "MULTICA_TEST_GC_EXTERNAL_ROOT="+root)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("external fixture writer: %v\n%s", err, output)
	}
	if !cmd.ProcessState.Exited() {
		t.Fatal("fixture writer was not reaped")
	}
	metaPath := filepath.Join(root, ".gc_meta.json")
	replacement, err := os.ReadFile(metaPath)
	if err != nil {
		t.Fatal(err)
	}
	if n, err := plan.remove(); err == nil || n != 0 {
		t.Fatalf("pre-validation replacement accepted: bytes=%d err=%v", n, err)
	}
	kept, err := os.ReadFile(metaPath)
	if err != nil || string(kept) != string(replacement) {
		t.Fatalf("replacement was not retained: %v", err)
	}
	t.Logf("external writer pid=%d exited under reservation; replacement detected and retained", cmd.ProcessState.Pid())
}
