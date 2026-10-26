//go:build cachebrokerlab

package daemon

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/internal/daemon/cachebrokerlab"
	"github.com/multica-ai/multica/server/internal/daemon/execenv"
)

func labReclaim(t *testing.T, root, route string, b *cachebrokerlab.Broker) (int, int64) {
	t.Helper()
	d := newGCTestDaemon(t, http.NewServeMux())
	reclaim := func(ctx context.Context, rel string) (cachebrokerlab.Outcome, error) {
		out, err := b.Reclaim(ctx, b.Receipt(rel))
		if err != nil {
			t.Fatalf("reclaim: %v", err)
		}
		t.Logf("RECLAIM artifact=%s removed=%t bytes=%d reason=%s", rel, out.Removed, out.Bytes, out.Reason)
		return out, err
	}
	if route == "direct" {
		n, size, _ := d.cleanManagedTaskArtifactsWithBroker(context.Background(), root, removeManagedArtifact, reclaim)
		return n, size
	}
	matcher := newArtifactMatcher(nil, execenv.ManagedReclaimableArtifactSubpaths())
	n, size, _ := d.cleanTaskArtifactsMatchingWithBroker(context.Background(), root, matcher, removeManagedArtifact, reclaim)
	return n, size
}

func TestBrokerNativeNegatives(t *testing.T) {
	cfg := mustLab(t)
	for _, route := range []string{"direct", "walk"} {
		for _, artifact := range []string{cachebrokerlab.Marketplaces, cachebrokerlab.Catalog} {
			for _, mutation := range []string{"content", "metadata", "credentials", "nested-git", "local-ref", "local-commit", "stash", "hardlink", "symlink", "extra-metadata", "directory-link", "widen-permissions"} {
				if artifact == cachebrokerlab.Catalog && (mutation == "local-ref" || mutation == "local-commit" || mutation == "stash") {
					continue
				}
				t.Run(route+"/"+artifact+"/"+mutation, func(t *testing.T) {
					root, b := labBroker(t, cfg)
					r, err := b.Materialize(context.Background(), artifact)
					if err != nil {
						t.Fatal(err)
					}
					target := filepath.Join(root, filepath.FromSlash(artifact))
					var file string
					for _, entry := range r.Entries {
						if entry.Digest != "" && !strings.Contains(entry.Path, ".git/") {
							file = filepath.Join(target, filepath.FromSlash(entry.Path))
							break
						}
					}
					if file == "" {
						t.Fatal("no producer file")
					}
					var preserved string
					switch mutation {
					case "content":
						preserved = file
						err = os.WriteFile(file, []byte("locally changed producer content"), 0644)
					case "metadata":
						err = os.Chmod(file, 0444)
					case "extra-metadata":
						err = nativeExtraMetadata(file)
					case "widen-permissions":
						err = nativeWidenPermissions(file)
					case "directory-link":
						preserved = filepath.Join(root, "external-dir", "result")
						labWrite(t, preserved, "external directory result")
						err = nativeDirectoryLink(filepath.Join(target, "external-dir-link"), filepath.Dir(preserved))
					case "credentials":
						preserved = filepath.Join(target, "auth.json")
						labWrite(t, preserved, "fixture credential must survive")
					case "nested-git":
						preserved = filepath.Join(target, "local-project", ".git", "HEAD")
						labWrite(t, preserved, "ref: refs/heads/local\n")
					case "local-ref", "local-commit", "stash":
						gitRoot := filepath.Join(target, "cache-lab")
						if mutation == "local-ref" {
							labGit(t, cfg, gitRoot, "branch", "local-work")
						} else {
							preserved = filepath.Join(gitRoot, "plugins/sample/marker.txt")
							labWrite(t, preserved, "unpublished local work")
							if mutation == "stash" {
								labGit(t, cfg, gitRoot, "-c", "user.name=Cache Lab", "-c", "user.email=cache-lab@example.invalid", "stash", "push", "-m", "local work")
							} else {
								labGit(t, cfg, gitRoot, "add", ".")
								labGit(t, cfg, gitRoot, "-c", "user.name=Cache Lab", "-c", "user.email=cache-lab@example.invalid", "commit", "-m", "unpublished work")
							}
						}
					case "hardlink":
						err = os.Link(file, filepath.Join(target, "second-link"))
					case "symlink":
						preserved = filepath.Join(root, "external-result")
						labWrite(t, preserved, "external result")
						err = os.Symlink(preserved, filepath.Join(target, "external-link"))
					}
					if err != nil {
						t.Fatal(err)
					}
					before := labTreeSnapshot(t, target)
					var content []byte
					if preserved != "" {
						content, err = os.ReadFile(preserved)
						if err != nil {
							t.Fatal(err)
						}
					}
					if n, size := labReclaim(t, root, route, b); n != 0 || size != 0 {
						t.Fatalf("removed changed cache: %d %d", n, size)
					}
					if after := labTreeSnapshot(t, target); before != after {
						t.Fatal("retained tree changed")
					}
					if preserved != "" {
						after, err := os.ReadFile(preserved)
						if err != nil || !bytes.Equal(after, content) {
							t.Fatal("local work or external content changed")
						}
					}
				})
			}
		}
	}
}

