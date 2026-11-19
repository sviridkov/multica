package continuation

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

func recoveryFixture(t *testing.T) (*testutil.Fixture, *Monitor, RecoveryRequest, string, string) {
	t.Helper()
	f, m, c, agent, runtime := fixture(t)
	f.Exec(t, "UPDATE issue SET status='in_progress',assignee_id=$2,assignee_type='agent' WHERE id=$1", c.IssueID, agent)
	f.Exec(t, "UPDATE agent SET model='fixture-model' WHERE id=$1", agent)
	f.Exec(t, "UPDATE agent_runtime SET status='online',last_seen_at=now() WHERE id=$1", runtime)
	f.Exec(t, "UPDATE agent_task_queue SET originator_user_id=$2,accountable_user_id=$2 WHERE id=$1", c.ReceiverTaskID, c.OwnerID)
	r := put(t, m, c, 0)
	req := RecoveryRequest{Key: c.Key, Generation: 1, ExpectedVersion: r.Version, OwnerID: c.OwnerID, OriginalTaskID: c.ReceiverTaskID,
		Admission: Admission{ObservedAt: time.Now().UTC(), Account: "fixture-account", Model: "fixture-model", RuntimeID: runtime, BindingVerified: true, Authenticated: true, ModelAvailable: true, AllRequiredWindowsAvailable: true, SlotAvailable: true},
		Proof:     PreStartProof{ObservedAt: time.Now().UTC(), Phase: "initialize", RetrySafe: true, ProcessTreeStopped: true, NoUsefulAction: true, NoExternalOperation: true, EnvironmentReady: true}}
	must(t, m.Pool.QueryRow(context.Background(), "SELECT revision FROM issue WHERE id=$1", c.IssueID).Scan(&req.IssueRevision))
	must(t, m.Pool.QueryRow(context.Background(), "SELECT updated_at FROM agent WHERE id=$1", agent).Scan(&req.Admission.AgentUpdatedAt))
	f.Cleanup(t, "DELETE FROM issue_continuation_recovery WHERE issue_id=$1", c.IssueID)
	f.Cleanup(t, "DELETE FROM agent_task_queue WHERE retry_of_task_id=$1", c.ReceiverTaskID)
	t.Setenv("MULTICA_CONTINUATION_RECOVERY", "true")
	return f, m, req, agent, runtime
}

func TestRecoveryRaceRestartAndExhaustion(t *testing.T) {
	f, m, req, _, _ := recoveryFixture(t)
	ctx := context.Background()
	var wg sync.WaitGroup
	results := make(chan RecoveryReceipt, 2)
	errs := make(chan error, 2)
	for range 2 {
		wg.Add(1)
		go func() { defer wg.Done(); r, e := m.RecoverPreStart(ctx, req); results <- r; errs <- e }()
	}
	wg.Wait()
	close(results)
	close(errs)
	for err := range errs {
		must(t, err)
	}
	var id string
	for r := range results {
		if id != "" && id != r.RetryTaskID {
			t.Fatal("duplicate retry")
		}
		id = r.RetryTaskID
	}
	if f.Count(t, "SELECT count(*) FROM agent_task_queue WHERE retry_of_task_id=$1", req.OriginalTaskID) != 1 {
		t.Fatal("expected one durable retry")
	}
	if f.Count(t, "SELECT count(*) FROM agent_task_queue WHERE id=$1 AND max_attempts=1 AND force_fresh_session AND session_id IS NULL", id) != 1 {
		t.Fatal("retry can recurse or resume ambiguous session")
	}
	// ACK lost: reconstruct the operator with stale version and stale admission.
	restarted := &Monitor{Pool: m.Pool, ProjectID: m.ProjectID}
	req.Admission.ObservedAt = time.Time{}
	r, err := restarted.RecoverPreStart(ctx, req)
	must(t, err)
	if r.RetryTaskID != id {
		t.Fatal("restart lost receipt")
	}
	f.Exec(t, "UPDATE agent_task_queue SET status='failed',completed_at=now() WHERE id=$1", id)
	r, err = restarted.RecoverPreStart(ctx, req)
	must(t, err)
	if r.State != "blocked_retry_exhausted" || r.OwnerID != req.OwnerID {
		t.Fatalf("exhaustion: %+v", r)
	}
	must(t, restarted.Reconcile(ctx, req.Key))
	state := current(t, m, req.Key)
	if state.State != "needs_reconcile" {
		t.Fatal("failed retry did not signal through detector")
	}
	if f.Count(t, "SELECT count(*) FROM issue_continuation_incident WHERE issue_id=$1", req.IssueID) != 1 {
		t.Fatal("expected one responsible incident")
	}
}

