package cachebrokerlab

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"time"
)

// ServiceConfig is privileged bootstrap, never an IPC request. The lab binary
// installs exactly one measured recipe. Production deployment is not wired.
type ServiceConfig struct {
	Root, RecipeID     string
	Recipe             Policy
	ArtifactTTLSeconds int64
	ReadOnly           bool
	Fault              func(string) error `json:"-"` // Disposable fault harness only.
	RuntimeHomes       []WritableHome
	Production         ProductionFactory `json:"-"`
	Storage            *StorageLayout
}

// ProductionFactory is a privileged bootstrap capability. IPC callers select
// neither a staging path nor a native identity. Release closes native handles.
type ProductionFactory func(homeID, artifact string, binding RunBinding) (plan ProductionPlan, release func() error, err error)

// Request deliberately has no paths, commands, timestamps or lifecycle flags.
// End/reconcile are assertions from the authenticated daemon authority, after it
// proves its run's entire process tree is stopped. A task cannot call them.
type Request struct {
	Operation, RequestID, Epoch, HomeID, RecipeID string
	Revision, Generation                          uint64
	LeaseID, RunID, Intent, Artifact              string
}

type Generation struct {
	ID                        uint64
	Lifecycle, LeaseID, RunID string
	Intent                    string
	EndedAt                   int64
	Artifacts                 map[string]string
}

type Home struct {
	ID, RecipeID      string
	Revision, Current uint64
	Generations       map[uint64]*Generation
}

type Response struct {
	Epoch, Error, Path string
	Home               *Home    `json:",omitempty"`
	Receipt            *Receipt `json:",omitempty"`
	Outcome            Outcome
}

type completedRequest struct {
	Digest   string
	Response Response
}

type registry struct {
	Version  int
	Homes    map[string]*Home
	Requests map[string]completedRequest
	Storage  *StorageLayout
	Slots    map[string]slotClaim
}

const (
	maxRegistryBytes         = 4 << 20
	maxRegisteredHomes       = 64
	maxHomeGenerations       = 64
	maxRequestOutcomes       = 1024
	quarantineAdmissionLimit = 16
)

func readBoundedJournal(path string, limit int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	err = errors.Join(err, f.Close())
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, errors.New("journal byte limit exceeded; retain namespace")
	}
	return data, nil
}

type Service struct {
	mu               sync.Mutex
	config           ServiceConfig
	epoch            string
	journal          registry
	lock             *os.File
	fault            func(string) error
	poisoned         bool
	storageProbe     storageProbe
	protectedStorage bool
}

func OpenService(cfg ServiceConfig) (*Service, error) {
	if cfg.RecipeID == "" || cfg.Root == "" {
		return nil, errors.New("missing service bootstrap")
	}
	if cfg.Production != nil && cfg.Storage == nil {
		return nil, errors.New("isolated acquisition requires protected quota storage registry")
	}
	if cfg.Storage != nil {
		if err := cfg.Storage.protected(cfg.Root); err != nil {
			return nil, err
		}
		if err := cfg.Storage.check(cfg.Root, false, nativeStorageVolume); err != nil {
			return nil, err
		}
	}
	for _, p := range []string{cfg.Root, cfg.Recipe.Producer, cfg.Recipe.Git, cfg.Recipe.Source} {
		if err := protectedPath(p); err != nil {
			return nil, err
		}
	}
	if err := verifyBinary(cfg.Recipe.Producer, cfg.Recipe.ProducerSHA256); err != nil {
		return nil, err
	}
	s, err := openService(cfg)
	if err == nil {
		s.protectedStorage = true
	}
	return s, err
}

// openService contains the same disk/lock/recovery implementation used natively;
// unit tests use an ordinary temporary directory, without claiming OS isolation.
func openService(cfg ServiceConfig) (*Service, error) {
	return openServiceWithStorage(cfg, nativeStorageVolume)
}

