# Controlled recovery pilot

This package builds on the continuation detector contract without enrolling
ordinary tasks or changing its scheduler. `cmd/continuation-recovery` is a
separate, operator-only tool using existing server database access. It can
enqueue **one** explicitly approved pre-start retry. It can only **read**
external systems; it has no merge, deploy, call, SMS or payment submission API.

`MULTICA_CONTINUATION_RECOVERY` is **OFF by default**. A locally passing test,
installed binary, receipt, or completed task is not evidence that an incident
was recovered or that this pilot is deployed. Automatic detector-to-recovery
dispatch is deliberately not wired: evidence acquisition is operator-managed.

## Preconditions and authority

The pilot is restricted to the detector's enrolled project, ordinary issue
tasks assigned to the original agent, and an original human originator who
owns that agent and runtime. Shared-agent, connected-app, squad, autopilot, channel, chat and
task-owned source-context recovery are held for their existing service paths.
This conservative boundary avoids granting new invocation access or dropping
channel/source-context delivery obligations. No model/account changes occur.

An operator reads the current contract, issue revision, agent configuration
timestamp, actual account/model/authentication, **all required quota windows**,
runtime and slots. Unknown is not available. Admission and process evidence
expire after ten minutes and must not be future-dated. Runtime heartbeat must
be newer than two minutes. The database rechecks scope, assignment, binding,
online status, versions and active tasks under enqueue/issue/ledger locks.

The operator must prove all of the following, independently of model output:

- The exact original task failed during environment preparation or initialize.
- No useful action or external operation was performed; any environment problem
  has been fixed. An execenv disk/untracked-file failure is not a quota reset.
- The **whole prior process tree** has stopped, using correlated lifecycle
  evidence. A terminal status, expired lease, parent PID exit, missing output,
  or an escalation is insufficient. Fencing a new local enqueue does not fence
  a remote operation or an unobserved old process.
- The runtime's explicit `retry_safe` decision is true. A thread/start timeout
  remains unsafe even if the process has since exited: provider state may exist.
- There is no pending/running successor or existing platform retry/manual rerun.

The booleans are attestations by a privileged operator, **not telemetry-derived
proof** or an untrusted caller API. Tasks do not receive DB credentials. Never
fill them with `true` merely to get past a hold. Keep the supporting evidence in
the existing private operations log; the ledger contains only bounded fields.

## Atomic retry and failure handling

The recovery key is `(workspace_id, original_task_id, generation)`. A unique
index, locked contract version and the existing queue insertion protect it.
The receipt, child task and explicit receiver adoption commit in one database
transaction. Failure before commit leaves no child; lost ACK after commit is
handled by repeating the same request and reading the existing receipt. It
does not require fresh admission to *read* that receipt. A receipt is never a
new dispatch authorization. Generations cannot reset budget for the same root.
After a new detector generation, callers must first read the current
contract and supply its generation. The original task still returns its first
receipt, with the original receipt generation and retry task ID; reading it
does not replace the new receiver or amend either history. Stale generations
conflict, and a new enqueue still requires the current contract version.

The child starts a fresh provider session, preserves the existing workdir and
attribution, and sets `max_attempts=1`. That disables the existing service's
reason-specific automatic retry ceilings too. If this child fails/cancels,
the same request returns `blocked_retry_exhausted` with the single contract
owner. The detector delivers `retry_exhausted` for the adopted failed/cancelled
retry. If this generation already delivered the original failure, reconciliation
advances its signal generation once and appends the prior contract to history;
the first incident and recovery receipt remain unchanged. Generation, history
and the new incident commit together; inbox delivery commits with its outbox ACK.
Restart or repeated terminal
delivery cannot create another exhaustion signal or refill the original-task
budget. A completed retry returns `awaiting_coverage`;
only the existing explicit result/coverage contract can resolve the remainder.
These are distinct receipts, not an exactly-once execution claim.

Recovery locks workspace (KEY SHARE), agent (UPDATE, in that workspace), then
issue (UPDATE), matching workspace deletion's fence. Task wakeup context is
also checked and held through enqueue: a deleted, explicitly disabled, revised
or malformed wakeup holds before spending the retry budget, even with a direct
receiver contract. A consumed once rule with the current revision and no
`disabled_at` remains eligible. The normal claim fence is unchanged; a later
wakeup invalidation still controls the child through the ordinary service path.

Build from `server/`:

```sh
go build -o /path/to/operator-tools/continuation-recovery ./cmd/continuation-recovery
```

Use existing secret provisioning for `DATABASE_URL`, never a file in the PR.
After review and authorized rollout, set the same
`MULTICA_CONTINUATION_PROJECT_ID` as the detector and explicitly set
`MULTICA_CONTINUATION_RECOVERY=true` for the operator process only.

```sh
continuation-recovery -operation recover-prestart -input recovery.json
```

`recovery.json` is `RecoveryRequest` in `recovery.go`: the four contract key
UUIDs, generation, expected contract version, issue revision, owner/original
task UUIDs, and the `Admission` and `PreStartProof` objects. Get the contract
with the existing `continuation -operation get` first. IDs, timestamps and
admission must be real observations, not copied from examples. On CAS conflict,
read current state; do not increment versions or generation to force acceptance.

