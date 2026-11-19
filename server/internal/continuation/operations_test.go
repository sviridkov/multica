package continuation

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

var fixtureHead = strings.Repeat("a", 40)
var fixtureBase = strings.Repeat("b", 40)
var fixtureCommit = strings.Repeat("c", 40)
var fixtureDigest = "sha256:" + strings.Repeat("a", 64)

func TestOperationSafetyMatrix(t *testing.T) {
	now := time.Now().UTC()
	merge := Operation{Kind: "merge", Repository: "org/repo", PR: 7, Head: fixtureHead, Base: fixtureBase}
	deploy := Operation{Kind: "deploy", Environment: "pilot", ReleaseDigest: fixtureDigest, ImageDigest: fixtureDigest, ConfigRevision: "rev7"}
	call := Operation{Kind: "call", DemoSession: "demo7", Attempt: "attempt1", Provider: "retell", ProviderOperationID: "call_7"}
	m := Observation{Known: true, ObservedAt: now, Head: fixtureHead, Base: fixtureBase, Merged: true, MergeCommit: fixtureCommit}
	d := Observation{Known: true, ObservedAt: now, ReleaseDigest: fixtureDigest, ImageDigest: fixtureDigest, ConfigRevision: "rev7", BootstrapReceipt: "bootstrap7", MigrationReceipt: "migrate7", DeployReceipt: "deploy7", Healthy: true}
	c := Observation{Known: true, ObservedAt: now, ProviderOperationID: "call_7", ProviderStatus: "completed"}
	cases := []struct {
		name        string
		op          Operation
		s           Observation
		state, next string
	}{
		{"merged before local ACK", merge, m, "complete", "none"},
		{"deploy already healthy", deploy, d, "complete", "none"},
		{"call already finished", call, c, "complete", "none"},
		{"timeout after submit", call, Observation{}, "hold", "reconcile_external_state"},
	}
	changed := m
	changed.Head = strings.Repeat("d", 40)
	cases = append(cases, struct {
		name        string
		op          Operation
		s           Observation
		state, next string
	}{"head changed", merge, changed, "needs_decision", "review_changed_head_or_base"})
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := decideOperation(tc.op, tc.s, now)
			if got.State != tc.state || got.Next != tc.next {
				t.Fatalf("%+v", got)
			}
		})
	}
	for _, stage := range []string{"bootstrap", "migration"} {
		s := d
		if stage == "bootstrap" {
			s.BootstrapReceipt = ""
		} else {
			s.MigrationReceipt = ""
		}
		if got := decideOperation(deploy, s, now); got.State != "hold" {
			t.Fatalf("unknown %s repeated: %+v", stage, got)
		}
	}
	d.Healthy = false
	if got := decideOperation(deploy, d, now); got.Next != "verify_health_only" {
		t.Fatal(got)
	}
	call.ProviderOperationID = ""
	if got := decideOperation(call, c, now); got.State != "hold" {
		t.Fatal("auto-redial without provider ID")
	}
	k1, err := call.key()
	must(t, err)
	call.ProviderOperationID = "call_7"
	k2, err := call.key()
	must(t, err)
	if k1 != k2 {
		t.Fatal("learning provider ID reset operation key")
	}
	call.Kind = "sms"
	sms, err := call.key()
	must(t, err)
	if sms == k1 {
		t.Fatal("SMS shares call key")
	}
	m.ObservedAt = now.Add(-11 * time.Minute)
	if decideOperation(merge, m, now).State != "hold" {
		t.Fatal("stale API result accepted")
	}
}

type observerFunc func(context.Context, Operation) (Observation, error)

func (f observerFunc) Observe(ctx context.Context, o Operation) (Observation, error) {
	return f(ctx, o)
}

