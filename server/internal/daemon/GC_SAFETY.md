# Task-root cleanup contract

Whole-root cleanup is an empty-scaffolding collector. Age and parent status
establish eligibility; they do not establish that results can be reconstructed.
Git, results, unknown entries and even a released `.task_lock` retain the root.
Artifact cleanup has its own eligibility and regenerability contract.

## Writer coordination

The supported deployment has one daemon owning a workspaces root. Task starts,
reuse, preparation helpers, agents and completion metadata writes must go through
that daemon's task lifecycle. Independent daemons must use separate roots.

| Writer | Required exclusion | Production path |
| --- | --- | --- |
| New preparation and `.task_owner` publication | Mark the resolved root active before claiming or preparing it; hold the mark through the task | `runTask` → `markActiveEnvRoot` → `ClaimEnvRoot` → preparation with `EnvRootPreclaimed` |
| Reuse of a prior root | Mark both candidate roots active before refresh; hold the prior root's OS claim while used | `runTask` → `markActiveEnvRoot` → `LockEnvRootForReuse` |
| Isolated preparation helper | Parent retains the active mark and OS claim until the helper and task exit | `prepareExecutionEnvironment`, `EnvRootPreclaimed` |
| Completion `.gc_meta.json` | Keep the outer active mark after `runTask` returns, through reporting and metadata publication | Task handler → `reportTaskResult` → `WriteGCMeta` → deferred `unmarkActiveEnvRoot` |
| Whole-root or retained-root artifact GC | Reserve the root before inspection/mutation; release after all mutation | `applyGCAction` → `reserveEnvRootForGC` |

The reservation and active marks share a mutex: a new mark waits for GC, and GC
refuses an already active root. `TestWholeTaskGCReservationBlocksResume`,
`TestActiveEnvRootRefcount` and the active-root GC regressions exercise this
exclusion. The root's OS lock additionally protects artifact fallback when a
previous execution left `.task_lock`; a held lock prevents fallback, a released
lock permits eligible artifact cleanup without authorizing whole-root removal.

`.task_owner`, `.gc_meta.json` and `.task_lock` are daemon control files, not
locations for user results. An agent may write results under its active task,
but it must not detach a writer beyond that task's lifetime, rename task/workspace
roots, or replace these control files. Manual repair requires stopping the
owning daemon and its task processes first. A tool that needs unrestricted
concurrent writes to these paths must use a different root or have the owning
daemon's GC disabled (`MULTICA_GC_ENABLED=false`) before it starts. Disabling
only the completed-task TTL does not disable other cleanup routes.

These are operational preconditions, not a same-user security boundary. The
filesystem permissions do not prevent another process running as the same user
from violating them. Check the owning daemon's configured root and launched
task lifecycle before enabling cleanup in a new integration; a successful
single-process test is not evidence that an arbitrary external writer obeys
this contract.

Server terminal status is not the end of the handler's writes. The completion
path deliberately reports the result before `WriteGCMeta`; shutdown reporting
can also use a context that outlives cancellation. Quiescence means the handler
has returned, its preparation/agent children have been reaped, and its active
marks and claims have been released. The completion regression
`TestHandleTask_KeepsEnvRootActiveAcrossCompletion` checks that metadata is absent
at the report, the root is still active there, and metadata exists when the
handler returns and the active mark is gone. The shutdown regressions exercise
a test-owned provider and the terminal dispositions, not every real provider.

The other control-file writers are `claimEnvRoot` / `LockEnvRootForReuse`
(`.task_lock` creation), `writeEnvRootOwner` (temporary owner file and rename),
and `resetEnvRootContents` (removes old metadata while holding a fresh claim).
GC's `gcTaskRoot.remove` removes control files and can restore their inspected
bytes with exclusive creation. Preparation also writes `.managed_env.json`,
which is retained content for whole-root GC. Task temporary directories have a
separate lock publication and cleanup lifecycle in `execenv/task_temp.go`; they
do not establish ownership of a workspaces root.

`TestGCReservationDoesNotExcludeExternalWriter` runs a child process that writes
metadata into a synthetic root while this daemon holds its GC reservation. The
child exits and is reaped; revalidation then detects the changed metadata and
preserves it. This demonstrates the reservation's process boundary and detection
before validation. It does not exercise or eliminate the later unlink race.

For an integration probe, use a newly created canonical root, fixture HOME and
profile, outside live roots and archives. Run only synthetic tests and config
commands there. An empty root and a process snapshot cannot establish future
writer exclusion. Until that exclusion is enforced (for example, a dedicated
OS principal/container with no external writable mount, plus supervised child
lifetimes) and independently accepted, leave GC disabled for a future rollout.
Root separation by itself under the same user does not enforce the contract.

## What the filesystem checks guarantee

Inspection pins the workspace and task directories, rejects linked roots and
invalid/foreign control metadata, and checks identity again before mutations.
Directory removal uses directory-only, nonrecursive operations; an arriving
payload prevents removal of its directory. Failed root removal restores removed
control bytes with `O_EXCL`, preserving an existing replacement.

Control-file validation and unlink are **not atomic**. An uncoordinated writer
that replaces `.task_owner` or `.gc_meta.json` after the last validation can have
its replacement removed. `O_EXCL` only protects restoration; it cannot recover
those replacement bytes. Neither pinned handles nor the reservation prove safety
against that writer. The contract above excludes it; accepting that operational
boundary requires review, and does not establish a general concurrent-writer
guarantee.

## Retained-root artifacts

A valid root retained for content can still reclaim eligible artifacts under the
same reservation. Completion age must exceed `GCArtifactTTL`; zero disables this
path. For legacy roots without a completion time, the metadata age (or root age
when metadata is absent) must exceed both the artifact and orphan TTLs.
Clean/terminal routes retain the existing basename plus exact-managed contract;
orphan and legacy routes use only exact managed paths. Unknown or malformed
metadata, ownership/path inspection failure, identity changes and held locks
prevent fallback. A failure while actually removing an empty root also prevents
fallback. This does not add any new cache paths or weaken Git preservation in
the artifact collector.
