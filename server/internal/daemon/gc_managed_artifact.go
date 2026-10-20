package daemon

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/multica-ai/multica/server/internal/daemon/execenv"
	"github.com/multica-ai/multica/server/internal/daemon/processtree"
)

// Names Codex gives its plugin cache contents (codex-rs/core-plugins).
const (
	// codexMarketplaceInstallRecord is the record Codex writes into every Git
	// marketplace clone it activates. A clean install adds no other file.
	codexMarketplaceInstallRecord = ".codex-marketplace-install.json"
	// codexMarketplaceStagingDir holds clones Codex has not activated yet.
	codexMarketplaceStagingDir = ".staging"
	// maxCodexMarketplaceInstallRecordBytes bounds the read of a record Codex
	// writes as a few hundred bytes of JSON.
	maxCodexMarketplaceInstallRecordBytes = 64 << 10
)

var (
	// codexRemotePluginCatalogFileName matches a catalog cache entry: the
	// 64-bit FNV-1a hash of its cache key in lowercase hex.
	codexRemotePluginCatalogFileName = regexp.MustCompile(`^[0-9a-f]{16}\.json$`)
	gitObjectName                    = regexp.MustCompile(`^(?:[0-9a-f]{40}|[0-9a-f]{64})$`)
)

// gitUnfinishedOperationPaths mark a merge, rebase, cherry-pick, revert or
// bisect that is still in progress. Their state lives only in these files.
var gitUnfinishedOperationPaths = []string{
	"MERGE_HEAD", "CHERRY_PICK_HEAD", "REVERT_HEAD", "REBASE_HEAD", "BISECT_LOG",
	"rebase-merge", "rebase-apply", "sequencer",
}

// codexMarketplaceInstall mirrors InstalledMarketplaceMetadata in codex-rs.
type codexMarketplaceInstall struct {
	SourceType  string   `json:"source_type"`
	Source      string   `json:"source"`
	RefName     *string  `json:"ref_name"`
	SparsePaths []string `json:"sparse_paths"`
	Revision    string   `json:"revision"`
}

// managedArtifactKeepReason reports why the exact managed artifact directory
// dir, found at rel below the task root, must be kept; "" means it may be
// removed. The sandbox copy holds only Codex's binary. The plugin caches can
// also hold whatever a person or a plugin put there, so they may go only when
// their content is exactly what Codex writes again. A failed inspection is a
// reason too: a directory that cannot be proven regenerable always stays.
func managedArtifactKeepReason(ctx context.Context, rel, dir string) string {
	switch filepath.ToSlash(rel) {
	case execenv.CodexSandboxBinArtifact:
		return ""
	case execenv.CodexMarketplacesArtifact:
		return codexMarketplacesKeepReason(ctx, dir)
	case execenv.CodexRemotePluginCatalogArtifact:
		return codexRemotePluginCatalogKeepReason(ctx, dir)
	default:
		return "no regenerability contract for this managed path"
	}
}

// codexMarketplacesKeepReason accepts Codex's marketplace install root only
// when every entry is a clean marketplace clone, apart from an empty staging
// directory. A staged clone or a backup left by a failed activation is not
// something Codex creates again, so it keeps the whole directory.
func codexMarketplacesKeepReason(ctx context.Context, dir string) string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Sprintf("list marketplaces: %v", err)
	}
	for _, entry := range entries {
		name := entry.Name()
		path := filepath.Join(dir, name)
		info, err := os.Lstat(path)
		if err != nil {
			return fmt.Sprintf("inspect %s: %v", name, err)
		}
		if info.Mode()&linkedDirModes != 0 || !info.IsDir() {
			return fmt.Sprintf("%s is not a marketplace clone directory", name)
		}
		if name == codexMarketplaceStagingDir {
			staged, err := os.ReadDir(path)
			if err != nil {
				return fmt.Sprintf("list %s: %v", name, err)
			}
			if len(staged) > 0 {
				return fmt.Sprintf("%s holds an unfinished marketplace install", name)
			}
			continue
		}
		if reason := codexMarketplaceCloneKeepReason(ctx, path); reason != "" {
			return name + ": " + reason
		}
	}
	return ""
}