// Snapshot is independent of the broker's inventory and includes links as links.
func labTreeSnapshot(t *testing.T, root string) string {
	t.Helper()
	var out strings.Builder
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		fmt.Fprintf(&out, "%s:%s:%d\n", path, info.Mode(), info.ModTime().UnixNano())
		if info.Mode().IsRegular() {
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			out.Write(data)
		} else if info.Mode()&os.ModeSymlink != 0 {
			target, err := os.Readlink(path)
			if err != nil {
				return err
			}
			out.WriteString(target)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out.String()
}

func TestBrokerNativeCatalogWriteFailure(t *testing.T) {
	cfg := mustLab(t)
	root, _ := labBroker(t, cfg)
	labWrite(t, filepath.Join(root, "failed-home", "cache"), "file blocks cache publication")
	evidence := filepath.Join(root, "failed-acquisition.json")
	cmd := exec.Command(cfg.Producer, "--exact", "remote::broker_lab::produce", "--ignored", "--nocapture")
	cmd.Dir = root
	cmd.Env = append(nativeBaseEnv(), "MULTICA_CACHE_LAB_ALLOW=1", "MULTICA_CACHE_LAB_HOME="+filepath.Join(root, "failed-home"), "MULTICA_CACHE_LAB_KIND=catalog-write-error", "MULTICA_CACHE_LAB_EVIDENCE="+evidence)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	data, err := os.ReadFile(evidence)
	if err != nil {
		t.Fatal(err)
	}
	var source cachebrokerlab.SourceIdentity
	if err := json.Unmarshal(data, &source); err != nil {
		t.Fatal(err)
	}
	if source.Written || source.ReadVerified {
		t.Fatal("failed write reported committed")
	}
	t.Log("actual catalog fetch succeeded, durable-write and readback=false")
}

func TestBrokerNativeLifecycleAndJournal(t *testing.T) {
	cfg := mustLab(t)
	guards := map[string]func(*cachebrokerlab.Policy){
		"ttl-disabled":    func(p *cachebrokerlab.Policy) { p.ArtifactTTLSeconds = 0 },
		"too-recent":      func(p *cachebrokerlab.Policy) { p.IdleSeconds = 0 },
		"active":          func(p *cachebrokerlab.Policy) { p.Active = true },
		"nonterminal":     func(p *cachebrokerlab.Policy) { p.Terminal = false },
		"local-directory": func(p *cachebrokerlab.Policy) { p.LocalDirectory = true },
	}
	for _, route := range []string{"direct", "walk"} {
		for name, guard := range guards {
			t.Run(route+"/"+name, func(t *testing.T) {
				root, b := labBroker(t, cfg, guard)
				if _, err := b.Materialize(context.Background(), cachebrokerlab.Catalog); err != nil {
					t.Fatal(err)
				}
				before := labTreeSnapshot(t, filepath.Join(root, filepath.FromSlash(cachebrokerlab.Catalog)))
				if n, size := labReclaim(t, root, route, b); n != 0 || size != 0 {
					t.Fatal("lifecycle guard bypassed")
				}
				if before != labTreeSnapshot(t, filepath.Join(root, filepath.FromSlash(cachebrokerlab.Catalog))) {
					t.Fatal("ineligible cache changed")
				}
			})
		}
	}
	for _, fault := range []string{"stale-lease", "corrupt-journal", "failed-producer"} {
		t.Run(fault, func(t *testing.T) {
			var policy cachebrokerlab.Policy
			root, b := labBroker(t, cfg, func(p *cachebrokerlab.Policy) {
				if fault == "failed-producer" {
					p.SourceOID = strings.Repeat("0", 40)
				}
				policy = *p
			})
			if fault == "failed-producer" {
				if _, err := b.Materialize(context.Background(), cachebrokerlab.Marketplaces); err == nil {
					t.Fatal("invalid source succeeded")
				}
				if b.Receipt(cachebrokerlab.Marketplaces).State != "unproven" {
					t.Fatal("failed producer committed")
				}
				out, err := b.Reclaim(context.Background(), b.Receipt(cachebrokerlab.Marketplaces))
				if err != nil || out.Removed || out.Bytes != 0 {
					t.Fatalf("failed producer reclaim: %+v %v", out, err)
				}
				return
			}
			if _, err := b.Materialize(context.Background(), cachebrokerlab.Catalog); err != nil {
				t.Fatal(err)
			}
			if err := b.Close(); err != nil {
				t.Fatal(err)
			}
			if fault == "stale-lease" {
				labWrite(t, filepath.Join(root, "broker.lease"), "interrupted owner")
			} else {
				labWrite(t, filepath.Join(root, "journal.json"), "incomplete journal {")
			}
			before := labTreeSnapshot(t, filepath.Join(root, filepath.FromSlash(cachebrokerlab.Catalog)))
			unexpected, err := cachebrokerlab.Open(policy)
			if err == nil {
				unexpected.Close()
				t.Fatal("incomplete state was adopted")
			}
			if before != labTreeSnapshot(t, filepath.Join(root, filepath.FromSlash(cachebrokerlab.Catalog))) {
				t.Fatal("incomplete cache changed")
			}
			t.Logf("retained incomplete state: %v", err)
		})
	}
}

func startConcurrentAttack(t *testing.T, cfg nativeLab, root, artifact string) func() {
	t.Helper()
	cmd := nativeAttackCommand(t, cfg, root, "concurrent-neighbour")
	cmd.Args = []string{cmd.Path, "-test.run=^TestBrokerConcurrentAttackChild$"}
	cmd.Env = append(cmd.Env, "MULTICA_BROKER_ARTIFACT="+artifact)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	finished := false
	stop := func() {
		if finished {
			return
		}
		finished = true
		stdin.Close()
		out, readErr := io.ReadAll(stdout)
		waitErr := cmd.Wait()
		t.Logf("concurrent writer: %s%s", out, stderr.String())
		if readErr != nil || waitErr != nil {
			t.Errorf("concurrent child: %v %v", readErr, waitErr)
		}
	}
	t.Cleanup(stop)
	line, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil || line != "READY\n" {
		t.Fatalf("child readiness: %q %v", line, err)
	}
	return stop
}

func TestBrokerConcurrentAttackChild(t *testing.T) {
	root := os.Getenv("MULTICA_BROKER_ATTACK_ROOT")
	if root == "" {
		return
	}
	if nativePrivileged() {
		t.Fatal("attacker is privileged")
	}
	leaf := filepath.Join(root, filepath.FromSlash(os.Getenv("MULTICA_BROKER_ARTIFACT")))
	done := make(chan struct{})
	go func() { io.Copy(io.Discard, os.Stdin); close(done) }()
	iterations := 0
	for {
		for _, path := range []string{root, filepath.Join(root, "codex-home"), filepath.Dir(leaf), leaf} {
			for op, run := range map[string]func() error{
				"write":  func() error { return os.WriteFile(filepath.Join(path, "concurrent-write"), []byte("attack"), 0600) },
				"rename": func() error { return os.Rename(path, path+".swap") },
				"chmod":  func() error { return os.Chmod(path, 0777) },
			} {
				if err := run(); err == nil {
					t.Fatalf("concurrent %s succeeded: %s", op, path)
				}
			}
		}
		if err := os.Mkdir(leaf, 0755); err == nil {
			t.Fatal("attacker replaced consumed instance")
		}
		iterations++
		if iterations == 1 {
			fmt.Println("READY")
		}
		select {
		case <-done:
			fmt.Printf("task_identity=%s iterations=%d all_concurrent_mutations_denied\n", nativeIdentity(), iterations)
			return
		default:
		}
	}
}