func TestRecoveryCrashBeforeReceiptRollsBackEnqueue(t *testing.T) {
	f, m, req, _, _ := recoveryFixture(t)
	ctx := context.Background()
	f.Exec(t, `CREATE FUNCTION fixture_recovery_crash() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'fixture crash before ACK'; END $$`)
	f.Exec(t, `CREATE TRIGGER fixture_recovery_crash BEFORE INSERT ON issue_continuation_recovery FOR EACH ROW EXECUTE FUNCTION fixture_recovery_crash()`)
	t.Cleanup(func() {
		f.Exec(t, "DROP TRIGGER IF EXISTS fixture_recovery_crash ON issue_continuation_recovery")
		f.Exec(t, "DROP FUNCTION IF EXISTS fixture_recovery_crash()")
	})
	if _, err := m.RecoverPreStart(ctx, req); err == nil {
		t.Fatal("injected crash did not fail")
	}
	if f.Count(t, "SELECT count(*) FROM agent_task_queue WHERE retry_of_task_id=$1", req.OriginalTaskID) != 0 {
		t.Fatal("unreceipted child survived rollback")
	}
	f.Exec(t, "DROP TRIGGER fixture_recovery_crash ON issue_continuation_recovery")
	_, err := m.RecoverPreStart(ctx, req)
	must(t, err)
}

func TestRecoveryHoldsAndReconnect(t *testing.T) {
	f, m, req, agent, runtime := recoveryFixture(t)
	ctx := context.Background()
	checks := []struct {
		name   string
		change func(*RecoveryRequest)
	}{
		{"retry_safe false", func(r *RecoveryRequest) { r.Proof.RetrySafe = false }},
		{"lease expiry not cleanup", func(r *RecoveryRequest) { r.Proof.ProcessTreeStopped = false }},
		{"unknown useful action", func(r *RecoveryRequest) { r.Proof.NoUsefulAction = false }},
		{"external outcome unknown", func(r *RecoveryRequest) { r.Proof.NoExternalOperation = false }},
		{"environment still broken", func(r *RecoveryRequest) { r.Proof.EnvironmentReady = false }},
		{"thread timeout", func(r *RecoveryRequest) { r.Proof.Phase = "thread/start" }},
		{"provider timeout", func(r *RecoveryRequest) { r.Admission.Authenticated = false }},
		{"unknown weekly", func(r *RecoveryRequest) { r.Admission.AllRequiredWindowsAvailable = false }},
		{"stale admission", func(r *RecoveryRequest) { r.Admission.ObservedAt = time.Now().Add(-11 * time.Minute) }},
		{"future evidence", func(r *RecoveryRequest) { r.Proof.ObservedAt = time.Now().Add(time.Hour) }},
		{"changed scope", func(r *RecoveryRequest) { r.IssueRevision++ }},
		{"changed model", func(r *RecoveryRequest) { r.Admission.Model = "other" }},
		{"changed version", func(r *RecoveryRequest) { r.ExpectedVersion++ }},
	}
	for _, tc := range checks {
		t.Run(tc.name, func(t *testing.T) {
			r := req
			tc.change(&r)
			if _, err := m.RecoverPreStart(ctx, r); err == nil {
				t.Fatal("unsafe retry admitted")
			}
		})
	}
	f.Exec(t, "UPDATE agent_runtime SET status='offline' WHERE id=$1", runtime)
	if _, err := m.RecoverPreStart(ctx, req); err == nil {
		t.Fatal("offline retry admitted")
	}
	f.Exec(t, "UPDATE agent_runtime SET status='online',last_seen_at=now() WHERE id=$1", runtime)
	successor := f.Task(t, agent, testutil.Cols{"issue_id": req.IssueID, "runtime_id": runtime, "status": "running"})
	if _, err := m.RecoverPreStart(ctx, req); err == nil {
		t.Fatal("active successor ignored")
	}
	f.Exec(t, "UPDATE agent_task_queue SET status='completed',completed_at=now() WHERE id=$1", successor)
	t.Setenv("MULTICA_CONTINUATION_RECOVERY", "false")
	if _, err := m.RecoverPreStart(ctx, req); err == nil {
		t.Fatal("OFF admitted retry")
	}
	t.Setenv("MULTICA_CONTINUATION_RECOVERY", "true")
	_, err := m.RecoverPreStart(ctx, req)
	must(t, err)
}

