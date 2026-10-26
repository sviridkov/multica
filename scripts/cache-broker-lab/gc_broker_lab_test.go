//go:build cachebrokerlab

package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"github.com/multica-ai/multica/server/internal/daemon/cachebrokerlab"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

type labReclaimer func(context.Context, string) (cachebrokerlab.Outcome, error)
type nativeLab struct {
	Root, Producer, Git, TaskUser, TaskPassword string
	UID, GID                                    uint32
}

func mustLab(t *testing.T) nativeLab {
	t.Helper()
	path := os.Getenv("MULTICA_NATIVE_BROKER_CONFIG")
	if path == "" {
		t.Fatal("native fixture config required; not a skipped security check")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var cfg nativeLab
	if err = json.Unmarshal(data, &cfg); err != nil {
		t.Fatal(err)
	}
	return cfg
}
func labWrite(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0644); err != nil {
		t.Fatal(err)
	}
}
func labGit(t *testing.T, cfg nativeLab, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command(cfg.Git, args...)
	cmd.Dir = dir
	cmd.Env = append(nativeBaseEnv(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+filepath.Join(cfg.Root, "empty.gitconfig"), "GIT_TERMINAL_PROMPT=0")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}
func labBroker(t *testing.T, cfg nativeLab, options ...func(*cachebrokerlab.Policy)) (string, *cachebrokerlab.Broker) {
	t.Helper()
	binary, err := os.ReadFile(cfg.Producer)
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256(binary)
	root, err := os.MkdirTemp(cfg.Root, "case-")
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Chmod(root, 0755); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(root, "source")
	labWrite(t, filepath.Join(source, ".agents/plugins/marketplace.json"), `{"name":"cache-lab","plugins":[{"name":"sample","source":{"source":"local","path":"./plugins/sample"}}]}`)
	labWrite(t, filepath.Join(source, "plugins/sample/.codex-plugin/plugin.json"), `{"name":"sample"}`)
	labWrite(t, filepath.Join(source, "plugins/sample/marker.txt"), "immutable local fixture")
	labGit(t, cfg, source, "init")
	labGit(t, cfg, source, "add", ".")
	labGit(t, cfg, source, "-c", "user.name=Cache Lab", "-c", "user.email=cache-lab@example.invalid", "commit", "-m", "fixture")
	oid := labGit(t, cfg, source, "rev-parse", "HEAD")
	for _, rel := range []string{"tmp", "no-hooks", "codex-home"} {
		if err = os.MkdirAll(filepath.Join(root, rel), 0755); err != nil {
			t.Fatal(err)
		}
	}
	policy := cachebrokerlab.Policy{Root: root, Producer: cfg.Producer, ProducerSHA256: hex.EncodeToString(hash[:]), Git: cfg.Git, Source: source, SourceOID: oid, HomeID: filepath.Base(root), ArtifactTTLSeconds: 60, IdleSeconds: 120, Terminal: true}
	for _, option := range options {
		option(&policy)
	}
	b, err := cachebrokerlab.Open(policy)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := b.Close(); err != nil {
			t.Error(err)
		}
	})
	return root, b
}