## External reconciliation

`OperationRequest` contains the contract key, `expected_version` (zero for the
first record), a typed operation and optionally an already-authenticated
callback event ID. `OperationRecord` returns a version, observation, decision
and persisted callback IDs. A duplicate callback returns its saved receipt;
callbacks are hints to read the provider, never independent success evidence.
There is no public webhook in this patch. Existing callback authentication
must precede the operator/API adapter. At 1024 unique callback IDs, stop adding
events and reconcile directly without a callback ID.

```sh
continuation-recovery -operation reconcile -input operation.json
continuation-recovery -operation reconcile -input deploy.json -snapshot verified-deploy.json
```

| Operation key | Required external proof | Result on uncertainty |
| --- | --- | --- |
| repository + PR + expected head/base commit | GitHub PR read; on merged, commit read and first-parent base verification | Hold; changed head/base requires a new decision |
| environment + release/image SHA256 digests + config revision | Exact environment, immutable refs, bootstrap/migration/deploy receipts, health observation | Never repeat bootstrap or migrations; verify only health when receipts exist |
| call: demo session + explicit attempt | Retell [GET /v2/get-call/{call_id}](https://docs.retellai.com/api-references/get-call) with exact returned call ID | No ID, timeout or unsupported state: hold; **no auto-redial** |
| SMS: demo session + explicit attempt | Verified operator snapshot of the separate SMS provider operation ID/status | Hold; no automatic resend |

GitHub uses `GITHUB_TOKEN`, Retell uses `RETELL_API_KEY`, only from existing
server provisioning. HTTP adapters issue GET to fixed hosts, deny redirects,
bound responses, and exclude response bodies/errors/headers from storage.
Deploy snapshots require `environment` in the same identifier format as the
operation, matching it exactly. An absent, malformed or different environment
holds even if all digests, receipts and health checks match. This scope check
does not independently authenticate the operator's observation.

The deployment/SMS snapshot is a typed operator attestation from a real
read-side check; it is not a claim that a universal deployment/SMS adapter
exists. Snapshot observations must be fresh. A `ready` decision identifies a
remaining read/check; **it is not permission to execute a mutation**. Revalidate
state and ordinary authorization immediately before any subsequent operation.

A missing deploy receipt cannot prove that the deploy never happened. It asks
for receipt reconciliation, not redeploy. A failed call requires an explicit
new attempt decision; never silently generate another attempt ID. An `ended`
Retell call proves the attempt ended, not that a person answered or the demo
succeeded. The [current create-phone-call specification](https://docs.retellai.com/api-references/create-phone-call)
(read 2026-11-19, spec revision `2026-11-19-f8fd0b4`) documents an optional
`idempotency_key` (5–255 characters, letters/digits/underscore/hyphen), allowing
the same request to be retried for **up to one hour** without creating another
call. This is documented API support, not proof that a particular demo caller
persisted and supplied it. This tool does not submit/replay that request or
assume an ID lookup by key exists. A production caller must durably bind the
key and exact immutable request to the explicit attempt *before* submission;
a changed request, missing key, unknown original timestamp or expired window
requires a decision. Submit timeout without a saved call ID therefore remains
an unresolved hold here. Exactly-once is not promised. SMS capabilities remain
provider-specific and unverified by this patch.

## Correlated diagnostics

Existing daemon phase timing covers claim, environment preparation, runtime
start, first output/tool and completion. It now records an explicit UTC phase
time, daemon PID, CPU count and daemon goroutine count. Existing Codex lifecycle
logs cover child PID/attempt, initialize/thread setup, cleanup/exit and active
launches. The allowlisted initialize/thread-start/resume/turn-start RPCs add
numeric request IDs with task/runtime/PID/attempt correlation. No RPC params,
tokens, environment, payload or raw stderr are added. Existing diagnostics are
not used as an automatic recovery permission. CPU count/goroutines/latency do
not prove Mac overload or provider failure; a single timeout leaves cause unknown.

## Review, rollout and rollback

1. Review this delta independently of the detector. Substantial recovery risk
   requires the applicable independent L2 before enablement, with provider
   separation or an explicit owner exception. No review is implied by fixtures.
2. Use ordinary upstream release/deployment authorization and verify the actual
   binary/source and additive migrations 708–710. Resolve migration numbering
   against the final merged base before landing; this stacked patch preserves
   the exact parent contract instead of rebasing its reviewed branch.
3. Keep recovery OFF while checking receipts, duplicate suppression and external
   read adapters against authorized fixtures. Enabling the detector alone does
   not enable recovery. A production pilot and actual recovery receipt require
   separate evidence; do not create a real side effect just to test this guard.
4. Disable `MULTICA_CONTINUATION_RECOVERY` to stop further controlled enqueues.
   Preserve tables/receipts/keys. This does not cancel an already committed child
   or stop an external operation. Reconcile it under the normal ownership rules.
   Do not run destructive down migrations as a production rollback.

Issue/workspace deletion cleans up the additional private records with their
owning resource. Focused tests cover concurrent handlers, before/after ACK,
restart, provider-success/local-receipt crash, changed intent, callback dedup,
offline/reconnect, active successors, budget exhaustion and OFF behavior.
No fixture performs a real merge, deployment, call, SMS or model invocation.