// codexMarketplaceCloneKeepReason accepts root only when it is the checkout
// Codex activated and nothing in it exists solely on this machine:
//   - Codex's install record names a Git source and the revision HEAD is on;
//   - the clone owns its Git metadata, no other checkout depends on it, and no
//     Git operation is unfinished;
//   - git status reports nothing but the install record: no modified, staged,
//     untracked or ignored file;
//   - every local branch equals its upstream, no ref lives outside heads,
//     remotes and tags, and every commit HEAD or a reflog reaches is on a
//     remote-tracking ref or below the installed revision, so stashes and
//     local commits, including ones reset away or only tagged, keep the clone;
//   - the checkout holds no nested repository, junction or special file.
//
// A symlink the checkout tracks is fine: removing a link never touches its
// target, and git status reports one that is untracked or changed.
func codexMarketplaceCloneKeepReason(ctx context.Context, root string) string {
	install, reason := readCodexMarketplaceInstall(root)
	if reason != "" {
		return reason
	}
	gitDir := filepath.Join(root, ".git")
	info, err := os.Lstat(gitDir)
	if err != nil {
		return fmt.Sprintf("inspect Git metadata: %v", err)
	}
	if info.Mode()&linkedDirModes != 0 || !info.IsDir() {
		return "Git metadata lives outside the clone"
	}
	for _, name := range []string{"worktrees", "modules"} {
		entries, err := os.ReadDir(filepath.Join(gitDir, name))
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Sprintf("inspect .git/%s: %v", name, err)
		}
		if len(entries) > 0 {
			return fmt.Sprintf("other checkouts depend on .git/%s", name)
		}
	}
	for _, name := range gitUnfinishedOperationPaths {
		if _, err := os.Lstat(filepath.Join(gitDir, name)); err == nil {
			return fmt.Sprintf("an unfinished Git operation left .git/%s", name)
		} else if !errors.Is(err, fs.ErrNotExist) {
			return fmt.Sprintf("inspect .git/%s: %v", name, err)
		}
	}
	if reason := codexMarketplaceGitKeepReason(ctx, root, gitDir, install); reason != "" {
		return reason
	}
	return codexMarketplaceTreeKeepReason(ctx, root, gitDir)
}