func TestBrokerNativeCycles(t *testing.T) {
	cfg := mustLab(t)
	for _, route := range []string{"direct", "walk"} {
		for _, artifact := range []string{cachebrokerlab.Marketplaces, cachebrokerlab.Catalog} {
			t.Run(route+"/"+artifact, func(t *testing.T) {
				root, b := labBroker(t, cfg)
				var previous cachebrokerlab.Receipt
				for cycle := 1; cycle <= 2; cycle++ {
					receipt, err := b.Materialize(context.Background(), artifact)
					if err != nil {
						t.Fatal(err)
					}
					var want int64
					for _, e := range receipt.Entries {
						want += e.Bytes
					}
					if want <= 0 {
						t.Fatal("producer created no logical bytes")
					}
					if cycle == 2 {
						out, err := b.Reclaim(context.Background(), previous)
						if err != nil || out.Removed || out.Bytes != 0 {
							t.Fatalf("old generation replay: %+v %v", out, err)
						}
					}
					// Both requester-labelled and hostile neighbours execute as the SAME task
					// identity; neither has the broker's privileged process credentials.
					for _, role := range []string{"runtime", "neighbour"} {
						attack := nativeAttackCommand(t, cfg, root, role)
						out, err := attack.CombinedOutput()
						t.Logf("%s: %s", role, out)
						if err != nil {
							t.Fatalf("native attacker: %v", err)
						}
					}
					stopAttack := startConcurrentAttack(t, cfg, root, artifact)
					removed, bytes := labReclaim(t, root, route, b)
					stopAttack()
					if removed != 1 || bytes != want {
						t.Fatalf("cycle%d removed=%d logical_bytes=%d want=%d", cycle, removed, bytes, want)
					}
					if _, err := os.Lstat(filepath.Join(root, filepath.FromSlash(artifact))); !os.IsNotExist(err) {
						t.Fatalf("artifact survived: %v", err)
					}
					t.Logf("PRODUCER_CYCLE route=%s artifact=%s cycle=%d generation=%d logical_bytes=%d", route, artifact, cycle, receipt.Generation, bytes)
					previous = receipt
				}
				// A historical, unknown tree is not adopted and cannot be reclaimed.
				unknown := filepath.Join(root, filepath.FromSlash(artifact), "owner-notes")
				labWrite(t, unknown, "preserve this unknown content")
				if _, err := b.Materialize(context.Background(), artifact); err == nil {
					t.Fatal("adopted historical tree")
				}
				out, err := b.Reclaim(context.Background(), previous)
				if err != nil || out.Removed || out.Bytes != 0 {
					t.Fatalf("consumed receipt reclaimed unknown: %+v %v", out, err)
				}
				if data, err := os.ReadFile(unknown); err != nil || string(data) != "preserve this unknown content" {
					t.Fatal("unknown content changed")
				}
			})
		}
	}
}

func TestBrokerAttackChild(t *testing.T) {
	root := os.Getenv("MULTICA_BROKER_ATTACK_ROOT")
	if root == "" {
		return
	}
	if nativePrivileged() {
		t.Fatal("attacker still has broker privileges")
	}
	paths := []string{filepath.Dir(root), root, filepath.Join(root, "codex-home"), filepath.Join(root, "journal.json"), os.Getenv("MULTICA_BROKER_PRODUCER")}
	for _, rel := range []string{cachebrokerlab.Marketplaces, cachebrokerlab.Catalog} {
		leaf := filepath.Join(root, filepath.FromSlash(rel))
		if _, err := os.Lstat(leaf); os.IsNotExist(err) {
			continue
		}
		for p := leaf; p != root; p = filepath.Dir(p) {
			paths = append(paths, p)
		}
		walkErr := filepath.WalkDir(leaf, func(p string, d os.DirEntry, err error) error {
			if err == nil && !d.IsDir() {
				paths = append(paths, p)
			}
			return err
		})
		if walkErr != nil {
			t.Fatal(walkErr)
		}
	}
	for _, path := range paths {
		info, err := os.Lstat(path)
		if err != nil {
			t.Fatal(err)
		}
		ops := map[string]func() error{
			"chmod":       func() error { return os.Chmod(path, 0777) },
			"rename":      func() error { return os.Rename(path, path+".attacker") },
			"permissions": func() error { return nativePermissionAttack(path) },
		}
		if info.IsDir() {
			ops["create"] = func() error { return os.WriteFile(filepath.Join(path, "attacker-insertion"), []byte("attack"), 0600) }
		} else {
			ops["write"] = func() error { return os.WriteFile(path, []byte("attack"), 0600) }
			ops["unlink"] = func() error { return os.Remove(path) }
		}
		for op, f := range ops {
			if err := f(); err == nil {
				t.Fatalf("unauthorized %s succeeded on %s", op, path)
			}
		}
	}
	fmt.Printf("task_identity=%s role=%s protected_paths=%d all_mutations_denied\n", nativeIdentity(), os.Getenv("MULTICA_BROKER_ATTACK_ROLE"), len(paths))
}
