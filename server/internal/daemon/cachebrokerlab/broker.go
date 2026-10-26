// Package cachebrokerlab is an isolated experiment. It has no runtime wiring.
// All registration and lifecycle policy comes from privileged test bootstrap.
package cachebrokerlab

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"time"
)

const Marketplaces = "codex-home/.tmp/marketplaces"
const Catalog = "codex-home/cache/remote_plugin_catalog"

// Policy is installed by the administrator in the protected namespace. There
// is no registration RPC, adoption of existing trees, or task-provided command.
type Policy struct {
	IdleSeconds                                            int64
	Root, Producer, ProducerSHA256, Git, Source, SourceOID string
	HomeID                                                 string
	ArtifactTTLSeconds                                     int64
	Terminal, Active, LocalDirectory                       bool
	RuntimeHome                                            *WritableHome
	Generation                                             uint64 // Nonzero for a service-owned generation namespace.
}

// WritableHome names an administrator-installed native identity for the three
// exact create-only parents. It never grants mutations to cache contents.
type WritableHome struct {
	Group uint32
	SID   string
}

func runtimeParent(path, root string) bool {
	home := filepath.Join(root, "codex-home")
	return path == home || path == filepath.Join(home, ".tmp") || path == filepath.Join(home, "cache")
}

type Entry struct {
	Path, Digest, Metadata string
	Bytes                  int64
}
type SourceIdentity struct {
	Schema       int    `json:"schema"`
	Kind         string `json:"kind"`
	Revision     string `json:"revision"`
	Endpoint     string `json:"endpoint"`
	Scope        string `json:"scope"`
	CacheKey     string `json:"cache_key"`
	Written      bool   `json:"written"`
	ReadVerified bool   `json:"read_verified"`
}
type Receipt struct {
	Source                                             SourceIdentity
	Schema                                             int
	HomeID, Artifact, ProducerSHA256, SourceOID, State string
	Generation                                         uint64
	Entries                                            []Entry
}
type Outcome struct {
	Removed bool
	Bytes   int64
	Reason  string
}
type Broker struct {
	mu      sync.Mutex
	policy  Policy
	root    *os.Root
	lease   *os.File
	records map[string]Receipt
	fault   func(string) error
}

func (b *Broker) hit(point string) error {
	if b.fault != nil {
		return b.fault(point)
	}
	return nil
}

