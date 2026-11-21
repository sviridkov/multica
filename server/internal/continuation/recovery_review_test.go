package continuation

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

func TestRecoveryExhaustionAfterDeliveredIncident(t *testing.T) {
	for _, terminal := range []string{"failed", "cancelled"} {
		t.Run(terminal, func(t *testing.T) {
			f, m, req, _, _ := recoveryFixture(t)
			ctx := context.Background()
			m.Notify = true
			must(t, m.Deliver(ctx, req.Key))
			before := current(t, m, req.Key)
			firstIncident := incidentSnapshot(t, f, before.Contract)
			req.ExpectedVersion = before.Version
			receipt, err := m.RecoverPreStart(ctx, req)
			must(t, err)
			var savedReceipt, adoptionHistory string
			f.QueryRow(t, "SELECT row_to_json(r)::text FROM issue_continuation_recovery r WHERE original_task_id=$1", req.OriginalTaskID).Scan(&savedReceipt)
			f.QueryRow(t, "SELECT history::text FROM issue_continuation WHERE issue_id=$1", req.IssueID).Scan(&adoptionHistory)
			f.Exec(t, "UPDATE agent_task_queue SET status=$2,completed_at=now() WHERE id=$1", receipt.RetryTaskID, terminal)
			for range 2 { // Repeated terminal/restart must deliver exhaustion once.
				must(t, (&Monitor{Pool: m.Pool, ProjectID: m.ProjectID, Notify: true}).Deliver(ctx, req.Key))
			}
			if got := f.Count(t, "SELECT count(*) FROM inbox_item WHERE issue_id=$1", req.IssueID); got != 2 {
				t.Errorf("got %d notifications, want original failure and exhaustion", got)
			}
			if got := f.Count(t, "SELECT count(*) FROM issue_continuation_incident WHERE issue_id=$1 AND reason='retry_exhausted' AND outbox_state='delivered'", req.IssueID); got != 1 {
				t.Errorf("got %d exhaustion receipts, want one", got)
			}
			if got := f.Count(t, "SELECT count(*) FROM inbox_item WHERE issue_id=$1 AND recipient_id=$2 AND details->>'reason'='retry_exhausted' AND details->>'receiver_task_id'=$3", req.IssueID, req.OwnerID, receipt.RetryTaskID); got != 1 {
				t.Error("exhaustion did not identify the existing owner and retry")
			}
			if incidentSnapshot(t, f, before.Contract) != firstIncident {
				t.Error("first delivered incident changed")
			}
			after := current(t, m, req.Key)
			req.Generation, req.ExpectedVersion = after.Generation, after.Version
			got, err := m.RecoverPreStart(ctx, req)
			must(t, err)
			if got.RetryTaskID != receipt.RetryTaskID || got.Generation != receipt.Generation || got.State != "blocked_retry_exhausted" {
				t.Fatalf("lost original exhaustion receipt: %+v", got)
			}
			var stored string
			f.QueryRow(t, "SELECT row_to_json(r)::text FROM issue_continuation_recovery r WHERE original_task_id=$1", req.OriginalTaskID).Scan(&stored)
			if stored != savedReceipt || f.Count(t, "SELECT count(*) FROM agent_task_queue WHERE retry_of_task_id=$1", req.OriginalTaskID) != 1 {
				t.Error("exhaustion changed receipt or replenished retry budget")
			}
			if f.Count(t, "SELECT count(*) FROM issue_continuation WHERE issue_id=$1 AND history @> $2::jsonb", req.IssueID, adoptionHistory) != 1 {
				t.Error("exhaustion discarded receiver history")
			}
		})
	}
}

func TestRecoveryWorkspaceDeleteLockOrder(t *testing.T) {
	for _, commit := range []bool{false, true} {
		name := "rollback"
		if commit {
			name = "commit"
		}
		t.Run(name, func(t *testing.T) {
			f, m, req, _, _ := recoveryFixture(t)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			tx, err := f.Pool.Begin(ctx)
			must(t, err)
			defer tx.Rollback(context.Background())
			q := db.New(tx)
			var workspace pgtype.UUID
			must(t, workspace.Scan(req.WorkspaceID))
			_, err = q.LockWorkspaceForDelete(ctx, workspace)
			must(t, err)
			_, err = tx.Exec(ctx, "SET LOCAL lock_timeout='500ms'")
			must(t, err)
			var deletePID int32
			must(t, tx.QueryRow(ctx, "SELECT pg_backend_pid()").Scan(&deletePID))
			done := make(chan error, 1)
			go func() { _, err := m.RecoverPreStart(ctx, req); done <- err }()
			defer func() {
				cancel()
				_ = tx.Rollback(context.Background())
				<-done
			}()
			for {
				var waiting bool
				must(t, f.Pool.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE $1=ANY(pg_blocking_pids(pid)))", deletePID).Scan(&waiting))
				if waiting {
					break
				}
				select {
				case <-ctx.Done():
					t.Fatal("recovery never reached the workspace fence")
				case <-time.After(5 * time.Millisecond):
				}
			}
			// Production teardown fences must not wait on an agent already
			// held by recovery while recovery waits on this workspace.
			must(t, q.LockWorkspaceTaskOwnerAgents(ctx, workspace))
			must(t, q.LockWorkspaceTaskOwnerIssues(ctx, workspace))
			if commit {
				must(t, q.DeleteWorkspaceIssueRoots(ctx, workspace))
				must(t, q.DeleteWorkspaceAgents(ctx, workspace))
				must(t, tx.Commit(ctx))
			} else {
				must(t, tx.Rollback(ctx))
			}
			err = <-done
			done <- err // The deferred drain also handles assertion failures.
			if commit && !errors.Is(err, pgx.ErrNoRows) {
				t.Fatalf("deleted owner admitted recovery or failed unexpectedly: %v", err)
			}
			if !commit {
				must(t, err)
			}
			want := 1
			if commit {
				want = 0
			}
			if got := f.Count(t, "SELECT count(*) FROM issue_continuation_recovery WHERE original_task_id=$1", req.OriginalTaskID); got != want {
				t.Fatalf("recovery receipt count=%d, want %d", got, want)
			}
		})
	}
}

