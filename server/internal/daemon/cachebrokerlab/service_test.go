package cachebrokerlab

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func serviceFixture(t *testing.T, root string) *Service {
	t.Helper()
	s, err := openService(ServiceConfig{Root: root, RecipeID: "pinned-fixture", ArtifactTTLSeconds: 1})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func serviceCall(t *testing.T, s *Service, req Request) Response {
	t.Helper()
	req.Epoch = s.epoch
	resp := s.Execute(context.Background(), req)
	if resp.Error != "" {
		t.Fatal(resp.Error)
	}
	return resp
}

func TestServicePrepareWithoutStagingDoesNotReserveOrLaunch(t *testing.T) {
	s := serviceFixture(t, t.TempDir())
	h := serviceCall(t, s, Request{Operation: "register", RequestID: "r", RecipeID: "pinned-fixture"}).Home
	h = serviceCall(t, s, Request{Operation: "begin", RequestID: "b", HomeID: h.ID, Revision: h.Revision, RunID: "run", Intent: "launch"}).Home
	g := h.Generations[h.Current]
	resp := s.Execute(context.Background(), Request{Operation: "prepare", RequestID: "p", Epoch: s.epoch, HomeID: h.ID, Revision: h.Revision, Generation: h.Current, RunID: g.RunID, LeaseID: g.LeaseID, Artifact: Catalog})
	if resp.Error == "" || resp.Receipt != nil {
		t.Fatal("service accepted preparation without isolated acquisition")
	}
	current := serviceCall(t, s, Request{Operation: "query", HomeID: h.ID}).Home
	if len(current.Generations[1].Artifacts) != 0 || current.Revision != h.Revision {
		t.Fatal("unconfigured staging reached the producer intent")
	}
}

func TestServiceAcquisitionRejectsForeignBindingBeforeLaunch(t *testing.T) {
	for _, change := range []string{"run", "lease", "generation"} {
		t.Run(change, func(t *testing.T) {
			s := serviceFixture(t, t.TempDir())
			released := false
			s.config.Production = func(_ string, _ string, binding RunBinding) (ProductionPlan, func() error, error) {
				switch change {
				case "run":
					binding.RunID = "foreign"
				case "lease":
					binding.LeaseID = "foreign"
				case "generation":
					binding.Generation++
				}
				return ProductionPlan{Binding: binding}, func() error { released = true; return nil }, nil
			}
			// No broker exists: accepting the foreign binding would reach launch.
			r, err := s.acquire(context.Background(), nil, &Home{ID: "home"}, &Generation{ID: 2, RunID: "run", LeaseID: "lease"}, Catalog)
			if err == nil || r.State != "" || !released {
				t.Fatal("foreign acquisition accepted or retained native handles", err)
			}
		})
	}
}

func TestServiceAcquisitionReportsProvisioningAndReleaseFailure(t *testing.T) {
	s := serviceFixture(t, t.TempDir())
	provision, release := errors.New("native identity unavailable"), errors.New("native handle close failed")
	s.config.Production = func(string, string, RunBinding) (ProductionPlan, func() error, error) {
		return ProductionPlan{}, func() error { return release }, provision
	}
	r, err := s.acquire(context.Background(), nil, &Home{}, &Generation{ID: 1}, Catalog)
	if !errors.Is(err, provision) || !errors.Is(err, release) || r.State != "" {
		t.Fatal("acquisition failure was hidden", err)
	}
}

func TestServiceRegistrationIsDurableAndNeverAdoptsUnknownHomes(t *testing.T) {
	root := t.TempDir()
	s := serviceFixture(t, root)
	req := Request{Operation: "register", RequestID: "registration", RecipeID: "pinned-fixture"}
	first := serviceCall(t, s, req)
	again := serviceCall(t, s, req)
	if first.Home.ID == "" || first.Home.ID != again.Home.ID || len(s.journal.Homes) != 1 {
		t.Fatal("registration retry created another home")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s = serviceFixture(t, root)
	recovered := serviceCall(t, s, Request{Operation: "result", RequestID: "registration"})
	if recovered.Home.ID != first.Home.ID {
		t.Fatal("lost committed registration response")
	}
	if resp := s.Execute(context.Background(), Request{Operation: "register", Epoch: s.epoch, RequestID: "bad", RecipeID: "arbitrary"}); resp.Error == "" {
		t.Fatal("unallowlisted recipe accepted")
	}

	unknown := t.TempDir()
	if err := os.Mkdir(filepath.Join(unknown, "homes"), 0700); err != nil {
		t.Fatal(err)
	}
	if unexpected, err := openService(ServiceConfig{Root: unknown}); err == nil {
		unexpected.Close()
		t.Fatal("missing registry adopted historical namespace")
	}
}

func TestServiceRestartRevokesLifecycleAndOldCommands(t *testing.T) {
	s := serviceFixture(t, t.TempDir())
	h := serviceCall(t, s, Request{Operation: "register", RequestID: "r", RecipeID: "pinned-fixture"}).Home
	h = serviceCall(t, s, Request{Operation: "begin", RequestID: "b", HomeID: h.ID, Revision: h.Revision, RunID: "run-one", Intent: "launch"}).Home
	oldEpoch, oldLease, oldPath := s.epoch, h.Generations[h.Current].LeaseID, s.generationPath(h.ID, h.Current)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s = serviceFixture(t, s.config.Root)
	h = serviceCall(t, s, Request{Operation: "query", HomeID: h.ID}).Home
	if h.Generations[h.Current].Lifecycle != "unknown" {
		t.Fatal("restart inferred task termination")
	}
	for _, req := range []Request{
		{Operation: "end", RequestID: "e", Epoch: oldEpoch, HomeID: h.ID, Revision: h.Revision, Generation: h.Current, LeaseID: oldLease},
		{Operation: "begin", RequestID: "b2", Epoch: s.epoch, HomeID: h.ID, Revision: h.Revision, RunID: "run-two", Intent: "restore"},
		{Operation: "reclaim", RequestID: "g", Epoch: s.epoch, HomeID: h.ID, Revision: h.Revision, Generation: h.Current, Artifact: Catalog},
	} {
		if resp := s.Execute(context.Background(), req); resp.Error == "" {
			t.Fatalf("unsafe command accepted: %s", req.Operation)
		}
	}
	h = serviceCall(t, s, Request{Operation: "reconcile", RequestID: "reconciled", HomeID: h.ID, Revision: h.Revision, Generation: h.Current, LeaseID: oldLease, RunID: "run-one"}).Home
	h = serviceCall(t, s, Request{Operation: "begin", RequestID: "b3", HomeID: h.ID, Revision: h.Revision, RunID: "run-two", Intent: "restore"}).Home
	if h.Current != 2 || s.generationPath(h.ID, h.Current) == oldPath {
		t.Fatal("reused old generation")
	}
	if _, err := os.Stat(oldPath); err != nil {
		t.Fatal("old generation lost", err)
	}
}

func TestServiceLockSurvivesFileExistenceAndRejectsConcurrentWriter(t *testing.T) {
	root := t.TempDir()
	s := serviceFixture(t, root)
	if second, err := openService(ServiceConfig{Root: root}); err == nil {
		second.Close()
		t.Fatal("two journal writers")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	serviceFixture(t, root)
}

func TestServiceJournalFailureLatchesMutationsAndPreservesUnknownData(t *testing.T) {
	s := serviceFixture(t, t.TempDir())
	s.fault = func(point string) error {
		if point == "registry.before-replace" {
			return os.ErrPermission
		}
		return nil
	}
	resp := s.Execute(context.Background(), Request{Operation: "register", Epoch: s.epoch, RequestID: "failed", RecipeID: "pinned-fixture"})
	if resp.Error == "" {
		t.Fatal("journal failure reported success")
	}
	if result := s.Execute(context.Background(), Request{Operation: "result", RequestID: "failed"}); result.Error == "" {
		t.Fatal("query exposed an uncommitted successful result")
	}
	s.fault = nil
	if resp := s.Execute(context.Background(), Request{Operation: "register", Epoch: s.epoch, RequestID: "retry", RecipeID: "pinned-fixture"}); resp.Error == "" {
		t.Fatal("continued after uncertain durability")
	}
}

func TestServiceRestartQuarantinesIncompleteGenerationsWithoutByteCredit(t *testing.T) {
	for _, state := range []string{"preparing", "consuming", "ready", "deleted"} {
		t.Run(state, func(t *testing.T) {
			s := serviceFixture(t, t.TempDir())
			h := serviceCall(t, s, Request{Operation: "register", RequestID: "r", RecipeID: "pinned-fixture"}).Home
			h = serviceCall(t, s, Request{Operation: "begin", RequestID: "b", HomeID: h.ID, Revision: h.Revision, RunID: "run", Intent: "launch"}).Home
			s.journal.Homes[h.ID].Generations[1].Artifacts[Catalog] = state
			path := filepath.Join(s.generationPath(h.ID, 1), "uncertain")
			if err := os.WriteFile(path, []byte("preserve bytes"), 0600); err != nil {
				t.Fatal(err)
			}
			if err := s.persist(); err != nil {
				t.Fatal(err)
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			s = serviceFixture(t, s.config.Root)
			h = serviceCall(t, s, Request{Operation: "query", HomeID: h.ID}).Home
			want := state
			if state == "preparing" || state == "consuming" {
				want = "quarantined"
			}
			if h.Generations[1].Artifacts[Catalog] != want {
				t.Fatal("unsafe recovery state")
			}
			resp := s.Execute(context.Background(), Request{Operation: "reclaim", Epoch: s.epoch, RequestID: "gc", HomeID: h.ID, Generation: 1, Revision: h.Revision, Artifact: Catalog})
			if resp.Error == "" || resp.Outcome.Removed || resp.Outcome.Bytes != 0 {
				t.Fatal("invented deletion/credit after restart")
			}
			data, err := os.ReadFile(path)
			if err != nil || string(data) != "preserve bytes" {
				t.Fatal("recovery altered remnants")
			}
		})
	}
}

func TestServiceSecondCycleSurvivesRestartWithoutRevivingRetiredLease(t *testing.T) {
	s := serviceFixture(t, t.TempDir())
	h := serviceCall(t, s, Request{Operation: "register", RequestID: "r", RecipeID: "pinned-fixture"}).Home
	for _, id := range []string{"one", "two"} {
		h = serviceCall(t, s, Request{Operation: "begin", RequestID: "b-" + id, HomeID: h.ID, Revision: h.Revision, RunID: id, Intent: "launch"}).Home
		g := h.Generations[h.Current]
		h = serviceCall(t, s, Request{Operation: "end", RequestID: "e-" + id, HomeID: h.ID, Revision: h.Revision, Generation: h.Current, LeaseID: g.LeaseID, RunID: id}).Home
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s = serviceFixture(t, s.config.Root)
	h = serviceCall(t, s, Request{Operation: "query", HomeID: h.ID}).Home
	g := h.Generations[h.Current]
	h = serviceCall(t, s, Request{Operation: "reconcile", RequestID: "rc", HomeID: h.ID, Revision: h.Revision, Generation: h.Current, LeaseID: g.LeaseID, RunID: g.RunID}).Home
	h = serviceCall(t, s, Request{Operation: "begin", RequestID: "b-three", HomeID: h.ID, Revision: h.Revision, RunID: "three", Intent: "restore"}).Home
	if h.Current != 3 {
		t.Fatal("could not reserve third generation")
	}
}

func TestServiceRollbackAndCorruptRegistryNeverRewriteStoredState(t *testing.T) {
	s := serviceFixture(t, t.TempDir())
	serviceCall(t, s, Request{Operation: "register", RequestID: "r", RecipeID: "pinned-fixture"})
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(s.config.Root, "registry.json")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	readonly, err := openService(ServiceConfig{Root: s.config.Root, RecipeID: "pinned-fixture", ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	resp := readonly.Execute(context.Background(), Request{Operation: "register", Epoch: readonly.epoch, RequestID: "new", RecipeID: "pinned-fixture"})
	if resp.Error == "" {
		t.Fatal("rollback enabled registration")
	}
	readonly.Close()
	after, err := os.ReadFile(path)
	if err != nil || string(before) != string(after) {
		t.Fatal("read-only rollback changed registry")
	}
	if err := os.WriteFile(path, []byte(`{"Version":99}`), 0600); err != nil {
		t.Fatal(err)
	}
	if bad, err := openService(s.config); err == nil {
		bad.Close()
		t.Fatal("unknown schema adopted")
	}
	after, err = os.ReadFile(path)
	if err != nil || string(after) != `{"Version":99}` {
		t.Fatal("corrupt state reset")
	}
}

func TestServiceAdmissionLimitsPreserveJournalAndRetainedData(t *testing.T) {
	for _, kind := range []string{"homes", "requests", "quarantine"} {
		t.Run(kind, func(t *testing.T) {
			s := serviceFixture(t, t.TempDir())
			h := serviceCall(t, s, Request{Operation: "register", RequestID: "r", RecipeID: "pinned-fixture"}).Home
			switch kind {
			case "homes":
				for i := 1; i < 64; i++ {
					id := nonce()
					s.journal.Homes[id] = &Home{ID: id, RecipeID: s.config.RecipeID, Revision: 1, Generations: map[uint64]*Generation{}}
				}
			case "requests":
				for i := 1; i < 1024; i++ {
					s.journal.Requests[nonce()] = completedRequest{}
				}
			case "quarantine":
				for i := 1; i <= 16; i++ {
					n := uint64(i)
					s.journal.Homes[h.ID].Generations[n] = &Generation{ID: n, Lifecycle: "terminal", RunID: nonce(), LeaseID: nonce(), Artifacts: map[string]string{Catalog: "quarantined"}}
					s.journal.Homes[h.ID].Current = n
				}
			}
			marker := filepath.Join(s.config.Root, "retained-unknown")
			if err := os.WriteFile(marker, []byte("never quota-evict uncertain data"), 0600); err != nil {
				t.Fatal(err)
			}
			if err := s.persist(); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(s.config.Root, "registry.json")
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			resp := s.Execute(context.Background(), Request{Operation: "register", RequestID: "new", Epoch: s.epoch, RecipeID: s.config.RecipeID})
			if resp.Error == "" {
				t.Fatal("capacity admitted new registration")
			}
			after, err := os.ReadFile(path)
			if err != nil || string(after) != string(before) {
				t.Fatal("admission refusal mutated journal", err)
			}
			if data, err := os.ReadFile(marker); err != nil || string(data) != "never quota-evict uncertain data" {
				t.Fatal("capacity evicted retained bytes", err)
			}
			if resp := s.Execute(context.Background(), Request{Operation: "result", RequestID: "r"}); resp.Error != "" {
				t.Fatal("capacity disabled committed result query", resp.Error)
			}
		})
	}
}

func TestServiceOversizedJournalFailsWithoutRewrite(t *testing.T) {
	s := serviceFixture(t, t.TempDir())
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(s.config.Root, "registry.json")
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	data := append(bytes.Repeat([]byte(" "), 4<<20), original...)
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	if opened, err := openService(s.config); err == nil {
		opened.Close()
		t.Fatal("oversized journal accepted")
	}
	after, err := os.ReadFile(path)
	if err != nil || len(after) != len(data) {
		t.Fatal("oversized journal changed", err)
	}
}
