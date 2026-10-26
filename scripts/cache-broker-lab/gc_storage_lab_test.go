//go:build cachebrokerlab && (darwin || windows)

package daemon

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/internal/daemon/cachebrokerlab"
)

func nativeStorageClaim(t *testing.T, s *serviceLab, generation uint64, artifact, want string) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(s.state, "registry.json"))
	if err != nil {
		t.Fatal(err)
	}
	var registry struct {
		Storage *cachebrokerlab.StorageLayout
		Slots   map[string]struct{ HomeID, RunID, LeaseID, State string }
	}
	if err := json.Unmarshal(data, &registry); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(registry.Storage, s.storage) {
		t.Fatal("storage registry changed across restart")
	}
	claim := registry.Slots[s.acquisitionStage(t, generation, artifact)]
	g := s.home.Generations[generation]
	if claim.HomeID != s.home.ID || claim.LeaseID != g.LeaseID || claim.RunID != g.RunID || claim.State != want {
		t.Fatalf("wrong retained slot binding: %+v, want %s", claim, want)
	}
	t.Logf("STORAGE_SLOT generation=%d artifact=%s state=%s binding_verified=true budget_bytes=%d", generation, artifact, want, registry.Storage.BudgetBytes)
}

func storageFileHash(t *testing.T, path string) string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	h := sha256.New()
	_, copyErr := io.Copy(h, f)
	closeErr := f.Close()
	if copyErr != nil || closeErr != nil {
		t.Fatal(copyErr, closeErr)
	}
	return fmt.Sprintf("%x", h.Sum(nil))
}

