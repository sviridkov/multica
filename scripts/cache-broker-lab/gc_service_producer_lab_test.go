//go:build cachebrokerlab && (darwin || windows)

package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/daemon/cachebrokerlab"
)

func (s *serviceLab) acquisitionStage(t *testing.T, generation uint64, artifact string) string {
	t.Helper()
	for _, slot := range s.stages {
		if slot.Generation == generation && slot.Artifact == artifact {
			return slot.Stage
		}
	}
	t.Fatal("missing acquisition stage")
	return ""
}

func serviceAcquisitionPreserved(t *testing.T, root, stage, other string) {
	t.Helper()
	for path, want := range map[string]string{
		filepath.Join(root, "workspace", "result.txt"):                "task result outside staging quota",
		filepath.Join(root, filepath.FromSlash(other), "unknown.txt"): "unknown other cache",
		filepath.Join(stage, "unknown.txt"):                           "unclassified staging bytes",
	} {
		data, err := os.ReadFile(path)
		if err != nil || string(data) != want {
			t.Fatalf("retained sentinel changed: %s: %v", path, err)
		}
	}
}

func TestBrokerNativeStagedServicePublicationFailures(t *testing.T) {
	points := []string{"publication.after-copy", "publication.after-rename", "publication.before-commit", "publication.after-commit", "prepare.before-commit", "prepare.after-commit"}
	for _, mode := range []string{"exit", "error"} {
		for _, route := range []string{"direct", "walk"} {
			for _, artifact := range []string{cachebrokerlab.Marketplaces, cachebrokerlab.Catalog} {
				for _, point := range points {
					t.Run(mode+"/"+route+"/"+artifact+"/"+point, func(t *testing.T) {
						s := newServiceLab(t)
						s.register(t)
						other := cachebrokerlab.Catalog
						if artifact == other {
							other = cachebrokerlab.Marketplaces
						}
						retained := map[string]string{}
						for generation := uint64(1); generation <= 2; generation++ {
							root := s.begin(t, "restore")
							stage := s.acquisitionStage(t, generation, artifact)
							labWrite(t, filepath.Join(root, "workspace", "result.txt"), "task result outside staging quota")
							labWrite(t, filepath.Join(root, filepath.FromSlash(other), "unknown.txt"), "unknown other cache")
							r := s.request("prepare")
							r.Artifact = artifact
							faultID := fmt.Sprintf("%s-%s-%d", mode, point, generation)
							data, err := json.Marshal(map[string]any{"ID": faultID, "Point": point, "Mode": mode, "After": 1})
							if err != nil {
								t.Fatal(err)
							}
							if err = os.WriteFile(filepath.Join(s.control, "fault.json"), data, 0600); err != nil {
								t.Fatal(err)
							}
							resp, callErr := s.client.Call(context.Background(), r)
							if mode == "exit" {
								if callErr == nil {
									t.Fatal("armed service exit did not occur")
								}
								nativeWaitServiceTree(t, s.control)
							} else if callErr != nil || resp.Error == "" || resp.Receipt != nil || resp.Outcome.Removed || resp.Outcome.Bytes != 0 {
								t.Fatal("publication IO error exposed success", callErr, resp)
							}
							log, err := os.ReadFile(filepath.Join(s.control, "service.log"))
							if err != nil || !strings.Contains(string(log), "FAULT "+point+" "+mode) {
								t.Fatal("fault did not reach requested publication boundary", err)
							}
							s.verifyAcquisitionIdentity(t, generation, artifact)
							// All six boundaries follow successful leader exit and native proof.
							// An earlier validation refusal cannot count as a crash-matrix pass.
							var records map[string]cachebrokerlab.Receipt
							journal, err := os.ReadFile(filepath.Join(root, "journal.json"))
							if err != nil {
								t.Fatal(err)
							}
							if err = json.Unmarshal(journal, &records); err != nil {
								t.Fatal(err)
							}
							wantReceipt := "unproven"
							if point == "publication.after-commit" || strings.HasPrefix(point, "prepare.") {
								wantReceipt = "committed"
							}
							if records[artifact].State != wantReceipt || records[artifact].Generation != generation {
								t.Fatal("wrong receipt commit boundary", records[artifact])
							}
							partials, err := filepath.Glob(filepath.Join(root, ".publication-*"))
							if err != nil {
								t.Fatal(err)
							}
							target := filepath.Join(root, filepath.FromSlash(artifact))
							if point == "publication.after-copy" {
								if len(partials) != 1 {
									t.Fatal("missing protected partial publication")
								}
								if _, err := os.Lstat(target); !os.IsNotExist(err) {
									t.Fatal("copy fault published a final cache", err)
								}
								retained[partials[0]] = labTreeSnapshot(t, partials[0])
							} else {
								if len(partials) != 0 {
									t.Fatal("rename did not consume partial name")
								}
								retained[target] = labTreeSnapshot(t, target)
							}
							retained[stage] = labTreeSnapshot(t, stage)
							s.restart(t)
							wantSlot := "quarantined"
							if point == "prepare.after-commit" {
								wantSlot = "retained"
							}
							nativeStorageClaim(t, s, generation, artifact, wantSlot)
							g := s.home.Generations[generation]
							wantState := "quarantined"
							if point == "prepare.after-commit" {
								wantState = "ready"
							}
							if g.Lifecycle != "unknown" || g.Artifacts[artifact] != wantState {
								t.Fatal("recovery adopted ambiguous publication", g)
							}
							result, err := s.client.Call(context.Background(), cachebrokerlab.Request{Operation: "result", RequestID: r.RequestID})
							if err != nil {
								t.Fatal(err)
							}
							if wantState == "ready" {
								if result.Error != "" || result.Receipt == nil || result.Receipt.State != "committed" || result.Receipt.Generation != generation {
									t.Fatal("lost durable acquisition outcome", result)
								}
							} else if result.Error == "" || result.Receipt != nil || result.Outcome.Bytes != 0 {
								t.Fatal("uncertain acquisition invented a successful result", result)
							}
							s.deny(t, r) // Old epoch/lease cannot relaunch, even with its original request ID.
							replay := s.request("prepare")
							replay.Artifact = artifact
							s.deny(t, replay)
							if n, b := s.gcExpect(t, root, route, artifact, true); n != 0 || b != 0 {
								t.Fatal("recovery credited an unknown lifecycle", n, b)
							}
							for path, snapshot := range retained {
								if snapshot != labTreeSnapshot(t, path) {
									t.Fatal("restart/replay/GC changed retained bytes", path)
								}
							}
							serviceAcquisitionPreserved(t, root, stage, other)
							s.call(t, s.request("reconcile"))
							time.Sleep(1100 * time.Millisecond)
							if wantState == "quarantined" {
								if n, b := s.gcExpect(t, root, route, artifact, true); n != 0 || b != 0 {
									t.Fatal("quarantine was reclaimed")
								}
							} else {
								if n, b := s.gc(t, root, route, artifact); n != 1 || b <= 0 {
									t.Fatal("committed outcome unavailable after trusted reconciliation")
								}
								delete(retained, target)
							}
							t.Logf("STAGED_SERVICE_FAULT point=%s mode=%s route=%s artifact=%s generation=%d state=%s receipt=%s old_lease_denied=true replay_denied=true retained=true", point, mode, route, artifact, generation, wantState, wantReceipt)
						}
						for path, snapshot := range retained {
							if snapshot != labTreeSnapshot(t, path) {
								t.Fatal("replacement generation changed retained evidence", path)
							}
						}
					})
				}
			}
		}
	}
}