func TestRecoveryRefusesExistingPlatformRetry(t *testing.T) {
	f, m, req, agent, runtime := recoveryFixture(t)
	f.Task(t, agent, testutil.Cols{"issue_id": req.IssueID, "runtime_id": runtime, "status": "failed", "retry_of_task_id": req.OriginalTaskID, "attempt": 2, "completed_at": testutil.Raw("now()")})
	_, err := m.RecoverPreStart(context.Background(), req)
	if err == nil || errors.Is(err, context.Canceled) {
		t.Fatalf("expected no new budget: %v", err)
	}
}

func TestRecoveryExternalHoldAndPrivateDeletion(t *testing.T) {
	f, m, req, _, _ := recoveryFixture(t)
	ctx := context.Background()
	f.Exec(t, "UPDATE member SET role='guest' WHERE workspace_id=$1 AND user_id=$2", req.WorkspaceID, req.OwnerID)
	if _, err := m.RecoverPreStart(ctx, req); err == nil {
		t.Fatal("guest invocation allowed")
	}
	f.Exec(t, "UPDATE member SET role='owner' WHERE workspace_id=$1 AND user_id=$2", req.WorkspaceID, req.OwnerID)
	_, err := m.ReconcileOperation(ctx, OperationRequest{Key: req.Key, Operation: Operation{Kind: "sms", Provider: "fixture", DemoSession: "demo", Attempt: req.IssueID}}, SnapshotObserver{})
	must(t, err)
	if _, err := m.RecoverPreStart(ctx, req); err == nil {
		t.Fatal("external operation ignored by pre-start path")
	}
	f.Cleanup(t, "DELETE FROM issue_continuation_operation WHERE issue_id=$1", req.IssueID)
	// Insert only a fixture receipt to verify the production deletion query
	// removes both private tables; it does not authorize a retry.
	f.Exec(t, `INSERT INTO issue_continuation_recovery(workspace_id,issue_id,chain_id,remainder_id,original_task_id,generation,receipt,admission) VALUES($1,$2,$3,$4,$5,1,'{}','{}')`, req.WorkspaceID, req.IssueID, req.ChainID, req.RemainderID, req.OriginalTaskID)
	var issue, workspace pgtype.UUID
	must(t, issue.Scan(req.IssueID))
	must(t, workspace.Scan(req.WorkspaceID))
	must(t, db.New(f.Pool).DeleteIssue(ctx, db.DeleteIssueParams{ID: issue, WorkspaceID: workspace}))
	for _, table := range []string{"issue_continuation_operation", "issue_continuation_recovery"} {
		if f.Count(t, "SELECT count(*) FROM "+table+" WHERE issue_id=$1", req.IssueID) != 0 {
			t.Fatal("private recovery data survived deletion")
		}
	}
}