func fillNativeStorage(t *testing.T, path string) int64 {
	t.Helper()
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	block := make([]byte, 1<<20)
	if _, err := rand.Read(block); err != nil {
		t.Fatal(err)
	}
	var written int64
	for nativeStorageAvailable(t, filepath.Dir(path)) >= 12<<20 {
		if written >= 128<<20 {
			t.Fatal("registered physical capacity did not bound writes")
		}
		n, err := f.Write(block)
		if err != nil || n != len(block) {
			t.Fatal("fill stopped before admission floor", n, err)
		}
		written += int64(n)
		if err := f.Sync(); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if written == 0 {
		t.Fatal("volume was exhausted before test")
	}
	return written
}

func TestBrokerNativeStorageExhaustion(t *testing.T) {
	for _, kind := range []string{"staging", "retention", "logs"} {
		t.Run(kind, func(t *testing.T) {
			s := newServiceLab(t)
			s.register(t)
			root := s.begin(t, "launch")
			s.prepare(t, cachebrokerlab.Catalog)
			nativeStorageClaim(t, s, 1, cachebrokerlab.Catalog, "retained")
			cache := filepath.Join(root, filepath.FromSlash(cachebrokerlab.Catalog))
			beforeCache := labTreeSnapshot(t, cache)
			outside := filepath.Join(s.cfg.Root, "result-"+filepath.Base(filepath.Dir(s.state)))
			labWrite(t, outside, "task result outside aggregate quota")
			dir := s.state
			if kind == "staging" {
				dir = s.acquisitionStage(t, 2, cachebrokerlab.Catalog)
			}
			if kind == "logs" {
				dir = s.control
			}
			path := filepath.Join(dir, "retained-unknown-budget-fill")
			written := fillNativeStorage(t, path)
			before := storageFileHash(t, path)
			journalPath := filepath.Join(s.state, "registry.json")
			journal, err := os.ReadFile(journalPath)
			if err != nil {
				t.Fatal(err)
			}
			denyBudget := func(op string) {
				r := s.request(op)
				r.Artifact = cachebrokerlab.Marketplaces
				response, err := s.client.Call(context.Background(), r)
				if err != nil || !strings.Contains(response.Error, "aggregate storage budget exhausted") || response.Receipt != nil || response.Outcome.Bytes != 0 || response.Outcome.Removed {
					t.Fatal("budget admission did not fail closed", op, err, response)
				}
			}
			for _, op := range []string{"register", "begin", "prepare"} {
				denyBudget(op)
			}
			after, err := os.ReadFile(journalPath)
			if err != nil || !bytes.Equal(journal, after) {
				t.Fatal("refused admission mutated journal", err)
			}
			s.restart(t)
			if s.home.Generations[1].Lifecycle != "unknown" {
				t.Fatal("restart restored lifecycle proof")
			}
			nativeStorageClaim(t, s, 1, cachebrokerlab.Catalog, "retained")
			for _, op := range []string{"register", "begin", "prepare"} {
				denyBudget(op)
			}
			r := s.request("reclaim")
			r.Artifact = cachebrokerlab.Catalog
			s.deny(t, r)
			s.call(t, s.request("reconcile"))
			denyBudget("begin")
			if before != storageFileHash(t, path) || beforeCache != labTreeSnapshot(t, cache) {
				t.Fatal("retained cache or unknown fill changed")
			}
			result, err := os.ReadFile(outside)
			if err != nil || string(result) != "task result outside aggregate quota" {
				t.Fatal("task result changed", err)
			}
			t.Logf("STORAGE_EXHAUSTED kind=%s written=%d retained_sha256=%s available=%d restart_denied=true no_eviction=true old_proof_not_adopted=true task_result_preserved=true", kind, written, before, nativeStorageAvailable(t, dir))
		})
	}
}

func TestBrokerNativeStorageRestartRefusesChangedVolume(t *testing.T) {
	s := newServiceLab(t)
	s.register(t)
	s.begin(t, "restore")
	s.prepare(t, cachebrokerlab.Marketplaces)
	s.client.Close()
	s.stop()
	data, err := os.ReadFile(filepath.Join(s.control, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	journalPath := filepath.Join(s.state, "registry.json")
	before, err := os.ReadFile(journalPath)
	if err != nil {
		t.Fatal(err)
	}
	replacement := nativeProducerVolume(t, s.cfg)
	for _, kind := range []string{"replacement-volume", "capacity", "missing-volume", "changed-slot", "changed-budget", "missing-registry"} {
		t.Run(kind, func(t *testing.T) {
			var cfg struct{ Service cachebrokerlab.ServiceConfig }
			if err := json.Unmarshal(data, &cfg); err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "replacement-volume":
				cfg.Service.Storage.Volumes[1].Root = replacement
			case "capacity":
				cfg.Service.Storage.Volumes[1].CapacityBytes--
			case "missing-volume":
				cfg.Service.Storage.Volumes[1].Root = filepath.Join(s.cfg.Root, "missing-volume")
			case "changed-slot":
				cfg.Service.Storage.Slots[0].Generation = 3
			case "changed-budget":
				cfg.Service.Storage.BudgetBytes++
			case "missing-registry":
				cfg.Service.Storage = nil
			}
			service, err := cachebrokerlab.OpenService(cfg.Service)
			if err == nil {
				service.Close()
				t.Fatal("changed storage adopted")
			}
			after, readErr := os.ReadFile(journalPath)
			if readErr != nil || !bytes.Equal(before, after) {
				t.Fatal("failed restart changed registry", readErr)
			}
			t.Logf("STORAGE_RESTART_REFUSAL kind=%s preserved=true reason=%s", kind, err)
		})
	}
}

func TestBrokerNativeStorageReservedCrash(t *testing.T) {
	s := newServiceLab(t)
	s.register(t)
	root := s.begin(t, "launch")
	stage := s.acquisitionStage(t, 1, cachebrokerlab.Catalog)
	labWrite(t, filepath.Join(stage, "unknown.txt"), "unknown stage before launch")
	before := labTreeSnapshot(t, stage)
	fault, err := json.Marshal(map[string]any{"ID": "reserved-slot-crash", "Point": "storage.reserved", "Mode": "exit", "After": 1})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.control, "fault.json"), fault, 0600); err != nil {
		t.Fatal(err)
	}
	r := s.request("prepare")
	r.Artifact = cachebrokerlab.Catalog
	if _, err := s.client.Call(context.Background(), r); err == nil {
		t.Fatal("reserved-slot crash did not fire")
	}
	nativeWaitServiceTree(t, s.control)
	log, err := os.ReadFile(filepath.Join(s.control, "service.log"))
	if err != nil || !strings.Contains(string(log), "FAULT storage.reserved exit") || strings.Contains(string(log), "SERVICE_PRODUCER") {
		t.Fatal("reservation did not precede producer", err, string(log))
	}
	s.restart(t)
	nativeStorageClaim(t, s, 1, cachebrokerlab.Catalog, "quarantined")
	if s.home.Generations[1].Lifecycle != "unknown" || s.home.Generations[1].Artifacts[cachebrokerlab.Catalog] != "quarantined" {
		t.Fatal("reserved slot adopted")
	}
	s.deny(t, r)
	if n, b := s.gcExpect(t, root, "direct", cachebrokerlab.Catalog, true); n != 0 || b != 0 {
		t.Fatal(n, b)
	}
	s.call(t, s.request("reconcile"))
	s.begin(t, "restore")
	s.prepare(t, cachebrokerlab.Catalog)
	nativeStorageClaim(t, s, 2, cachebrokerlab.Catalog, "retained")
	nativeStorageClaim(t, s, 1, cachebrokerlab.Catalog, "quarantined")
	if before != labTreeSnapshot(t, stage) {
		t.Fatal("new generation evicted old unknown stage")
	}
	t.Log("STORAGE_RESERVED_CRASH before_factory=true old_slot_quarantined=true new_generation_separate=true unknown_retained=true")
}
