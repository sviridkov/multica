package continuation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/dbid"
)

// Admission is an operator attestation from the actual account/model collector.
// Quota is not inferred from an old task error, a reset time, or a timeout.
type Admission struct {
	ObservedAt                  time.Time `json:"observed_at"`
	AgentUpdatedAt              time.Time `json:"agent_updated_at"`
	Account                     string    `json:"account"`
	Model                       string    `json:"model"`
	RuntimeID                   string    `json:"runtime_id"`
	BindingVerified             bool      `json:"binding_verified"`
	Authenticated               bool      `json:"authenticated"`
	ModelAvailable              bool      `json:"model_available"`
	AllRequiredWindowsAvailable bool      `json:"all_required_windows_available"`
	SlotAvailable               bool      `json:"slot_available"`
}

// PreStartProof must come from lifecycle/operation evidence, never absence of
// model output. Cleanup means the entire old process tree, not an expired lease.
type PreStartProof struct {
	ObservedAt          time.Time `json:"observed_at"`
	Phase               string    `json:"phase"`
	RetrySafe           bool      `json:"retry_safe"`
	ProcessTreeStopped  bool      `json:"process_tree_stopped"`
	NoUsefulAction      bool      `json:"no_useful_action"`
	NoExternalOperation bool      `json:"no_external_operation"`
	EnvironmentReady    bool      `json:"environment_ready"`
}

type RecoveryRequest struct {
	Key
	Generation      int64         `json:"generation"`
	ExpectedVersion int64         `json:"expected_version"`
	IssueRevision   int64         `json:"issue_revision"`
	OwnerID         string        `json:"owner_id"`
	OriginalTaskID  string        `json:"original_task_id"`
	Admission       Admission     `json:"admission"`
	Proof           PreStartProof `json:"proof"`
}

type RecoveryReceipt struct {
	OriginalTaskID string    `json:"original_task_id"`
	Generation     int64     `json:"generation"`
	RetryTaskID    string    `json:"retry_task_id"`
	OwnerID        string    `json:"owner_id"`
	State          string    `json:"state"`
	CreatedAt      time.Time `json:"created_at"`
}

func fresh(at, now time.Time) bool {
	return !at.IsZero() && !at.After(now) && now.Sub(at) <= 10*time.Minute
}

func (r RecoveryRequest) validate(now time.Time) error {
	for _, id := range []string{r.WorkspaceID, r.IssueID, r.ChainID, r.RemainderID, r.OwnerID, r.OriginalTaskID, r.Admission.RuntimeID} {
		if _, err := uuid.Parse(id); err != nil {
			return errors.New("invalid recovery identity")
		}
	}
	a, p := r.Admission, r.Proof
	if r.Generation < 1 || r.ExpectedVersion < 1 || r.IssueRevision < 1 {
		return errors.New("current generation, contract version and issue revision required")
	}
	if !fresh(a.ObservedAt, now) || !fresh(p.ObservedAt, now) {
		return errors.New("fresh admission and cleanup evidence required")
	}
	if a.Account == "" || a.Model == "" || !a.BindingVerified || !a.Authenticated || !a.ModelAvailable || !a.AllRequiredWindowsAvailable || !a.SlotAvailable {
		return errors.New("account/model/auth/quota/slot admission unproven")
	}
	if !p.RetrySafe || !p.ProcessTreeStopped || !p.NoUsefulAction || !p.NoExternalOperation || !p.EnvironmentReady {
		return errors.New("hold: pre-start safety not proven")
	}
	// A thread/start timeout can have created provider state; it is not an
	// initialize failure and cannot acquire a fresh retry through this path.
	if p.Phase != "environment_prepare" && p.Phase != "initialize" {
		return errors.New("hold: phase requires reconciliation")
	}
	return nil
}