func readCodexMarketplaceInstall(root string) (codexMarketplaceInstall, string) {
	var install codexMarketplaceInstall
	path := filepath.Join(root, codexMarketplaceInstallRecord)
	info, err := os.Lstat(path)
	if err != nil {
		return install, fmt.Sprintf("no Codex install record: %v", err)
	}
	if !info.Mode().IsRegular() || info.Size() > maxCodexMarketplaceInstallRecordBytes {
		return install, "Codex install record is not a small regular file"
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return install, fmt.Sprintf("read Codex install record: %v", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&install); err != nil {
		return install, fmt.Sprintf("Codex install record is not Codex's format: %v", err)
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return install, "Codex install record has trailing data"
	}
	if install.SourceType != "git" || strings.TrimSpace(install.Source) == "" || !gitObjectName.MatchString(install.Revision) {
		return install, "Codex install record does not name a Git source and revision"
	}
	return install, ""
}

func codexMarketplaceGitKeepReason(ctx context.Context, root, gitDir string, install codexMarketplaceInstall) string {
	// A core.worktree setting would make every query below describe another
	// tree, so first prove Git resolves this clone to its own metadata.
	layout, err := runCodexMarketplaceGit(ctx, root, "rev-parse", "--show-toplevel", "--absolute-git-dir")
	if err != nil {
		return err.Error()
	}
	paths := strings.FieldsFunc(layout, isLineBreak)
	if len(paths) != 2 || !sameExistingPath(paths[0], root) || !sameExistingPath(paths[1], gitDir) {
		return "Git does not resolve the clone to its own metadata"
	}

	head, err := runCodexMarketplaceGit(ctx, root, "rev-parse", "--verify", "HEAD^{commit}")
	if err != nil {
		return err.Error()
	}
	if strings.TrimSpace(head) != install.Revision {
		return "HEAD is not the revision Codex installed"
	}

	status, err := runCodexMarketplaceGit(ctx, root, "status", "--porcelain=v1", "-z", "--untracked-files=all", "--ignored=matching", "--no-renames")
	if err != nil {
		return err.Error()
	}
	for _, entry := range strings.Split(status, "\x00") {
		if entry == "" || entry == "?? "+codexMarketplaceInstallRecord || entry == "!! "+codexMarketplaceInstallRecord {
			continue
		}
		return fmt.Sprintf("uncommitted changes: %q", entry)
	}

	refs, err := runCodexMarketplaceGit(ctx, root, "for-each-ref", "--format=%(refname)%00%(objectname)%00%(upstream)")
	if err != nil {
		return err.Error()
	}
	type localBranch struct{ name, object, upstream string }
	var branches []localBranch
	remoteTracking := map[string]string{}
	for _, line := range strings.FieldsFunc(refs, isLineBreak) {
		fields := strings.Split(line, "\x00")
		if len(fields) != 3 {
			return fmt.Sprintf("unreadable ref listing: %q", line)
		}
		name, object, upstream := fields[0], fields[1], fields[2]
		switch {
		case strings.HasPrefix(name, "refs/remotes/"):
			remoteTracking[name] = object
		case strings.HasPrefix(name, "refs/heads/"):
			branches = append(branches, localBranch{name, object, upstream})
		case strings.HasPrefix(name, "refs/tags/"):
			// The clone fetches every tag of the source, including tags on
			// commits no branch reaches, so a tag is upstream data. A commit
			// made here would show up in HEAD or a reflog, checked below.
		default:
			return fmt.Sprintf("local ref %s", name)
		}
	}
	for _, branch := range branches {
		if branch.upstream == "" {
			return fmt.Sprintf("local branch %s has no upstream", branch.name)
		}
		if remoteTracking[branch.upstream] != branch.object {
			return fmt.Sprintf("local branch %s differs from %s", branch.name, branch.upstream)
		}
	}

	// Committing, resetting or checking out here moves HEAD or a branch, and
	// the reflog keeps every commit it moved to. Codex's own checkout of a ref
	// lands on the recorded revision, which may be a tag no branch reaches.
	localOnly, err := runCodexMarketplaceGit(ctx, root, "rev-list", "--count", "HEAD", "--reflog", "--not", "--remotes", install.Revision)
	if err != nil {
		return err.Error()
	}
	if count := strings.TrimSpace(localOnly); count != "0" {
		return fmt.Sprintf("%s commits exist only in this clone", count)
	}
	return ""
}

// codexMarketplaceTreeKeepReason walks the checkout outside .git. Git cannot
// record a nested repository, a junction or a special file, so a fresh clone
// would not bring any of them back.
func codexMarketplaceTreeKeepReason(ctx context.Context, root, gitDir string) string {
	reason := ""
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if ctx.Err() != nil {
			return context.Cause(ctx)
		}
		if err != nil {
			return err
		}
		if path == root {
			return nil
		}
		if path == gitDir {
			return filepath.SkipDir
		}
		rel, _ := filepath.Rel(root, path)
		if entry.Name() == ".git" {
			reason = fmt.Sprintf("nested Git repository at %s", filepath.ToSlash(rel))
			return filepath.SkipAll
		}
		mode := entry.Type()
		if entry.IsDir() {
			info, err := os.Lstat(path)
			if err != nil {
				return err
			}
			mode = info.Mode()
		}
		if mode&os.ModeSymlink == 0 && mode&(os.ModeIrregular|os.ModeNamedPipe|os.ModeSocket|os.ModeDevice) != 0 {
			reason = fmt.Sprintf("%s is a junction or special file", filepath.ToSlash(rel))
			return filepath.SkipAll
		}
		return nil
	})
	if err != nil {
		return fmt.Sprintf("inspect checkout: %v", err)
	}
	return reason
}

// runCodexMarketplaceGit runs a read-only Git query against a marketplace
// clone. Inherited GIT_* variables are dropped so they cannot point it at
// another repository or index. Optional locks stay off so a clone that is kept
// is not written to, a configured fsmonitor hook is not started, and a partial
// clone does not fetch.
func runCodexMarketplaceGit(ctx context.Context, root string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, gitCmdTimeout)
	defer cancel()
	env := make([]string, 0, len(os.Environ())+3)
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(strings.ToUpper(entry), "GIT_") {
			env = append(env, entry)
		}
	}
	env = append(env, "GIT_OPTIONAL_LOCKS=0", "GIT_TERMINAL_PROMPT=0", "GIT_NO_LAZY_FETCH=1")
	var out []byte
	var err error
	for attempt := 0; attempt < 2; attempt++ {
		cmd := exec.Command("git", append([]string{"--no-optional-locks", "-c", "core.fsmonitor=false", "-C", root}, args...)...)
		cmd.Env = env
		out, err = processtree.Output(ctx, cmd, 5*time.Second)
		// Behind Apple's /usr/bin/git trampoline, stopping the process group
		// of a query that already exited now and then fails with EPERM. The
		// query only reads, so one more attempt is safe.
		if err == nil || !errors.Is(err, syscall.EPERM) {
			break
		}
	}
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && len(exitErr.Stderr) > 0 {
			return "", fmt.Errorf("git %s: %w: %s", args[0], err, strings.TrimSpace(string(exitErr.Stderr)))
		}
		return "", fmt.Errorf("git %s: %w", args[0], err)
	}
	return string(out), nil
}