func openServiceWithStorage(cfg ServiceConfig, probe storageProbe) (*Service, error) {
	if cfg.Storage != nil {
		if err := cfg.Storage.check(cfg.Root, false, probe); err != nil {
			return nil, err
		}
	}
	lock, err := lockService(filepath.Join(cfg.Root, "service.lock"))
	if err != nil {
		return nil, err
	}
	s := &Service{config: cfg, epoch: nonce(), lock: lock, fault: cfg.Fault, storageProbe: probe}
	fail := func(err error) (*Service, error) { s.Close(); return nil, err }
	data, err := readBoundedJournal(filepath.Join(cfg.Root, "registry.json"), maxRegistryBytes)
	if errors.Is(err, os.ErrNotExist) {
		entries, e := os.ReadDir(cfg.Root)
		if e != nil {
			return fail(e)
		}
		if len(entries) != 1 || entries[0].Name() != "service.lock" || cfg.ReadOnly {
			return fail(errors.New("missing registry in a nonempty or read-only installation; retain everything"))
		}
		s.journal = registry{Version: 1, Homes: map[string]*Home{}, Requests: map[string]completedRequest{}, Storage: cfg.Storage, Slots: map[string]slotClaim{}}
	} else if err != nil {
		return fail(err)
	} else {
		if err = strictJSON(data, &s.journal); err != nil {
			return fail(err)
		}
		if err = s.validateRegistry(); err != nil {
			return fail(err)
		}
		if err = s.validateStorageRegistry(); err != nil {
			return fail(err)
		}
		for path, claim := range s.journal.Slots {
			if claim.State == "reserved" {
				claim.State = "quarantined"
				s.journal.Slots[path] = claim
			}
		}
		for _, h := range s.journal.Homes {
			h.Revision++
			for _, g := range h.Generations {
				if g.ID == h.Current {
					g.Lifecycle, g.EndedAt = "unknown", 0
				}
				for artifact, state := range g.Artifacts {
					if state == "preparing" || state == "consuming" {
						g.Artifacts[artifact] = "quarantined"
					}
				}
			}
		}
	}
	if !cfg.ReadOnly {
		if err = s.persist(); err != nil {
			return fail(err)
		}
		if err = os.MkdirAll(filepath.Join(cfg.Root, "homes"), 0755); err != nil {
			return fail(err)
		}
	}
	return s, nil
}

func (s *Service) validateRegistry() error {
	if s.journal.Version != 1 || s.journal.Homes == nil || s.journal.Requests == nil || len(s.journal.Homes) > maxRegisteredHomes || len(s.journal.Requests) > maxRequestOutcomes {
		return errors.New("unsupported or incomplete registry")
	}
	for id, h := range s.journal.Homes {
		if !validID(id) || h == nil || h.ID != id || h.RecipeID != s.config.RecipeID || h.Revision == 0 || h.Generations == nil {
			return errors.New("invalid registered home")
		}
		if uint64(len(h.Generations)) != h.Current || h.Current > maxHomeGenerations {
			return errors.New("missing generation")
		}
		for n, g := range h.Generations {
			if g == nil || n == 0 || n > h.Current || g.ID != n || !validID(g.LeaseID) || g.RunID == "" || g.Artifacts == nil {
				return errors.New("invalid generation")
			}
			if g.Lifecycle != "active" && g.Lifecycle != "terminal" && g.Lifecycle != "unknown" {
				return errors.New("unknown lifecycle schema")
			}
			if n != h.Current && g.Lifecycle != "terminal" {
				return errors.New("superseded generation still owns a run")
			}
			for a, state := range g.Artifacts {
				if !exact(a) {
					return errors.New("unregistered artifact in registry")
				}
				switch state {
				case "preparing", "ready", "consuming", "deleted", "quarantined":
				default:
					return errors.New("unknown artifact state")
				}
			}
		}
	}
	return nil
}

func validID(id string) bool { data, err := hex.DecodeString(id); return err == nil && len(data) == 16 }
func nonce() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b[:])
}
func strictJSON(data []byte, v any) error {
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		return err
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return errors.New("trailing JSON")
	}
	return nil
}

func (s *Service) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lock == nil {
		return nil
	}
	err := s.lock.Close()
	s.lock = nil
	return err // Keep the stable lock inode; the OS releases its lock on death.
}

func (s *Service) hit(point string) error {
	if s.fault != nil {
		return s.fault(point)
	}
	return nil
}

func (s *Service) persist() (err error) {
	defer func() {
		if err != nil {
			s.poisoned = true
		}
	}()
	data, err := json.Marshal(s.journal)
	if err != nil {
		return err
	}
	if len(data) > maxRegistryBytes {
		return errors.New("registry byte limit exceeded; service latched")
	}
	name := filepath.Join(s.config.Root, "registry-"+nonce()+".tmp")
	f, err := os.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	if err = s.hit("registry.before-write"); err == nil {
		_, err = f.Write(data)
	}
	if err == nil {
		err = s.hit("registry.before-sync")
	}
	if err == nil {
		err = f.Sync()
	}
	err = errors.Join(err, f.Close())
	if err != nil {
		return err
	}
	if err = s.hit("registry.before-replace"); err != nil {
		return err
	}
	if err = replaceServiceFile(name, filepath.Join(s.config.Root, "registry.json")); err != nil {
		return err
	}
	return s.hit("registry.after-replace")
}

