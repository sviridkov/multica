package daemon

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/multica-ai/multica/server/internal/daemon/execenv"
)

// managedArtifactKeepReason returns an empty string only when this artifact
// may be removed. Plugin caches are writable by tasks: Git status, install
// records and catalog schemas cannot prove their contents are reproducible.
// Without independent producer evidence, even an apparently clean cache stays.
// The caller verifies path components and holds the environment's GC lock.
func managedArtifactKeepReason(ctx context.Context, rel, dir string) string {
	switch filepath.ToSlash(rel) {
	case execenv.CodexSandboxBinArtifact:
		return ""
	case execenv.CodexMarketplacesArtifact, execenv.CodexRemotePluginCatalogArtifact:
		if ctx.Err() != nil {
			return fmt.Sprintf("inspect plugin cache: %v", context.Cause(ctx))
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			return fmt.Sprintf("list plugin cache: %v", err)
		}
		if len(entries) != 0 {
			return "nonempty plugin cache has no independent regenerability proof"
		}
		return ""
	default:
		return "no regenerability contract for this managed path"
	}
}

func removeManagedArtifact(rel, dir string) error {
	switch filepath.ToSlash(rel) {
	case execenv.CodexSandboxBinArtifact:
		return os.RemoveAll(dir)
	case execenv.CodexMarketplacesArtifact, execenv.CodexRemotePluginCatalogArtifact:
		// Never recurse: a writer may have populated the empty directory
		// since inspection, in which case removal must fail and retain it.
		return os.Remove(dir)
	default:
		return fmt.Errorf("no removal contract for managed path %q", rel)
	}
}