func TestOperationCrashReceiptCallbacksAndProviderTimeout(t *testing.T) {
	f, m, c, _, _ := fixture(t)
	put(t, m, c, 0)
	ctx := context.Background()
	f.Cleanup(t, "DELETE FROM issue_continuation_operation WHERE issue_id=$1", c.IssueID)
	req := OperationRequest{Key: c.Key, Operation: Operation{Kind: "call", DemoSession: "demo", Attempt: c.IssueID, Provider: "retell", ProviderOperationID: "call_7"}, CallbackID: "callback7"}
	reads := 0
	reader := observerFunc(func(context.Context, Operation) (Observation, error) {
		reads++
		return Observation{Known: true, ObservedAt: time.Now().UTC(), ProviderOperationID: "call_7", ProviderStatus: "completed"}, nil
	})
	// The provider has succeeded; simulate local receipt failure, then restart.
	f.Exec(t, `CREATE FUNCTION fixture_operation_crash() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'fixture receipt failure'; END $$`)
	f.Exec(t, `CREATE TRIGGER fixture_operation_crash BEFORE INSERT ON issue_continuation_operation FOR EACH ROW EXECUTE FUNCTION fixture_operation_crash()`)
	t.Cleanup(func() {
		f.Exec(t, "DROP TRIGGER IF EXISTS fixture_operation_crash ON issue_continuation_operation")
		f.Exec(t, "DROP FUNCTION IF EXISTS fixture_operation_crash()")
	})
	if _, err := m.ReconcileOperation(ctx, req, reader); err == nil {
		t.Fatal("receipt failure not injected")
	}
	f.Exec(t, "DROP TRIGGER fixture_operation_crash ON issue_continuation_operation")
	restarted := &Monitor{Pool: m.Pool, ProjectID: m.ProjectID}
	r, err := restarted.ReconcileOperation(ctx, req, reader)
	must(t, err)
	if r.Decision.State != "complete" || r.Version != 1 || reads != 2 {
		t.Fatalf("reconcile external success: %+v reads=%d", r, reads)
	}
	// Duplicate callback after an ACK loss returns the original receipt even
	// though the caller's expected version is still zero.
	r, err = restarted.ReconcileOperation(ctx, req, reader)
	must(t, err)
	if len(r.CallbackIDs) != 1 || r.Version != 1 {
		t.Fatal("callback was not deduplicated")
	}
	req.CallbackID = ""
	req.ExpectedVersion = 1
	r, err = restarted.ReconcileOperation(ctx, req, observerFunc(func(context.Context, Operation) (Observation, error) {
		return Observation{}, errors.New("timeout including secret payload")
	}))
	must(t, err)
	if r.Completion == nil {
		t.Fatal("lost durable external completion receipt")
	}
	if r.Decision.State != "hold" {
		t.Fatal("timeout became repeat permission")
	}
	if f.Count(t, "SELECT count(*) FROM issue_continuation_operation WHERE issue_id=$1 AND record::text LIKE '%secret%'", c.IssueID) != 0 {
		t.Fatal("raw provider error persisted")
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestHTTPObserversOnlyReadAndVerifyIdentity(t *testing.T) {
	requests := 0
	reader := HTTPObserver{GitHubToken: "fixture-secret", RetellToken: "fixture-secret", Client: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		requests++
		if r.Method != "GET" || r.Body != nil {
			t.Fatal("external mutation")
		}
		var body string
		switch r.URL.Path {
		case "/repos/org/repo/pulls/7":
			body = `{"number":7,"state":"closed","merged":true,"merge_commit_sha":"` + fixtureCommit + `","head":{"sha":"` + fixtureHead + `"},"base":{"sha":"` + strings.Repeat("d", 40) + `","repo":{"full_name":"org/repo"}}}`
		case "/repos/org/repo/commits/" + fixtureCommit:
			body = `{"sha":"` + fixtureCommit + `","parents":[{"sha":"` + fixtureBase + `"}]}`
		case "/v2/get-call/call_7":
			body = `{"call_id":"call_7","call_status":"ended"}`
		default:
			t.Fatalf("unexpected provider read %s", r.URL.Path)
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
	})}}
	merge := Operation{Kind: "merge", Repository: "org/repo", PR: 7, Head: fixtureHead, Base: fixtureBase}
	s, err := reader.Observe(context.Background(), merge)
	must(t, err)
	if got := decideOperation(merge, s, time.Now()); got.State != "complete" {
		t.Fatalf("merge ancestry not reconciled: %+v", got)
	}
	call := Operation{Kind: "call", DemoSession: "demo", Attempt: "1", Provider: "retell", ProviderOperationID: "call_7"}
	s, err = reader.Observe(context.Background(), call)
	must(t, err)
	if s.ProviderStatus != "completed" {
		t.Fatal(s)
	}
	call.ProviderOperationID = ""
	if _, err = reader.Observe(context.Background(), call); err == nil {
		t.Fatal("unknown ID lookup allowed")
	}
	if requests != 3 {
		t.Fatal("unknown ID caused request")
	}
}