// Open requires a privileged, protected test installation; an ordinary process
// on a same-UID host cannot opt into trusting itself with a boolean or key.
func Open(p Policy) (*Broker, error) {
	if p.HomeID == "" || !filepath.IsAbs(p.Root) || !filepath.IsAbs(p.Producer) || !filepath.IsAbs(p.Git) {
		return nil, errors.New("invalid registered policy")
	}
	if err := protectedPath(p.Root); err != nil {
		return nil, err
	}
	for _, path := range []string{p.Producer, p.Git, p.Source} {
		if err := protectedPath(path); err != nil {
			return nil, err
		}
	}
	if err := verifyBinary(p.Producer, p.ProducerSHA256); err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(p.Root)
	if err != nil {
		return nil, err
	}
	lease, err := root.OpenFile("broker.lease", os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		root.Close()
		return nil, fmt.Errorf("broker lease: %w", err)
	}
	b := &Broker{policy: p, root: root, lease: lease, records: map[string]Receipt{}}
	data, err := root.ReadFile("journal.json")
	if err == nil {
		err = json.Unmarshal(data, &b.records)
	} else if errors.Is(err, os.ErrNotExist) {
		err = nil
	}
	if err != nil {
		b.Close()
		return nil, fmt.Errorf("journal unreadable: %w", err)
	}
	return b, nil
}
func (b *Broker) Close() error {
	if b.lease == nil {
		return nil
	}
	err := b.lease.Close()
	b.lease = nil
	err = errors.Join(err, b.root.Remove("broker.lease"), b.root.Close())
	return err
}
func verifyBinary(path, want string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	h := sha256.New()
	if _, err = io.Copy(h, f); err != nil {
		return err
	}
	if want == "" || hex.EncodeToString(h.Sum(nil)) != want {
		return errors.New("producer build mismatch")
	}
	return nil
}
func (b *Broker) persist() error {
	data, err := json.Marshal(b.records)
	if err != nil {
		return err
	}
	f, err := b.root.OpenFile("journal.new", os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	writeErr := b.hit("receipt.before-write")
	if writeErr == nil {
		_, writeErr = f.Write(data)
	}
	syncErr := b.hit("receipt.before-sync")
	if syncErr == nil {
		syncErr = f.Sync()
	}
	closeErr := f.Close()
	if err = errors.Join(writeErr, syncErr, closeErr); err != nil {
		return err
	}
	if err := b.hit("receipt.before-replace"); err != nil {
		return err
	}
	if err := replaceJournal(b.policy.Root); err != nil {
		return err
	}
	return b.hit("receipt.after-replace")
}
func exact(artifact string) bool { return artifact == Marketplaces || artifact == Catalog }
func (b *Broker) Receipt(artifact string) Receipt {
	b.mu.Lock()
	defer b.mu.Unlock()
	r := b.records[artifact]
	r.Entries = append([]Entry(nil), r.Entries...)
	return r
}

// Materialize runs only the measured producer. Unknown existing trees are never
// adopted or overwritten. Failed production leaves an unproven generation.
func (b *Broker) Materialize(ctx context.Context, artifact string) (Receipt, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !exact(artifact) {
		return Receipt{}, errors.New("unregistered artifact")
	}
	if _, err := b.root.Lstat(artifact); !errors.Is(err, os.ErrNotExist) {
		return Receipt{}, errors.New("existing or unreadable artifact retained")
	}
	if err := protectedPath(b.policy.Root); err != nil {
		return Receipt{}, err
	}
	if err := verifyBinary(b.policy.Producer, b.policy.ProducerSHA256); err != nil {
		return Receipt{}, err
	}
	old := b.records[artifact]
	r := Receipt{Schema: 1, HomeID: b.policy.HomeID, Artifact: artifact, ProducerSHA256: b.policy.ProducerSHA256, SourceOID: b.policy.SourceOID, Generation: old.Generation + 1, State: "unproven"}
	if artifact == Catalog {
		r.SourceOID = ""
	}
	b.records[artifact] = r
	if err := b.persist(); err != nil {
		return cloneReceipt(r), err
	}
	if err := b.hit("producer.intent"); err != nil {
		return cloneReceipt(r), err
	}
	kind := "catalog"
	if artifact == Marketplaces {
		kind = "marketplace"
	}
	cmd := exec.CommandContext(ctx, b.policy.Producer, "--exact", "remote::broker_lab::produce", "--ignored", "--nocapture")
	cmd.Dir = b.policy.Root
	evidencePath := filepath.Join(b.policy.Root, fmt.Sprintf("producer-%s-%d.json", kind, r.Generation))
	cmd.Env = append(producerEnvironment(b.policy, kind), "MULTICA_CACHE_LAB_EVIDENCE="+evidencePath)
	cmd.WaitDelay = 5 * time.Second
	var output bytes.Buffer
	cmd.Stdout, cmd.Stderr = &output, &output
	if err := cmd.Start(); err != nil {
		return r, err
	}
	if err := b.hit("producer.running"); err != nil {
		killErr := cmd.Process.Kill()
		waitErr := cmd.Wait()
		return r, errors.Join(err, killErr, waitErr)
	}
	if err := cmd.Wait(); err != nil {
		return r, fmt.Errorf("producer failed: %w: %s", err, output.Bytes())
	}
	if err := b.hit("producer.exited"); err != nil {
		return cloneReceipt(r), err
	}
	evidenceBytes, err := os.ReadFile(evidencePath)
	if err != nil {
		return cloneReceipt(r), err
	}
	if err = json.Unmarshal(evidenceBytes, &r.Source); err != nil {
		return cloneReceipt(r), err
	}
	if err = validateSource(b.policy, artifact, r.Source); err != nil {
		return r, err
	}
	if err := publishReadable(filepath.Join(b.policy.Root, filepath.FromSlash(artifact))); err != nil {
		return cloneReceipt(r), err
	}
	entries, err := inventory(filepath.Join(b.policy.Root, filepath.FromSlash(artifact)))
	if err != nil {
		return cloneReceipt(r), err
	}
	r.State = "committed"
	if err := b.hit("producer.inventory"); err != nil {
		return cloneReceipt(r), err
	}
	r.Entries = entries
	b.records[artifact] = r
	if err := b.persist(); err != nil {
		r.State = "unproven"
		b.records[artifact] = r
		return cloneReceipt(r), err
	}
	return cloneReceipt(r), b.hit("producer.committed")
}

// Reclaim owns validation, journal consumption and deletion in one serialized
// operation. Its protected namespace excludes uncooperative task writers.
func (b *Broker) Reclaim(ctx context.Context, expected Receipt) (Outcome, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	retain := func(reason string) (Outcome, error) { return Outcome{Reason: reason}, nil }
	if err := ctx.Err(); err != nil {
		return Outcome{}, err
	}
	if !exact(expected.Artifact) {
		return retain("unregistered artifact")
	}
	if b.policy.ArtifactTTLSeconds <= 0 || b.policy.IdleSeconds < b.policy.ArtifactTTLSeconds || b.policy.Active || !b.policy.Terminal || b.policy.LocalDirectory {
		return retain("protected lifecycle policy denies reclamation")
	}
	r, ok := b.records[expected.Artifact]
	if !ok || r.State != "committed" || r.Schema != 1 || !reflect.DeepEqual(expected, r) {
		return retain("missing, stale, replayed or mismatched receipt")
	}
	if r.HomeID != b.policy.HomeID || r.ProducerSHA256 != b.policy.ProducerSHA256 || (r.Artifact == Marketplaces && r.SourceOID != b.policy.SourceOID) {
		return retain("registered identity mismatch")
	}
	if err := validateSource(b.policy, r.Artifact, r.Source); err != nil {
		return retain("source binding changed: " + err.Error())
	}
	target := filepath.Join(b.policy.Root, filepath.FromSlash(r.Artifact))
	if err := protectedArtifactPath(target, b.policy); err != nil {
		return retain("namespace protection changed: " + err.Error())
	}
	current, err := inventory(target)
	if err != nil {
		return retain("inventory unavailable: " + err.Error())
	}
	if !reflect.DeepEqual(current, r.Entries) {
		return retain("content, identity or metadata changed")
	}
	if err := ctx.Err(); err != nil {
		return Outcome{}, err
	}
	// Consume before unlink. A crash or partial failure cannot authorize a new
	// instance; remaining bytes require explicit recovery and are never adopted.
	r.State = "consumed"
	b.records[r.Artifact] = r
	if err := b.persist(); err != nil {
		return Outcome{}, err
	}
	if err := b.hit("reclaim.consumed"); err != nil {
		return Outcome{}, err
	}
	if err := b.remove(r); err != nil {
		return Outcome{}, fmt.Errorf("partial reclaim; receipt consumed: %w", err)
	}
	if err := b.hit("reclaim.absent"); err != nil {
		return Outcome{}, err
	}
	var size int64
	for _, e := range r.Entries {
		size += e.Bytes
	}
	return Outcome{Removed: true, Bytes: size}, nil
}

func (b *Broker) remove(r Receipt) error {
	if b.fault == nil {
		return b.root.RemoveAll(r.Artifact)
	}
	// WalkDir inventory is parent-first. Reverse removal provides real, precise
	// crash boundaries without following links or deleting unrecorded entries.
	for i := len(r.Entries) - 1; i >= 0; i-- {
		if err := b.hit("unlink.before"); err != nil {
			return err
		}
		if err := b.root.Remove(filepath.Join(r.Artifact, filepath.FromSlash(r.Entries[i].Path))); err != nil {
			return err
		}
		if err := b.hit("unlink.after"); err != nil {
			return err
		}
	}
	return nil
}

func inventory(root string) ([]Entry, error) {
	entries := []Entry{}
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		if !info.IsDir() && !info.Mode().IsRegular() {
			return fmt.Errorf("unsupported entry %s", path)
		}
		metadata, err := entryMetadata(path, info)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		e := Entry{Path: filepath.ToSlash(rel), Metadata: metadata}
		if info.Mode().IsRegular() {
			f, err := os.Open(path)
			if err != nil {
				return err
			}
			h := sha256.New()
			n, copyErr := io.Copy(h, f)
			closeErr := f.Close()
			if err = errors.Join(copyErr, closeErr); err != nil {
				return err
			}
			e.Bytes = n
			e.Digest = hex.EncodeToString(h.Sum(nil))
		}
		entries = append(entries, e)
		return nil
	})
	return entries, err
}

func validateSource(p Policy, artifact string, s SourceIdentity) error {
	if s.Schema != 1 || !s.Written || !s.ReadVerified {
		return errors.New("producer did not confirm publication/readback")
	}
	if artifact == Marketplaces {
		if s.Kind != "marketplace" || s.Revision != p.SourceOID || s.Endpoint != "https://cache-lab.invalid/fixture.git" || s.Scope != "fixture" || s.CacheKey != "cache-lab" {
			return errors.New("marketplace recipe mismatch")
		}
		return nil
	}
	endpoint, err := url.Parse(s.Endpoint)
	if err != nil {
		return err
	}
	digest, err := hex.DecodeString(s.Revision)
	if s.Kind != "catalog" || err != nil || len(digest) != 32 || endpoint.Scheme != "http" || (endpoint.Hostname() != "127.0.0.1" && endpoint.Hostname() != "[::1]" && endpoint.Hostname() != "::1") || s.Scope != "global" || !strings.HasSuffix(s.CacheKey, ".json") || filepath.Base(s.CacheKey) != s.CacheKey {
		return errors.New("catalog acquisition mismatch")
	}
	return nil
}

func cloneReceipt(r Receipt) Receipt { r.Entries = append([]Entry(nil), r.Entries...); return r }
