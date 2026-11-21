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

func TestStagedWorkspaceDeletionRemovesRecoveryOnlyInTarget(t *testing.T) {
	f, _, c, _, _ := fixture(t)
	neighbor, _, other, _, _ := fixture(t)
	for _, seed := range []struct {
		f *testutil.Fixture
		c Contract
	}{{f, c}, {neighbor, other}} {
		seed.f.InsertNoID(t, "issue_continuation_recovery", testutil.Cols{
			"workspace_id": seed.c.WorkspaceID, "issue_id": seed.c.IssueID,
			"chain_id": seed.c.ChainID, "remainder_id": seed.c.RemainderID,
			"original_task_id": seed.c.ReceiverTaskID, "generation": 1,
			"receipt": "{}", "admission": "{}",
		}, "issue_id=$1", seed.c.IssueID)
		seed.f.InsertNoID(t, "issue_continuation_operation", testutil.Cols{
			"workspace_id": seed.c.WorkspaceID, "issue_id": seed.c.IssueID,
			"operation_key": "fixture", "version": 1, "record": "{}",
		}, "issue_id=$1", seed.c.IssueID)
	}
	ctx := context.Background()
	tx, err := f.Pool.Begin(ctx)
	must(t, err)
	defer tx.Rollback(ctx)
	_, err = tx.Exec(ctx, "SELECT id FROM workspace WHERE id=$1 FOR UPDATE", c.WorkspaceID)
	must(t, err)
	var workspace pgtype.UUID
	must(t, workspace.Scan(c.WorkspaceID))
	must(t, db.New(tx).DeleteWorkspaceIssueRoots(ctx, workspace))
	must(t, tx.Commit(ctx))
	for _, table := range []string{"issue", "issue_continuation_recovery", "issue_continuation_operation"} {
		if got := f.Count(t, "SELECT count(*) FROM "+table+" WHERE workspace_id=$1", c.WorkspaceID); got != 0 {
			t.Errorf("staged deletion retained %d %s rows", got, table)
		}
		if got := neighbor.Count(t, "SELECT count(*) FROM "+table+" WHERE workspace_id=$1", other.WorkspaceID); got != 1 {
			t.Errorf("staged deletion changed neighbor %s rows: %d", table, got)
		}
	}
}

func TestRecoveryStaleGenerationAndCASCannotEnqueue(t *testing.T) {
	f, m, req, _, _ := recoveryFixture(t)
	ctx := context.Background()
	before := current(t, m, req.Key)
	before.Evidence = "Updated operator handoff"
	before = put(t, m, before.Contract, before.Version)
	for _, stale := range []RecoveryRequest{
		req,
		{Key: req.Key, Generation: req.Generation + 1, ExpectedVersion: before.Version, OwnerID: req.OwnerID, OriginalTaskID: req.OriginalTaskID},
	} {
		if _, err := m.RecoverPreStart(ctx, stale); !errors.Is(err, ErrConflict) {
			t.Fatalf("stale generation/version did not conflict: %v", err)
		}
	}
	if got := f.Count(t, "SELECT count(*) FROM agent_task_queue WHERE retry_of_task_id=$1", req.OriginalTaskID); got != 0 {
		t.Fatal("stale request consumed retry budget")
	}
	if got := f.Count(t, "SELECT count(*) FROM issue_continuation_recovery WHERE original_task_id=$1", req.OriginalTaskID); got != 0 {
		t.Fatal("stale request persisted a receipt")
	}
	if after := current(t, m, req.Key); after.Version != before.Version || after.ReceiverTaskID != before.ReceiverTaskID {
		t.Fatal("stale request changed the contract")
	}
	req.ExpectedVersion = before.Version
	_, err := m.RecoverPreStart(ctx, req)
	must(t, err)
}

func TestRecoveryReceiptAndBudgetSurviveNewGeneration(t *testing.T) {
	f, m, req, agent, runtime := recoveryFixture(t)
	ctx := context.Background()
	receipt, err := m.RecoverPreStart(ctx, req)
	must(t, err)
	f.Exec(t, "UPDATE agent_task_queue SET status='failed',completed_at=now() WHERE id=$1", receipt.RetryTaskID)
	m.Notify = true
	must(t, m.Deliver(ctx, req.Key))
	before := current(t, m, req.Key)
	incident := incidentSnapshot(t, f, before.Contract)
	var saved string
	f.QueryRow(t, "SELECT row_to_json(r)::text FROM issue_continuation_recovery r WHERE original_task_id=$1", req.OriginalTaskID).Scan(&saved)
	// Accepted1769 permits an explicit new live receiver after delivery. That
	// changes the detector generation, not the original task's retry budget.
	next := before.Contract
	next.Generation++
	next.ReceiverTaskID = f.Task(t, agent, testutil.Cols{"issue_id": req.IssueID, "runtime_id": runtime})
	after, err := m.Put(ctx, Change{Contract: next, ExpectedVersion: before.Version, NewGeneration: true})
	must(t, err)
	if _, err = m.RecoverPreStart(ctx, req); !errors.Is(err, ErrConflict) {
		t.Fatalf("old generation did not conflict: %v", err)
	}
	req.Generation, req.ExpectedVersion = after.Generation, after.Version
	req.Admission.ObservedAt = time.Time{} // Receipt reads do not dispatch.
	restarted := &Monitor{Pool: m.Pool, ProjectID: m.ProjectID}
	for range 2 {
		got, err := restarted.RecoverPreStart(ctx, req)
		must(t, err)
		if got.OriginalTaskID != req.OriginalTaskID || got.RetryTaskID != receipt.RetryTaskID || got.Generation != receipt.Generation || got.OwnerID != receipt.OwnerID || got.State != "blocked_retry_exhausted" || !got.CreatedAt.Equal(receipt.CreatedAt) {
			t.Fatalf("lost original receipt across generation: %+v", got)
		}
	}
	if got := f.Count(t, "SELECT count(*) FROM agent_task_queue WHERE retry_of_task_id=$1", req.OriginalTaskID); got != 1 {
		t.Fatal("generation reset retry budget")
	}
	if got := f.Count(t, "SELECT count(*) FROM issue_continuation_recovery WHERE original_task_id=$1", req.OriginalTaskID); got != 1 {
		t.Fatal("generation created another recovery receipt")
	}
	var stored string
	f.QueryRow(t, "SELECT row_to_json(r)::text FROM issue_continuation_recovery r WHERE original_task_id=$1", req.OriginalTaskID).Scan(&stored)
	if stored != saved || incidentSnapshot(t, f, before.Contract) != incident {
		t.Fatal("receipt read changed the recovery or delivered incident history")
	}
	if current := current(t, m, req.Key); current.Version != after.Version || current.Generation != after.Generation || current.ReceiverTaskID != next.ReceiverTaskID {
		t.Fatal("receipt read adopted the old retry into the new generation")
	}
}
