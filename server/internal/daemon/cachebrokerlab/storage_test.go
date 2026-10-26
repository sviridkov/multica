package cachebrokerlab

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func storageFixture(t *testing.T) (ServiceConfig, map[string]storageReading, storageProbe) {
	t.Helper()
	base := t.TempDir()
	data, stage := filepath.Join(base, "data"), filepath.Join(base, "stage")
	root, control := filepath.Join(data, "state"), filepath.Join(data, "control")
	layout := &StorageLayout{BudgetBytes: 256 << 20, Control: control, Volumes: []StorageVolume{
		{Root: data, Identity: "data-uuid", CapacityBytes: 128 << 20, LimitBytes: 128 << 20, MinFreeBytes: 16 << 20},
		{Root: stage, Identity: "stage-uuid", CapacityBytes: 128 << 20, LimitBytes: 128 << 20, MinFreeBytes: 16 << 20},
	}}
	readings := map[string]storageReading{}
	for _, v := range layout.Volumes {
		readings[v.Root] = storageReading{Identity: v.Identity, Capacity: v.CapacityBytes, Available: 100 << 20}
	}
	for _, p := range []string{root, control, stage} {
		if err := os.MkdirAll(p, 0700); err != nil {
			t.Fatal(err)
		}
	}
	for g := uint64(1); g <= 2; g++ {
		for _, artifact := range []string{Marketplaces, Catalog} {
			path, err := os.MkdirTemp(stage, "slot-")
			if err != nil {
				t.Fatal(err)
			}
			layout.Slots = append(layout.Slots, StorageSlot{Generation: g, Artifact: artifact, Stage: path, Volume: stage})
		}
	}
	probe := func(path string) (storageReading, error) {
		if r, ok := readings[path]; ok {
			return r, nil
		}
		for _, volume := range []string{data, stage} {
			if within(volume, path) {
				return readings[volume], nil
			}
		}
		return storageReading{}, errors.New("unknown filesystem")
	}
	return ServiceConfig{Root: root, RecipeID: "fixture", Storage: layout}, readings, probe
}

func TestStorageRejectsUnknownOrOvercommittedVolumes(t *testing.T) {
	for _, change := range []string{"identity", "capacity-up", "capacity-down", "query-error", "budget", "alias", "nested", "slot-overlap", "slot-mount", "state-mount", "control-outside", "staging-exhausted", "data-exhausted"} {
		t.Run(change, func(t *testing.T) {
			cfg, readings, probe := storageFixture(t)
			if err := cfg.Storage.check(cfg.Root, true, probe); err != nil {
				t.Fatal(err)
			}
			v := cfg.Storage.Volumes[1]
			r := readings[v.Root]
			switch change {
			case "identity":
				r.Identity = "replacement-disk"
			case "capacity-up":
				r.Capacity++
			case "capacity-down":
				r.Capacity--
			case "query-error":
				probe = func(string) (storageReading, error) { return storageReading{}, errors.New("native query failed") }
			case "budget":
				cfg.Storage.BudgetBytes--
			case "alias":
				cfg.Storage.Volumes[1].Identity = cfg.Storage.Volumes[0].Identity
			case "nested":
				cfg.Storage.Volumes[1].Root = filepath.Join(cfg.Storage.Volumes[0].Root, "nested")
			case "slot-overlap":
				cfg.Storage.Slots[1].Stage = cfg.Storage.Slots[0].Stage
			case "slot-mount":
				readings[cfg.Storage.Slots[0].Stage] = storageReading{Identity: "other", Capacity: 128 << 20}
			case "state-mount":
				readings[cfg.Root] = storageReading{Identity: "other", Capacity: 128 << 20}
			case "control-outside":
				cfg.Storage.Control = filepath.Join(t.TempDir(), "unbounded")
			case "staging-exhausted":
				r.Available = 15 << 20
			case "data-exhausted":
				data := cfg.Storage.Volumes[0].Root
				x := readings[data]
				x.Available = 15 << 20
				readings[data] = x
			}
			readings[v.Root] = r
			if err := cfg.Storage.check(cfg.Root, true, probe); err == nil {
				t.Fatal("unsafe storage admitted")
			}
		})
	}
}

