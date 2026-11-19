package continuation

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"regexp"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
)

// Operation pins immutable intent. Call and SMS are distinct operations even
// when they belong to the same demo session. No credential or payload belongs here.
type Operation struct {
	Kind                string `json:"kind"`
	Repository          string `json:"repository,omitempty"`
	PR                  int    `json:"pr,omitempty"`
	Head                string `json:"head,omitempty"`
	Base                string `json:"base,omitempty"`
	Environment         string `json:"environment,omitempty"`
	ReleaseDigest       string `json:"release_digest,omitempty"`
	ImageDigest         string `json:"image_digest,omitempty"`
	ConfigRevision      string `json:"config_revision,omitempty"`
	DemoSession         string `json:"demo_session,omitempty"`
	Attempt             string `json:"attempt,omitempty"`
	Provider            string `json:"provider,omitempty"`
	ProviderOperationID string `json:"provider_operation_id,omitempty"`
}

var shaPattern = regexp.MustCompile(`^[a-f0-9]{40}$`)
var digestPattern = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)
var refPattern = regexp.MustCompile(`^[A-Za-z0-9_.:-]{1,160}$`)
var repoPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)

func (o Operation) key() (string, error) {
	var identity any
	switch o.Kind {
	case "merge":
		if !repoPattern.MatchString(o.Repository) || o.PR < 1 || !shaPattern.MatchString(o.Head) || !shaPattern.MatchString(o.Base) {
			return "", errors.New("merge requires repository, PR and immutable expected head/base")
		}
		identity = []any{o.Kind, o.Repository, o.PR, o.Head, o.Base}
	case "deploy":
		if !refPattern.MatchString(o.Environment) || !digestPattern.MatchString(o.ReleaseDigest) || !digestPattern.MatchString(o.ImageDigest) || !refPattern.MatchString(o.ConfigRevision) {
			return "", errors.New("deploy requires environment, immutable release/image digests and config revision")
		}
		identity = []string{o.Kind, o.Environment, o.ReleaseDigest, o.ImageDigest, o.ConfigRevision}
	case "call", "sms":
		if !refPattern.MatchString(o.DemoSession) || !refPattern.MatchString(o.Attempt) || !refPattern.MatchString(o.Provider) || (o.ProviderOperationID != "" && !refPattern.MatchString(o.ProviderOperationID)) {
			return "", errors.New("call/SMS requires a demo session, explicit attempt and provider")
		}
		identity = []string{o.Kind, o.DemoSession, o.Attempt} // ID learned after submit cannot create a new attempt.
	default:
		return "", errors.New("unsupported external operation")
	}
	raw, _ := json.Marshal(identity)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

// Observation contains only a read-side result, not an assertion that timeout
// implies failure. Deploy completion includes an immutable receipt at each stage.
type Observation struct {
	ObservedAt          time.Time `json:"observed_at"`
	Known               bool      `json:"known"`
	Head                string    `json:"head,omitempty"`
	Base                string    `json:"base,omitempty"`
	Merged              bool      `json:"merged,omitempty"`
	MergeCommit         string    `json:"merge_commit,omitempty"`
	Open                bool      `json:"open,omitempty"`
	ReleaseDigest       string    `json:"release_digest,omitempty"`
	ImageDigest         string    `json:"image_digest,omitempty"`
	ConfigRevision      string    `json:"config_revision,omitempty"`
	BootstrapReceipt    string    `json:"bootstrap_receipt,omitempty"`
	MigrationReceipt    string    `json:"migration_receipt,omitempty"`
	DeployReceipt       string    `json:"deploy_receipt,omitempty"`
	Healthy             bool      `json:"healthy,omitempty"`
	ProviderOperationID string    `json:"provider_operation_id,omitempty"`
	ProviderStatus      string    `json:"provider_status,omitempty"`
}

type OperationDecision struct {
	State string `json:"state"`
	Next  string `json:"next"`
}

func decideOperation(o Operation, s Observation, now time.Time) OperationDecision {
	hold := OperationDecision{"hold", "reconcile_external_state"}
	if !s.Known || !fresh(s.ObservedAt, now) {
		return hold
	}
	switch o.Kind {
	case "merge":
		if o.Head != s.Head || o.Base != s.Base {
			return OperationDecision{"needs_decision", "review_changed_head_or_base"}
		}
		if s.Merged {
			if shaPattern.MatchString(s.MergeCommit) {
				return OperationDecision{"complete", "none"}
			}
			return hold
		}
		if s.Open {
			return OperationDecision{"ready", "revalidate_before_merge"}
		}
	case "deploy":
		if s.ReleaseDigest != o.ReleaseDigest || s.ImageDigest != o.ImageDigest || s.ConfigRevision != o.ConfigRevision {
			return OperationDecision{"needs_decision", "review_changed_deployment"}
		}
		// Missing durable stage receipts cannot prove bootstrap/migrations did
		// not run. Never recommend repeating either non-idempotent stage.
		if s.BootstrapReceipt == "" || s.MigrationReceipt == "" {
			return hold
		}
		if s.DeployReceipt == "" {
			return OperationDecision{"ready", "reconcile_missing_deploy_receipt"}
		}
		if !s.Healthy {
			return OperationDecision{"ready", "verify_health_only"}
		}
		return OperationDecision{"complete", "none"}
	case "call", "sms":
		if o.ProviderOperationID == "" || s.ProviderOperationID != o.ProviderOperationID {
			return hold
		}
		switch s.ProviderStatus {
		case "completed", "delivered":
			return OperationDecision{"complete", "none"}
		case "queued", "in_progress":
			return OperationDecision{"pending", "query_same_provider_operation"}
		case "failed":
			return OperationDecision{"needs_decision", "explicit_new_attempt_required"}
		}
	}
	return hold
}

// Observer has no submit/merge/deploy/send method. A timeout or missing API
// capability is represented by an unknown observation, never a new operation.
type Observer interface {
	Observe(context.Context, Operation) (Observation, error)
}

type OperationRequest struct {
	Key
	ExpectedVersion int64     `json:"expected_version"`
	Operation       Operation `json:"operation"`
	CallbackID      string    `json:"callback_id,omitempty"`
}

type OperationRecord struct {
	Completion  *Observation      `json:"completion,omitempty"`
	Operation   Operation         `json:"operation"`
	Observation Observation       `json:"observation"`
	Decision    OperationDecision `json:"decision"`
	Version     int64             `json:"version"`
	CallbackIDs []string          `json:"callback_ids,omitempty"`
}

// ReconcileOperation only reads an external system. Callback IDs are deduped
// durably, but callbacks never constitute completion without an authoritative
// read. The database lock is not held across a network call.
func (m *Monitor) ReconcileOperation(ctx context.Context, req OperationRequest, reader Observer) (OperationRecord, error) {
	key, err := req.Operation.key()
	if err != nil {
		return OperationRecord{}, err
	}
	if req.ExpectedVersion < 0 || (req.CallbackID != "" && !refPattern.MatchString(req.CallbackID)) {
		return OperationRecord{}, errors.New("invalid version or callback ID")
	}
	// Verify private scope before using credentials to read external state.
	c, err := m.Get(ctx, req.Key)
	if err != nil {
		return OperationRecord{}, err
	}
	if reader == nil {
		return OperationRecord{}, errors.New("read-only observer is required")
	}
	snapshot, readErr := reader.Observe(ctx, req.Operation)
	if readErr != nil || !snapshot.safe() {
		snapshot = Observation{}
	} // Do not persist a raw provider error.
	tx, err := m.Pool.Begin(ctx)
	if err != nil {
		return OperationRecord{}, err
	}
	defer tx.Rollback(ctx)
	if err = m.lockIssue(ctx, tx, req.Key); err != nil {
		return OperationRecord{}, err
	}
	current, err := load(ctx, tx, req.Key)
	if err != nil {
		return OperationRecord{}, err
	}
	if current.Version != c.Version {
		return OperationRecord{}, ErrConflict
	}
	if err = m.checkScope(ctx, tx, current.Contract); err != nil {
		return OperationRecord{}, err
	}
	// The unique operation key is workspace-wide; it cannot be adopted by a
	// different issue merely by guessing the same external intent.
	if _, err = tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtextextended($1,1773))", req.WorkspaceID+key); err != nil {
		return OperationRecord{}, err
	}
	var old OperationRecord
	var raw []byte
	var issue string
	err = tx.QueryRow(ctx, "SELECT issue_id::text,record FROM issue_continuation_operation WHERE workspace_id=$1 AND operation_key=$2 FOR UPDATE", req.WorkspaceID, key).Scan(&issue, &raw)
	if err == nil {
		if issue != req.IssueID {
			return old, errors.New("operation belongs to a different issue")
		}
		if err = json.Unmarshal(raw, &old); err != nil {
			return old, err
		}
		if old.Operation.Provider != req.Operation.Provider || (old.Operation.ProviderOperationID != "" && old.Operation.ProviderOperationID != req.Operation.ProviderOperationID) {
			return old, errors.New("provider operation identity changed")
		}
		if req.CallbackID != "" && slices.Contains(old.CallbackIDs, req.CallbackID) {
			return old, nil
		}
		if old.Version != req.ExpectedVersion {
			return old, ErrConflict
		}
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return old, err
	} else if req.ExpectedVersion != 0 {
		return old, ErrConflict
	}
	if req.CallbackID != "" && len(old.CallbackIDs) >= 1024 {
		return old, errors.New("callback budget exhausted; reconcile without another callback")
	}
	var now time.Time
	if err = tx.QueryRow(ctx, "SELECT clock_timestamp()").Scan(&now); err != nil {
		return old, err
	}
	record := OperationRecord{Operation: req.Operation, Observation: snapshot, Decision: decideOperation(req.Operation, snapshot, now), Version: old.Version + 1, CallbackIDs: old.CallbackIDs}
	if req.CallbackID != "" {
		record.CallbackIDs = append(record.CallbackIDs, req.CallbackID)
	}
	record.Completion = old.Completion
	if record.Decision.State == "complete" {
		completion := snapshot
		record.Completion = &completion
	}
	raw, err = json.Marshal(record)
	if err != nil {
		return old, err
	}
	_, err = tx.Exec(ctx, `INSERT INTO issue_continuation_operation(workspace_id,issue_id,operation_key,version,record) VALUES($1,$2,$3,$4,$5) ON CONFLICT(workspace_id,operation_key) DO UPDATE SET version=EXCLUDED.version,record=EXCLUDED.record`, req.WorkspaceID, req.IssueID, key, record.Version, raw)
	if err != nil {
		return old, err
	}
	return record, tx.Commit(ctx)
}

// Reject arbitrary payload/error text instead of trying to redact secrets after
// storing it. Unknown/unexpected provider data fails closed.
func (s Observation) safe() bool {
	for _, v := range []string{s.Head, s.Base, s.MergeCommit} {
		if v != "" && !shaPattern.MatchString(v) {
			return false
		}
	}
	for _, v := range []string{s.ReleaseDigest, s.ImageDigest} {
		if v != "" && !digestPattern.MatchString(v) {
			return false
		}
	}
	for _, v := range []string{s.ConfigRevision, s.BootstrapReceipt, s.MigrationReceipt, s.DeployReceipt, s.ProviderOperationID} {
		if v != "" && !refPattern.MatchString(v) {
			return false
		}
	}
	return slices.Contains([]string{"", "queued", "in_progress", "completed", "delivered", "failed"}, s.ProviderStatus)
}