func (s *Service) generationPath(home string, generation uint64) string {
	return filepath.Join(s.config.Root, "homes", home, fmt.Sprintf("g-%d", generation))
}

func (s *Service) Execute(ctx context.Context, req Request) Response {
	s.mu.Lock()
	defer s.mu.Unlock()
	resp := Response{Epoch: s.epoch}
	bad := func(err error) Response {
		resp.Error = err.Error()
		resp.Outcome = Outcome{}
		resp.Receipt = nil
		return resp
	}
	if s.lock == nil {
		return bad(errors.New("service closed"))
	}
	if err := ctx.Err(); err != nil {
		return bad(err)
	}
	if req.Operation == "hello" {
		return resp
	}
	if s.poisoned {
		return bad(errors.New("uncertain registry durability or storage authority; restart required even for outcome queries"))
	}
	if req.Operation == "query" {
		h := s.journal.Homes[req.HomeID]
		if h == nil {
			return bad(errors.New("unknown home"))
		}
		resp.Home = cloneHome(h)
		resp.Path = s.generationPath(h.ID, h.Current)
		return resp
	}
	if req.Operation == "result" {
		r, ok := s.journal.Requests[req.RequestID]
		if !ok {
			return bad(errors.New("no committed request outcome"))
		}
		return cloneResponse(r.Response)
	}
	if s.config.ReadOnly {
		return bad(errors.New("read-only rollback; mutations disabled"))
	}
	admission := req.Operation == "register" || req.Operation == "begin" || req.Operation == "prepare"
	if err := s.checkStorage(admission); err != nil {
		if !errors.Is(err, errStorageExhausted) {
			s.poisoned = true
		}
		return bad(err)
	}
	if req.Epoch != s.epoch || req.RequestID == "" || len(req.RequestID) > 128 {
		return bad(errors.New("stale service epoch or invalid request ID"))
	}
	encoded, _ := json.Marshal(req)
	hash := sha256.Sum256(encoded)
	digest := hex.EncodeToString(hash[:])
	if old, ok := s.journal.Requests[req.RequestID]; ok {
		if old.Digest != digest {
			return bad(errors.New("request ID reused with different command"))
		}
		return cloneResponse(old.Response)
	}
	if len(s.journal.Requests) >= maxRequestOutcomes {
		return bad(errors.New("request journal capacity reached; preserve outcomes and retained data"))
	}
	if req.Operation == "register" || req.Operation == "begin" || req.Operation == "prepare" {
		quarantined := 0
		for _, home := range s.journal.Homes {
			for _, g := range home.Generations {
				for _, state := range g.Artifacts {
					if state == "quarantined" {
						quarantined++
					}
				}
			}
		}
		if quarantined >= quarantineAdmissionLimit {
			return bad(errors.New("quarantine admission limit reached; no automatic eviction"))
		}
	}
	var h *Home
	if req.Operation == "register" {
		if len(s.journal.Homes) >= maxRegisteredHomes || ((s.config.RuntimeHomes != nil || s.config.Production != nil) && len(s.journal.Homes) != 0) {
			return bad(errors.New("home or immutable identity allocation capacity reached"))
		}
		if req.RecipeID != s.config.RecipeID || req.HomeID != "" || req.Generation != 0 {
			return bad(errors.New("unallowlisted registration"))
		}
		h = &Home{ID: nonce(), RecipeID: req.RecipeID, Revision: 1, Generations: map[uint64]*Generation{}}
		s.journal.Homes[h.ID] = h
	} else {
		h = s.journal.Homes[req.HomeID]
		if h == nil || h.Revision != req.Revision {
			return bad(errors.New("unknown home or stale revision"))
		}
		before := cloneHome(h)
		if err := s.mutate(ctx, h, req, &resp); err != nil {
			resp.Error = err.Error()
			resp.Outcome = Outcome{}
			resp.Receipt = nil
			// A failed intent may have been durable. Persist the exclusion/result;
			// a failed journal write instead permanently latches this instance.
			if s.poisoned {
				return resp
			}
			if reflect.DeepEqual(before, h) {
				return resp
			}
		}
		h.Revision++
	}
	resp.Home = cloneHome(h)
	resp.Path = s.generationPath(h.ID, h.Current)
	s.journal.Requests[req.RequestID] = completedRequest{Digest: digest, Response: cloneResponse(resp)}
	if req.Operation == "prepare" && resp.Error == "" {
		if err := s.hit("prepare.before-commit"); err != nil {
			s.poisoned = true
			return bad(err)
		}
	}
	if err := s.persist(); err != nil {
		return bad(err)
	}
	if req.Operation == "prepare" && resp.Error == "" {
		if err := s.hit("prepare.after-commit"); err != nil {
			s.poisoned = true
			return bad(err)
		}
	}
	if err := s.hit(req.Operation + ".response"); err != nil {
		return bad(err)
	}
	return resp
}