func TestRecoveryWakeupClaimFence(t *testing.T) {
	for _, state := range []string{"disabled", "deleted", "revised", "consumed once"} {
		t.Run(state, func(t *testing.T) {
			f, m, req, agent, runtime := recoveryFixture(t)
			ctx := context.Background()
			wakeup := f.Insert(t, "issue_wakeup", testutil.Cols{
				"id": uuid.NewString(), "workspace_id": req.WorkspaceID, "issue_id": req.IssueID,
				"agent_id": agent, "created_by": req.OwnerID, "instruction": "Continue repair",
				"kind": "event", "mode": "once", "enabled": false, "last_task_id": req.OriginalTaskID,
			})
			f.Exec(t, "UPDATE agent_task_queue SET context=jsonb_build_object('wakeup_id',$2::text,'wakeup_revision',1) WHERE id=$1", req.OriginalTaskID, wakeup)
			switch state {
			case "disabled":
				f.Exec(t, "UPDATE issue_wakeup SET disabled_at=now() WHERE id=$1", wakeup)
			case "deleted":
				f.Exec(t, "DELETE FROM issue_wakeup WHERE id=$1", wakeup)
			case "revised":
				f.Exec(t, "UPDATE issue_wakeup SET revision=revision+1 WHERE id=$1", wakeup)
			}
			// A direct receiver contract cannot bypass its task's wakeup context.
			before := current(t, m, req.Key)
			receipt, recoverErr := m.RecoverPreStart(ctx, req)
			var agentID, runtimeID pgtype.UUID
			must(t, agentID.Scan(agent))
			must(t, runtimeID.Scan(runtime))
			claimed, claimErr := db.New(f.Pool).ClaimAgentTask(ctx, db.ClaimAgentTaskParams{AgentID: agentID, RuntimeID: runtimeID, PrepareLeaseSecs: 60, RuntimeStaleSecs: 120})
			if state == "consumed once" {
				must(t, recoverErr)
				must(t, claimErr)
				if claimed.ID.String() != receipt.RetryTaskID || claimed.Status != "dispatched" {
					t.Fatal("current consumed-once retry was not claimable")
				}
				return
			}
			if recoverErr == nil || !errors.Is(claimErr, pgx.ErrNoRows) {
				t.Errorf("stale wakeup must hold before enqueue: recovery=%v claim=%v", recoverErr, claimErr)
			}
			if f.Count(t, "SELECT count(*) FROM issue_continuation_recovery WHERE original_task_id=$1", req.OriginalTaskID) != 0 || f.Count(t, "SELECT count(*) FROM agent_task_queue WHERE retry_of_task_id=$1", req.OriginalTaskID) != 0 {
				t.Error("unclaimable wakeup consumed recovery budget")
			}
			if after := current(t, m, req.Key); before.Version != after.Version || before.ReceiverTaskID != after.ReceiverTaskID {
				t.Error("wake-up hold changed receiver history")
			}
		})
	}
}

func TestDeployObservationEnvironment(t *testing.T) {
	for _, environment := range []string{"", "staging", "invalid environment", "production"} {
		t.Run(environment, func(t *testing.T) {
			f, m, c, _, _ := fixture(t)
			put(t, m, c, 0)
			f.Cleanup(t, "DELETE FROM issue_continuation_operation WHERE issue_id=$1", c.IssueID)
			// Decode the operator's actual JSON shape: older binaries silently
			// discarded environment even when all the other facts matched.
			raw, err := json.Marshal(map[string]any{
				"known": true, "observed_at": time.Now().UTC(), "environment": environment,
				"release_digest": fixtureDigest, "image_digest": fixtureDigest, "config_revision": "rev7",
				"bootstrap_receipt": "bootstrap7", "migration_receipt": "migrate7", "deploy_receipt": "deploy7", "healthy": true,
			})
			must(t, err)
			var snapshot Observation
			must(t, json.Unmarshal(raw, &snapshot))
			req := OperationRequest{Key: c.Key, Operation: Operation{Kind: "deploy", Environment: "production", ReleaseDigest: fixtureDigest, ImageDigest: fixtureDigest, ConfigRevision: "rev7"}}
			record, err := m.ReconcileOperation(context.Background(), req, SnapshotObserver{Snapshot: snapshot})
			must(t, err)
			if environment == "production" {
				if record.Decision.State != "complete" || record.Completion == nil {
					t.Fatalf("matching environment did not complete: %+v", record.Decision)
				}
			} else if record.Decision.State != "hold" || record.Completion != nil {
				t.Fatalf("unverified target environment completed: %+v", record.Decision)
			}
		})
	}
}
