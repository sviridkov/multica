package cachebrokerlab

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestOrdinaryTaskCannotOpenBroker(t *testing.T) {
	if runtime.GOOS != "windows" && os.Geteuid() == 0 {
		t.Skip("privileged fixture runs separately")
	}
	root := t.TempDir()
	if _, err := Open(Policy{Root: root, Producer: filepath.Join(root, "producer"), Git: filepath.Join(root, "git"), HomeID: "task"}); err == nil {
		t.Fatal("same-UID caller became a protected broker")
	}
}

func TestReclaimRejectsUnprovenAndIneligible(t *testing.T) {
	for _, artifact := range []string{Marketplaces, Catalog, "cache", "../marketplaces"} {
		for _, state := range []string{"", "unproven", "consumed", "committed"} {
			b := &Broker{records: map[string]Receipt{artifact: {Schema: 1, Artifact: artifact, State: state}}, policy: Policy{}}
			out, err := b.Reclaim(context.Background(), b.records[artifact])
			if err != nil || out.Removed || out.Bytes != 0 || out.Reason == "" {
				t.Fatalf("artifact=%q state=%q: %+v %v", artifact, state, out, err)
			}
		}
	}
}
func TestReclaimRejectsReplayBeforeFilesystemAccess(t *testing.T) {
	b := &Broker{policy: Policy{HomeID: "home-a", ArtifactTTLSeconds: 1, IdleSeconds: 2, Terminal: true}, records: map[string]Receipt{Marketplaces: {Schema: 1, Artifact: Marketplaces, HomeID: "home-a", State: "committed", Generation: 2}}}
	for _, r := range []Receipt{
		{Artifact: Marketplaces, HomeID: "home-a", State: "committed", Generation: 1},
		{Schema: 1, Artifact: Marketplaces, HomeID: "home-b", State: "committed", Generation: 2},
		{Schema: 1, Artifact: Catalog, HomeID: "home-a", State: "committed", Generation: 2},
	} {
		out, err := b.Reclaim(context.Background(), r)
		if err != nil || out.Removed || out.Bytes != 0 || out.Reason == "" {
			t.Fatalf("replayed receipt: %+v %v", out, err)
		}
	}
}

func TestReceiptStatesWithEligibleLifecycle(t *testing.T) {
	for _, artifact := range []string{Marketplaces, Catalog} {
		for _, state := range []string{"", "unproven", "consumed"} {
			r := Receipt{Schema: 1, Artifact: artifact, State: state}
			b := &Broker{policy: Policy{ArtifactTTLSeconds: 1, IdleSeconds: 2, Terminal: true}, records: map[string]Receipt{artifact: r}}
			out, err := b.Reclaim(context.Background(), r)
			if err != nil || out.Removed || out.Bytes != 0 || out.Reason != "missing, stale, replayed or mismatched receipt" {
				t.Fatalf("artifact=%q state=%q: %+v %v", artifact, state, out, err)
			}
		}
	}
}

func TestEachLifecycleGuardRejectsCommittedReceipt(t *testing.T) {
	cases := map[string]func(*Policy){
		"ttl-disabled":    func(p *Policy) { p.ArtifactTTLSeconds = 0 },
		"too-recent":      func(p *Policy) { p.IdleSeconds = 0 },
		"active":          func(p *Policy) { p.Active = true },
		"nonterminal":     func(p *Policy) { p.Terminal = false },
		"local-directory": func(p *Policy) { p.LocalDirectory = true },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			p := Policy{ArtifactTTLSeconds: 1, IdleSeconds: 2, Terminal: true}
			mutate(&p)
			r := Receipt{Schema: 1, Artifact: Catalog, State: "committed"}
			b := &Broker{policy: p, records: map[string]Receipt{Catalog: r}}
			out, err := b.Reclaim(context.Background(), r)
			if err != nil || out.Removed || out.Bytes != 0 || out.Reason != "protected lifecycle policy denies reclamation" {
				t.Fatalf("%+v %v", out, err)
			}
		})
	}
}
func TestReceiptDoesNotExposeJournalEntries(t *testing.T) {
	b := &Broker{records: map[string]Receipt{Catalog: {Entries: []Entry{{Digest: "original"}}}}}
	r := b.Receipt(Catalog)
	r.Entries[0].Digest = "forged"
	if b.Receipt(Catalog).Entries[0].Digest != "original" {
		t.Fatal("caller mutated authoritative receipt")
	}
}

func TestReturnedReceiptCopyDoesNotAliasEntries(t *testing.T) {
	original := Receipt{Entries: []Entry{{Digest: "producer"}}}
	returned := cloneReceipt(original)
	returned.Entries[0].Digest = "forged"
	if original.Entries[0].Digest != "producer" {
		t.Fatal("receipt return aliases authority")
	}
}
func TestCatalogRequiresItsOwnAcquisition(t *testing.T) {
	p := Policy{SourceOID: "0123456789012345678901234567890123456789"}
	forged := SourceIdentity{Schema: 1, Kind: "catalog", Revision: p.SourceOID, Endpoint: "http://127.0.0.1:1234", Scope: "global", CacheKey: "a.json", Written: true, ReadVerified: true}
	if validateSource(p, Catalog, forged) == nil {
		t.Fatal("Git OID accepted as catalog acquisition")
	}
	forged.Revision = "0123456789012345678901234567890123456789012345678901234567890123"
	if err := validateSource(p, Catalog, forged); err != nil {
		t.Fatal(err)
	}
	forged.Written = false
	if validateSource(p, Catalog, forged) == nil {
		t.Fatal("fetch success accepted despite failed write")
	}
}
