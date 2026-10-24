package execenv

import "path/filepath"

const codexHomeDirName = "codex-home"

// Exact managed artifact paths below a task env root, spelled with forward
// slashes. Compare them with filepath.ToSlash of a cleaned relative path.
const (
	// CodexSandboxBinArtifact is Codex's own copy of its binary, provisioned
	// again on demand.
	CodexSandboxBinArtifact = codexHomeDirName + "/.sandbox-bin"
	// CodexMarketplacesArtifact holds Codex's clones of the Git marketplaces
	// configured in config.toml (INSTALLED_MARKETPLACES_DIR in codex-rs). Codex
	// clones a missing one again when it starts.
	CodexMarketplacesArtifact = codexHomeDirName + "/.tmp/marketplaces"
	// CodexRemotePluginCatalogArtifact is Codex's disk cache of the remote
	// plugin catalog (REMOTE_PLUGIN_CATALOG_DISK_CACHE_DIR in codex-rs),
	// fetched again once missing or older than three hours.
	CodexRemotePluginCatalogArtifact = codexHomeDirName + "/cache/remote_plugin_catalog"
)

// ManagedReclaimableArtifactSubpaths returns artifact candidates inside a
// task env root. Callers must match these as exact relative paths rather than
// basenames: a repository may legitimately contain a directory with the same
// leaf name. Only the sandbox copy is reclaimable on sight; the two plugin
// caches lack independent provenance and can currently be removed only empty.
func ManagedReclaimableArtifactSubpaths() []string {
	return []string{
		filepath.FromSlash(CodexSandboxBinArtifact),
		filepath.FromSlash(CodexMarketplacesArtifact),
		filepath.FromSlash(CodexRemotePluginCatalogArtifact),
	}
}