func (s *Service) mutate(ctx context.Context, h *Home, req Request, resp *Response) error {
	if req.Operation == "begin" {
		if h.Current >= maxHomeGenerations {
			return errors.New("generation capacity reached; retained namespaces preserved")
		}
		if s.config.RuntimeHomes != nil && h.Current >= uint64(len(s.config.RuntimeHomes)) {
			return errors.New("runtime identity capacity reached; retained generations preserved")
		}
		if req.RunID == "" || len(req.RunID) > 128 || (req.Intent != "launch" && req.Intent != "restore") {
			return errors.New("invalid trusted run reservation")
		}
		for _, old := range h.Generations {
			if old.Lifecycle != "terminal" {
				return errors.New("unresolved run retains its lease")
			}
		}
		for _, old := range h.Generations {
			for a, state := range old.Artifacts {
				if state == "ready" {
					old.Artifacts[a] = "quarantined"
				}
			}
		}
		h.Current++
		g := &Generation{ID: h.Current, Lifecycle: "active", LeaseID: nonce(), RunID: req.RunID, Intent: req.Intent, Artifacts: map[string]string{}}
		h.Generations[g.ID] = g
		if err := s.persist(); err != nil {
			return err
		}
		if err := s.hit("begin.intent"); err != nil {
			return err
		}
		parent := filepath.Dir(s.generationPath(h.ID, g.ID))
		if err := os.MkdirAll(parent, 0755); err != nil {
			return err
		}
		if err := os.Mkdir(s.generationPath(h.ID, g.ID), 0755); err != nil {
			return err
		}
		for _, rel := range []string{"tmp", "no-hooks", "codex-home"} {
			if err := os.Mkdir(filepath.Join(s.generationPath(h.ID, g.ID), rel), 0755); err != nil {
				return err
			}
		}
		return s.hit("begin.namespace")
	}
	g := h.Generations[req.Generation]
	if g == nil || g.ID != h.Current {
		return errors.New("unknown or superseded generation")
	}
	switch req.Operation {
	case "end", "reconcile":
		want := "active"
		if req.Operation == "reconcile" {
			want = "unknown"
		}
		if g.Lifecycle != want || req.LeaseID != g.LeaseID || req.RunID != g.RunID {
			return errors.New("stale use lease or run")
		}
		g.Lifecycle, g.EndedAt = "terminal", time.Now().UnixNano()
		return nil
	case "prepare":
		if g.Lifecycle != "active" || req.LeaseID != g.LeaseID || req.RunID != g.RunID || !exact(req.Artifact) || g.Artifacts[req.Artifact] != "" {
			return errors.New("prepare requires new artifact and active use lease")
		}
		if s.config.Production == nil {
			return errors.New("isolated acquisition is not provisioned")
		}
		g.Artifacts[req.Artifact] = "preparing"
		if err := s.persist(); err != nil {
			return err
		}
		if err := s.hit("prepare.intent"); err != nil {
			g.Artifacts[req.Artifact] = "quarantined"
			return err
		}
		b, err := s.broker(h, g)
		if err != nil {
			g.Artifacts[req.Artifact] = "quarantined"
			return err
		}
		r, err := s.acquire(ctx, b, h, g, req.Artifact)
		err = errors.Join(err, b.Close())
		if err != nil {
			g.Artifacts[req.Artifact] = "quarantined"
			return err
		}
		g.Artifacts[req.Artifact] = "ready"
		resp.Receipt = &r
		return nil
	case "reclaim":
		if g.Lifecycle != "terminal" || g.EndedAt <= 0 || s.config.ArtifactTTLSeconds <= 0 || time.Since(time.Unix(0, g.EndedAt)) < time.Duration(s.config.ArtifactTTLSeconds)*time.Second {
			return errors.New("trusted lifecycle or idle period denies reclamation")
		}
		if !exact(req.Artifact) || g.Artifacts[req.Artifact] != "ready" {
			return errors.New("unproven, consumed or quarantined generation")
		}
		g.Artifacts[req.Artifact] = "consuming"
		if err := s.persist(); err != nil {
			return err
		}
		if err := s.hit("reclaim.intent"); err != nil {
			g.Artifacts[req.Artifact] = "quarantined"
			return err
		}
		b, err := s.broker(h, g)
		if err != nil {
			g.Artifacts[req.Artifact] = "quarantined"
			return err
		}
		out, err := b.Reclaim(ctx, b.Receipt(req.Artifact))
		err = errors.Join(err, b.Close())
		if err != nil || !out.Removed {
			g.Artifacts[req.Artifact] = "quarantined"
		} else {
			g.Artifacts[req.Artifact] = "deleted"
		}
		resp.Outcome = out
		return err
	default:
		return errors.New("unknown operation")
	}
}