// RecoverPreStart is deliberately operator-only, not called by the detector or
// terminal handlers. It records the one retry and its receipt in one transaction.
// Existing platform retries are never adopted as new budget or repeated here.
func (m *Monitor) RecoverPreStart(ctx context.Context, req RecoveryRequest) (RecoveryReceipt, error) {
	if os.Getenv("MULTICA_CONTINUATION_RECOVERY") != "true" {
		return RecoveryReceipt{}, errors.New("continuation recovery is OFF")
	}
	// Validate identities before SQL. Evidence freshness is checked against DB
	// time below, after locks, including a delayed/reconnected operator request.
	for _, id := range []string{req.OriginalTaskID, req.WorkspaceID, req.IssueID, req.ChainID, req.RemainderID} {
		if _, err := uuid.Parse(id); err != nil {
			return RecoveryReceipt{}, errors.New("invalid recovery key")
		}
	}
	tx, err := m.Pool.Begin(ctx)
	if err != nil {
		return RecoveryReceipt{}, err
	}
	defer tx.Rollback(ctx)
	q := db.New(tx)
	var original pgtype.UUID
	_ = original.Scan(req.OriginalTaskID)
	parent, err := q.GetAgentTask(ctx, original)
	if err != nil {
		return RecoveryReceipt{}, err
	}
	// Agent first, then workspace/issue/ledger, matching enqueue lock order.
	// FOR UPDATE serializes even ordinary enqueues (which take KEY SHARE).
	var locked string
	if err = tx.QueryRow(ctx, "SELECT id::text FROM agent WHERE id=$1 AND workspace_id=$2 FOR UPDATE", parent.AgentID, req.WorkspaceID).Scan(&locked); err != nil {
		return RecoveryReceipt{}, err
	}
	if err = m.lockIssue(ctx, tx, req.Key); err != nil {
		return RecoveryReceipt{}, err
	}
	c, err := load(ctx, tx, req.Key)
	if err != nil {
		return RecoveryReceipt{}, err
	}
	if c.OwnerID != req.OwnerID || c.Generation != req.Generation {
		return RecoveryReceipt{}, ErrConflict
	}
	if err = m.checkScope(ctx, tx, c.Contract); err != nil {
		return RecoveryReceipt{}, err
	}
	var receipt RecoveryReceipt
	var raw []byte
	err = tx.QueryRow(ctx, `SELECT receipt FROM issue_continuation_recovery WHERE workspace_id=$1 AND original_task_id=$2 AND generation=$3 AND issue_id=$4 AND chain_id=$5 AND remainder_id=$6`, req.WorkspaceID, req.OriginalTaskID, req.Generation, req.IssueID, req.ChainID, req.RemainderID).Scan(&raw)
	if err == nil {
		if err = json.Unmarshal(raw, &receipt); err != nil {
			return receipt, err
		}
		var status string
		if err = tx.QueryRow(ctx, "SELECT status FROM agent_task_queue WHERE id=$1", receipt.RetryTaskID).Scan(&status); err != nil {
			return receipt, err
		}
		switch status {
		case "failed", "cancelled":
			receipt.State = "blocked_retry_exhausted"
		case "completed":
			receipt.State = "awaiting_coverage"
		}
		return receipt, nil // Lost ACK/restart does not re-enqueue or spend budget.
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return receipt, err
	}
	if c.Version != req.ExpectedVersion || c.ReceiverTaskID != req.OriginalTaskID || c.CoverageConfirmed || c.ExternalOutcome != "none" {
		return receipt, ErrConflict
	}
	var now time.Time
	if err = tx.QueryRow(ctx, "SELECT clock_timestamp()").Scan(&now); err != nil {
		return receipt, err
	}
	if err = req.validate(now); err != nil {
		return receipt, err
	}
	f, err := readFacts(ctx, tx, &c.Contract)
	if err != nil {
		return receipt, err
	}
	if f.handoffChanged || f.receiverMissing || f.status != "failed" || c.ReceiverTaskID != req.OriginalTaskID || (c.WaitUntil != nil && now.Before(*c.WaitUntil)) {
		return receipt, errors.New("hold: current receiver/wait differs")
	}
	// Refresh after the locks. Only original, ordinary issue tasks are eligible.
	parent, err = q.GetAgentTask(ctx, original)
	if err != nil {
		return receipt, err
	}
	if parent.OriginatorUserID.String() != req.OwnerID || parent.Status != "failed" || !parent.CompletedAt.Valid || parent.CompletedAt.Time.After(req.Proof.ObservedAt) || parent.RetryOfTaskID.Valid || parent.RerunOfTaskID.Valid || parent.Attempt != 1 || parent.AutopilotRunID.Valid || parent.ChatSessionID.Valid || parent.SquadID.Valid || parent.IsLeaderTask || parent.IssueID.String() != req.IssueID {
		return receipt, errors.New("hold: not an original failed ordinary issue task")
	}
	var allowed bool
	err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM issue i JOIN agent a ON a.id=$3 JOIN agent_runtime rt ON rt.id=a.runtime_id
	 WHERE i.id=$1 AND i.workspace_id=$2 AND i.revision=$4 AND i.assignee_type='agent' AND i.assignee_id=a.id AND i.status IN ('todo','in_progress')
	 AND a.owner_id=$10 AND rt.owner_id=$10
 AND EXISTS(SELECT 1 FROM member u WHERE u.workspace_id=i.workspace_id AND u.user_id=$10 AND u.role<>'guest')
 AND COALESCE(cardinality(a.composio_toolkit_allowlist),0)=0 AND a.archived_at IS NULL AND a.updated_at=$5 AND a.model=$6 AND rt.id=$7 AND rt.status='online' AND rt.last_seen_at>$8
	 AND NOT EXISTS(SELECT 1 FROM agent_task_queue t WHERE t.issue_id=i.id AND t.status IN ('queued','dispatched','running','deferred','waiting_local_directory'))
	 AND NOT EXISTS(SELECT 1 FROM agent_task_queue t WHERE t.retry_of_task_id=$9 OR t.rerun_of_task_id=$9)
	 AND NOT EXISTS(SELECT 1 FROM issue_continuation_recovery r WHERE r.original_task_id=$9)
 AND NOT EXISTS(SELECT 1 FROM issue_continuation_operation o WHERE o.issue_id=i.id AND o.workspace_id=i.workspace_id)
	 AND (SELECT count(*) FROM agent_task_queue t WHERE t.agent_id=a.id AND t.status IN ('queued','dispatched','running','deferred','waiting_local_directory'))<a.max_concurrent_tasks
	 AND (rt.max_concurrent_tasks IS NULL OR (SELECT count(*) FROM agent_task_queue t WHERE t.runtime_id=rt.id AND t.status IN ('dispatched','running','waiting_local_directory'))<rt.max_concurrent_tasks))`, req.IssueID, req.WorkspaceID, parent.AgentID, req.IssueRevision, req.Admission.AgentUpdatedAt, req.Admission.Model, req.Admission.RuntimeID, now.Add(-2*time.Minute), req.OriginalTaskID, req.OwnerID).Scan(&allowed)
	if err != nil {
		return receipt, err
	}
	if !allowed {
		return receipt, errors.New("hold: ownership, scope, binding, runtime, successor or slot changed")
	}
	// No task-owned source-context transfer or connected-app credentials can be
	// reconstructed here. Such cases stay with the existing service path.
	var unsupported bool
	if err = tx.QueryRow(ctx, `SELECT COALESCE(context ? 'source_context_id',false) OR EXISTS(SELECT 1 FROM channel_task_delivery WHERE task_id=$1) FROM agent_task_queue WHERE id=$1`, original).Scan(&unsupported); err != nil {
		return receipt, err
	}
	if unsupported {
		return receipt, errors.New("hold: channel/source-context task requires its service recovery path")
	}
	child, err := q.CreateRetryTask(ctx, db.CreateRetryTaskParams{ID: original, NewTaskID: dbid.NewV7(), MaxAttempts: pgtype.Int4{Int32: 1, Valid: true}})
	if err != nil {
		return receipt, fmt.Errorf("recovery enqueue: %w", err)
	}
	// max_attempts=1 disables all platform retry ceilings, including provider
	// network escalation. No old provider session is replayed.
	if _, err = tx.Exec(ctx, "UPDATE agent_task_queue SET session_id=NULL,force_fresh_session=true WHERE id=$1", child.ID); err != nil {
		return receipt, err
	}
	receipt = RecoveryReceipt{req.OriginalTaskID, req.Generation, child.ID.String(), req.OwnerID, "retry_queued", now}
	raw, err = json.Marshal(receipt)
	if err != nil {
		return receipt, err
	}
	proof, err := json.Marshal(req)
	if err != nil {
		return receipt, err
	}
	_, err = tx.Exec(ctx, `INSERT INTO issue_continuation_recovery(workspace_id,issue_id,chain_id,remainder_id,original_task_id,generation,receipt,admission) VALUES($1,$2,$3,$4,$5,$6,$7,$8)`, req.WorkspaceID, req.IssueID, req.ChainID, req.RemainderID, req.OriginalTaskID, req.Generation, raw, proof)
	if err != nil {
		return receipt, err
	}
	c.ReceiverTaskID = receipt.RetryTaskID
	c.WakeupID = ""
	c.WakeupRevision = 0
	c.WaitKind = "none"
	c.WaitUntil = nil
	raw, err = json.Marshal(c.Contract)
	if err != nil {
		return receipt, err
	}
	_, err = tx.Exec(ctx, `UPDATE issue_continuation SET history=history||jsonb_build_array(jsonb_build_object('contract',contract,'version',version,'state',state,'at',now())),contract=$5,version=version+1,state='receiver_queued',last_progress_at=now(),next_check_at=now() WHERE `+keyWhere, append(keyArgs(req.Key), raw)...)
	if err != nil {
		return receipt, err
	}
	return receipt, tx.Commit(ctx)
}