// isLineBreak splits Git's line output whether lines end in LF or CRLF.
func isLineBreak(r rune) bool { return r == '\n' || r == '\r' }

// sameExistingPath reports whether two spellings name the same existing file.
// Git prints resolved paths, which differ from the GC's on a symlinked
// temporary directory or in letter case on Windows.
func sameExistingPath(a, b string) bool {
	infoA, errA := os.Stat(a)
	infoB, errB := os.Stat(b)
	return errA == nil && errB == nil && os.SameFile(infoA, infoB)
}

// codexRemotePluginCatalogKeepReason accepts the catalog cache only when
// every entry is a regular file with Codex's name and exactly its schema: a
// schema version, a fetch time and a list of plugin objects. Codex fetches a
// missing file again, and that schema has no field for a credential.
func codexRemotePluginCatalogKeepReason(ctx context.Context, dir string) string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Sprintf("list catalog cache: %v", err)
	}
	for _, entry := range entries {
		if ctx.Err() != nil {
			return fmt.Sprintf("inspect catalog cache: %v", context.Cause(ctx))
		}
		name := entry.Name()
		if !codexRemotePluginCatalogFileName.MatchString(name) {
			return fmt.Sprintf("%s is not a Codex catalog cache file", name)
		}
		path := filepath.Join(dir, name)
		info, err := os.Lstat(path)
		if err != nil {
			return fmt.Sprintf("inspect %s: %v", name, err)
		}
		if !info.Mode().IsRegular() {
			return fmt.Sprintf("%s is not a regular file", name)
		}
		if err := checkCodexRemotePluginCatalogFile(path); err != nil {
			return fmt.Sprintf("%s is not Codex's catalog format: %v", name, err)
		}
	}
	return ""
}

// checkCodexRemotePluginCatalogFile streams the file so a catalog of tens of
// megabytes is checked without holding it in memory.
func checkCodexRemotePluginCatalogFile(path string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	decoder := json.NewDecoder(bufio.NewReader(file))
	if err := expectJSONDelim(decoder, '{'); err != nil {
		return err
	}
	seen := map[string]bool{}
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return err
		}
		field, _ := token.(string)
		if seen[field] {
			return fmt.Errorf("duplicate field %q", field)
		}
		seen[field] = true
		switch field {
		case "schema_version":
			var version uint8
			if err := decoder.Decode(&version); err != nil {
				return fmt.Errorf("schema_version: %w", err)
			}
		case "fetched_at":
			var fetchedAt *string
			if err := decoder.Decode(&fetchedAt); err != nil {
				return fmt.Errorf("fetched_at: %w", err)
			}
		case "plugins":
			if err := expectJSONDelim(decoder, '['); err != nil {
				return fmt.Errorf("plugins: %w", err)
			}
			for decoder.More() {
				var plugin map[string]json.RawMessage
				if err := decoder.Decode(&plugin); err != nil {
					return fmt.Errorf("plugins: %w", err)
				}
			}
			if err := expectJSONDelim(decoder, ']'); err != nil {
				return fmt.Errorf("plugins: %w", err)
			}
		default:
			return fmt.Errorf("unexpected field %q", field)
		}
	}
	if err := expectJSONDelim(decoder, '}'); err != nil {
		return err
	}
	if !seen["schema_version"] || !seen["plugins"] {
		return errors.New("schema_version or plugins is missing")
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return errors.New("trailing data")
	}
	return nil
}

func expectJSONDelim(decoder *json.Decoder, want json.Delim) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	if token != want {
		return fmt.Errorf("want %v, got %v", want, token)
	}
	return nil
}