func (s *Service) broker(h *Home, g *Generation) (*Broker, error) {
	p := s.config.Recipe
	p.Root, p.HomeID = s.generationPath(h.ID, g.ID), fmt.Sprintf("%s/%d", h.ID, g.ID)
	p.Generation = g.ID
	p.ArtifactTTLSeconds = s.config.ArtifactTTLSeconds
	if s.config.RuntimeHomes != nil {
		if g.ID == 0 || g.ID > uint64(len(s.config.RuntimeHomes)) {
			return nil, errors.New("missing immutable runtime identity")
		}
		layout := s.config.RuntimeHomes[g.ID-1]
		p.RuntimeHome = &layout
	}
	p.Active, p.Terminal = g.Lifecycle == "active", g.Lifecycle == "terminal"
	p.IdleSeconds = int64(time.Since(time.Unix(0, g.EndedAt)) / time.Second)
	b, err := Open(p)
	if err == nil {
		b.fault = s.hit
	}
	return b, err
}

func (s *Service) acquire(ctx context.Context, b *Broker, h *Home, g *Generation, artifact string) (receipt Receipt, err error) {
	slot, err := s.reserveSlot(h, g, artifact)
	if err != nil {
		return Receipt{}, err
	}
	if slot != nil {
		defer func() {
			claim := s.journal.Slots[slot.Stage]
			claim.State = "retained"
			if err != nil {
				claim.State = "quarantined"
			}
			s.journal.Slots[slot.Stage] = claim
		}()
	}
	binding := RunBinding{RunID: g.RunID, LeaseID: g.LeaseID, Generation: g.ID}
	plan, release, err := s.config.Production(h.ID, artifact, binding)
	if release != nil {
		defer func() {
			err = errors.Join(err, release())
			if err != nil {
				receipt = Receipt{}
			}
		}()
	}
	if err != nil {
		return Receipt{}, err
	}
	if release == nil || plan.Binding.RunID != binding.RunID || plan.Binding.LeaseID != binding.LeaseID || plan.Binding.Generation != binding.Generation {
		return Receipt{}, errors.New("acquisition does not own this service run/lease/generation")
	}
	if slot != nil {
		var limit int64
		for _, volume := range s.config.Storage.Volumes {
			if volume.Root == slot.Volume {
				limit = volume.LimitBytes
			}
		}
		if plan.Stage != slot.Stage || plan.Volume != slot.Volume || plan.CapacityBytes != limit {
			return Receipt{}, errors.New("acquisition differs from protected slot registry")
		}
	}
	production, err := b.StartProduction(ctx, artifact, plan)
	if err != nil {
		return Receipt{}, err
	}
	if err = production.Wait(); err != nil {
		return Receipt{}, err
	}
	quiescence, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	for {
		quiet, err := plan.Supervisor.Quiescent(plan.Binding)
		if err != nil {
			return Receipt{}, err
		}
		if quiet {
			break
		}
		select {
		case <-quiescence.Done():
			return Receipt{}, fmt.Errorf("acquisition domain did not quiesce: %w", quiescence.Err())
		case <-time.After(20 * time.Millisecond):
		}
	}
	return production.Publish(ctx)
}

func cloneHome(h *Home) *Home {
	if h == nil {
		return nil
	}
	data, _ := json.Marshal(h)
	var copy Home
	_ = json.Unmarshal(data, &copy)
	return &copy
}
func cloneResponse(r Response) Response {
	r.Home = cloneHome(r.Home)
	if r.Receipt != nil {
		receipt := cloneReceipt(*r.Receipt)
		r.Receipt = &receipt
	}
	return r
}