func TestStorageExhaustionPreservesJournalAndRestartDoesNotEvict(t *testing.T) {
	cfg, readings, probe := storageFixture(t)
	s, err := openServiceWithStorage(cfg, probe)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	h := serviceCall(t, s, Request{Operation: "register", RequestID: "r", RecipeID: "fixture"}).Home
	h = serviceCall(t, s, Request{Operation: "begin", RequestID: "b", HomeID: h.ID, Revision: h.Revision, RunID: "run", Intent: "restore"}).Home
	unknown := filepath.Join(cfg.Root, "unknown-partial")
	if err := os.WriteFile(unknown, []byte("retain me"), 0600); err != nil {
		t.Fatal(err)
	}
	stage := cfg.Storage.Volumes[1].Root
	r := readings[stage]
	r.Available = 15 << 20
	readings[stage] = r
	journal := filepath.Join(cfg.Root, "registry.json")
	before, err := os.ReadFile(journal)
	if err != nil {
		t.Fatal(err)
	}
	for _, op := range []string{"register", "begin", "prepare"} {
		response := s.Execute(context.Background(), Request{Operation: op, Epoch: s.epoch, RequestID: "denied-" + op, HomeID: h.ID, Revision: h.Revision})
		if !strings.Contains(response.Error, "budget exhausted") || response.Outcome.Bytes != 0 || response.Receipt != nil {
			t.Fatal("exhausted admission", response)
		}
	}
	after, err := os.ReadFile(journal)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("denial changed durable state", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = openServiceWithStorage(cfg, probe)
	if err != nil {
		t.Fatal(err)
	}
	if s.journal.Homes[h.ID].Generations[1].Lifecycle != "unknown" {
		t.Fatal("restart restored lifecycle proof")
	}
	response := s.Execute(context.Background(), Request{Operation: "register", Epoch: s.epoch, RequestID: "after-restart"})
	if !strings.Contains(response.Error, "budget exhausted") {
		t.Fatal(response)
	}
	data, err := os.ReadFile(unknown)
	if err != nil || string(data) != "retain me" {
		t.Fatal("retained data evicted", err)
	}
}

func TestStorageRestartRejectsChangedBootstrapBeforeWriting(t *testing.T) {
	for _, change := range []string{"volume", "capacity", "slot", "budget", "missing-storage"} {
		t.Run(change, func(t *testing.T) {
			cfg, readings, probe := storageFixture(t)
			s, err := openServiceWithStorage(cfg, probe)
			if err != nil {
				t.Fatal(err)
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(cfg.Root, "registry.json")
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			switch change {
			case "volume":
				r := readings[cfg.Storage.Volumes[1].Root]
				r.Identity = "replacement"
				readings[cfg.Storage.Volumes[1].Root] = r
			case "capacity":
				r := readings[cfg.Storage.Volumes[1].Root]
				r.Capacity--
				readings[cfg.Storage.Volumes[1].Root] = r
			case "slot":
				cfg.Storage.Slots[0].Stage = filepath.Join(cfg.Storage.Volumes[1].Root, "different-empty-slot")
			case "budget":
				cfg.Storage.BudgetBytes++
			case "missing-storage":
				cfg.Storage = nil
			}
			if reopened, err := openServiceWithStorage(cfg, probe); err == nil {
				reopened.Close()
				t.Fatal("changed bootstrap adopted")
			}
			after, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(before, after) {
				t.Fatal("refusal rewrote registry", err)
			}
		})
	}
}

func TestStorageReservationPrecedesFactoryAndCannotBeReusedAfterRestart(t *testing.T) {
	cfg, _, probe := storageFixture(t)
	s, err := openServiceWithStorage(cfg, probe)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	h := serviceCall(t, s, Request{Operation: "register", RequestID: "r", RecipeID: "fixture"}).Home
	h = serviceCall(t, s, Request{Operation: "begin", RequestID: "b", HomeID: h.ID, Revision: h.Revision, RunID: "run", Intent: "restore"}).Home
	h = s.journal.Homes[h.ID]
	g := h.Generations[1]
	g.Artifacts[Catalog] = "preparing"
	want := errors.New("factory stopped after durable intent")
	s.config.Production = func(string, string, RunBinding) (ProductionPlan, func() error, error) {
		data, err := os.ReadFile(filepath.Join(cfg.Root, "registry.json"))
		if err != nil {
			t.Fatal(err)
		}
		var saved registry
		if err := json.Unmarshal(data, &saved); err != nil {
			t.Fatal(err)
		}
		if len(saved.Slots) != 1 {
			t.Fatal("factory launched without durable reservation")
		}
		for _, claim := range saved.Slots {
			if claim.State != "reserved" || claim.LeaseID != g.LeaseID {
				t.Fatal(claim)
			}
		}
		return ProductionPlan{}, nil, want
	}
	if _, err := s.acquire(context.Background(), nil, h, g, Catalog); !errors.Is(err, want) {
		t.Fatal(err)
	}
	// Do not persist the returned error: model process death before the caller's
	// commit. Restart must retain the durable reserved slot as quarantined.
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = openServiceWithStorage(cfg, probe)
	if err != nil {
		t.Fatal(err)
	}
	h = s.journal.Homes[h.ID]
	g = h.Generations[1]
	if g.Lifecycle != "unknown" || g.Artifacts[Catalog] != "quarantined" {
		t.Fatal(g)
	}
	for _, claim := range s.journal.Slots {
		if claim.State != "quarantined" {
			t.Fatal(claim)
		}
	}
	if _, err := s.reserveSlot(h, g, Catalog); err == nil {
		t.Fatal("recovered slot reissued")
	}
	serviceCall(t, s, Request{Operation: "reconcile", RequestID: "end", HomeID: h.ID, Revision: h.Revision, Generation: 1, LeaseID: g.LeaseID, RunID: g.RunID})
	h = s.journal.Homes[h.ID]
	serviceCall(t, s, Request{Operation: "begin", RequestID: "next", HomeID: h.ID, Revision: h.Revision, RunID: "new-run", Intent: "restore"})
	g = h.Generations[2]
	g.Artifacts[Catalog] = "preparing"
	if _, err := s.reserveSlot(h, g, Catalog); err != nil {
		t.Fatal(err)
	}
	if len(s.journal.Slots) != 2 {
		t.Fatal("new generation replaced old claim")
	}
}

func TestStorageAuthorityLossLatchesUntilRestart(t *testing.T) {
	cfg, readings, probe := storageFixture(t)
	s, err := openServiceWithStorage(cfg, probe)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	path := cfg.Storage.Volumes[1].Root
	old := readings[path]
	replacement := old
	replacement.Identity = "another-volume"
	readings[path] = replacement
	r := Request{Operation: "register", RequestID: "r", RecipeID: "fixture", Epoch: s.epoch}
	if response := s.Execute(context.Background(), r); response.Error == "" {
		t.Fatal("replacement accepted")
	}
	readings[path] = old
	if response := s.Execute(context.Background(), r); !strings.Contains(response.Error, "restart required") {
		t.Fatal("authority silently recovered", response)
	}
}

func TestStorageFactoryCannotSelectUnregisteredStage(t *testing.T) {
	for _, field := range []string{"stage", "volume", "capacity"} {
		t.Run(field, func(t *testing.T) {
			cfg, _, probe := storageFixture(t)
			s, err := openServiceWithStorage(cfg, probe)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			h := serviceCall(t, s, Request{Operation: "register", RequestID: "r", RecipeID: "fixture"}).Home
			serviceCall(t, s, Request{Operation: "begin", RequestID: "b", HomeID: h.ID, Revision: h.Revision, RunID: "run", Intent: "restore"})
			h = s.journal.Homes[h.ID]
			g := h.Generations[1]
			g.Artifacts[Catalog] = "preparing"
			released := false
			s.config.Production = func(_, _ string, binding RunBinding) (ProductionPlan, func() error, error) {
				var slot StorageSlot
				for _, x := range cfg.Storage.Slots {
					if x.Generation == 1 && x.Artifact == Catalog {
						slot = x
					}
				}
				plan := ProductionPlan{Stage: slot.Stage, Volume: slot.Volume, CapacityBytes: 128 << 20, Binding: binding}
				switch field {
				case "stage":
					plan.Stage += "-other"
				case "volume":
					plan.Volume = cfg.Storage.Volumes[0].Root
				case "capacity":
					plan.CapacityBytes++
				}
				return plan, func() error { released = true; return nil }, nil
			}
			// No broker exists: reaching StartProduction would panic, not pass.
			if _, err := s.acquire(context.Background(), nil, h, g, Catalog); err == nil || !strings.Contains(err.Error(), "protected slot registry") || !released {
				t.Fatal("unregistered launch accepted", err)
			}
		})
	}
}
