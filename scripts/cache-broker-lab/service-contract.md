# Cache service contract and disposable implementation

This continues LYNAIX-1249 option A. No production GC path calls this service.
Populated production caches remain retained, and the original five acceptance
criteria remain open. The experiment is a separate test service, not a Desktop
installation or authority to adopt existing home instances.

## Trust and registration

The administrator installs the measured service, pinned producer, immutable Git
fixture/source OID and one acquisition recipe. Registration accepts only that
opaque recipe ID. The service allocates an unpredictable new home ID and fresh
generation directories. Neither tasks nor the authenticated driver can supply a
filesystem path, producer command, idle age, active flag or deletion receipt.
The two artifact names are exactly `codex-home/.tmp/marketplaces` and
`codex-home/cache/remote_plugin_catalog` within a new generation.

The pinned producer provides source/revision, acquisition identity and successful
write/readback evidence. The service inventories content, file identity, owner,
permissions and native metadata, then commits the receipt. Existing unknown data
is never inventoried as a way to adopt it. A changed or unsupported artifact is
excluded from deletion. A source, producer or recipe upgrade requires a new
registration policy, never reinterpretation of old evidence.

The lab uses root on macOS and LocalSystem on Windows, with administrators outside
the threat model. General ancestors exclude ordinary mutations; the runtime
layout below permits creation only in three exact protected parents. The control
directory additionally excludes ordinary reads; certificates and keys are not
placed in task environment variables, logs or uploaded artifacts. Tasks can read
published caches but cannot write, rename or replace their entries or parents.

IPC binds only `127.0.0.1`; TLS 1.3 requires normal certificate/usage validation
and an exact peer certificate pin. A CA-issued but different client is not an
authority. A forged endpoint fails authentication before application requests.
Both sides have separate private keys. The service rejects plaintext, missing
client credentials, unknown JSON fields, extra JSON documents and oversized
requests, and sets connection/request deadlines. Readiness is published only in
protected configuration. Fault injection is a protected bootstrap file understood
only by the experimental executable; it is not an RPC.

This authenticates a protected credential's holder. It does not authenticate the
ordinary desktop user's executable. A real daemon sharing an unrestricted UID
with tasks cannot safely own that credential merely by changing file modes.

## Lifecycle and operation ordering

1. `register(recipeID, requestID)` commits a fresh logical home and request result.
2. `begin(homeID, revision, runID, launch|restore)` durably reserves a use lease
   before namespace creation, producer execution, task launch or discovery.
3. `prepare(leaseID, runID, artifact)` requires bootstrap-provisioned staged
   acquisition and persists PREPARING. A fresh ordinary producer executes the
   measured recipe on its bounded volume. Successful leader exit and sealed
   whole-domain quiescence precede bounded copy, no-replace publication, protected
   receipt commit and finally READY plus the request outcome. Restore precedes
   discovery; the native driver starts an ordinary reader only after this returns.
4. The trusted daemon supervises its run's complete process tree. Only after
   observing quiescence may it call `end(leaseID, runID)`. The service records its
   own terminal timestamp; the client cannot choose an idle age. Disconnect,
   timeout and missing heartbeat never imply termination.
5. `reclaim(generation, revision, artifact)` requires trusted terminal/idle state,
   enabled TTL and unchanged evidence. The service persists CONSUMING, then the
   receipt is consumed before unlink. Success and logical bytes are committed in
   the request result only after complete removal. Caller-side accounting must
   deduplicate request IDs when retrying or querying a committed result.

One service mutex and a stable, non-inherited kernel file lock serialize journal
and filesystem authority. The stable lock file is never unlinked. Every mutation
requires the current service epoch, home revision and a bounded request ID.
Reusing an ID with a different command is rejected. `result(requestID)` recovers a
committed response after disconnect; it grants no new deletion authority. A new
service epoch rejects old mutation requests even when their other fields match.

Each use gets a new path. Beginning a replacement excludes unreclaimed previous
READY artifacts, preserves their bytes and never transfers an unresolved run's
lease to the new generation. Only the current generation accepts mutations.
Historical namespaces, expired credentials and receipt generations are not
automatically migrated or rebound.

## Recovery and failure contract

| Durable artifact state at restart | Treatment |
| --- | --- |
| PREPARING, including producer alive or receipt committed but no service commit | Quarantine; preserve output and deny adoption/reclamation |
| READY | Keep evidence, but revoke current lifecycle until trusted reconciliation |
| CONSUMING, whether all, some or none of its bytes remain | Quarantine; preserve remnants; no invented byte credit |
| DELETED with committed request result | Keep the result for idempotent query; never remove a replacement instance |
| Unknown/corrupt/unsupported registry or missing registry in nonempty root | Refuse mutation; preserve every file |

Recovery durably records exclusions before enabling mutations. Quarantine is an
exclusion state, not a filesystem move. Orphan temporary journals are never
promoted based on parseability. Any uncertain registry write/sync/replace latches
the running service against further mutations; restart reloads durable state.
A failed artifact journal quarantines that generation through the service
journal. If that exclusion cannot be persisted, the whole service stays latched.

After restart, `reconcile` requires the authenticated daemon and exact run/lease.
It means the daemon has proved the old process tree stopped; it does not accept
task-provided flags. Reconciliation starts a new idle period. The test driver
waits for launchd's service process group to disappear; the Windows service owns
a non-inherited kill-on-close Job with no breakaway for producer descendants.
The measured recipe confines output to its old generation. These assumptions do
not establish arbitrary production process-tree supervision.

Native faults terminate the service at reservation/namespace creation, preparation
intent, producer start/exit, inventory/receipt commit, response loss, reclamation
intent/consumption, first and third unlink, final absence and committed response.
Journal write/sync/replace error injection exercises refusal and restart. Both
platforms must retain uncertain bytes and complete a fresh replacement cycle.
These are process-crash and injected IO tests, not storage power-loss tests.

## Deployment, update and rollback requirements

The lab bootstrap creates a unique disposable root, ordinary account, private
control directory, service configuration and native service registration. Only
free standard GitHub-hosted macOS/Windows runners are allowed. The test driver
removes its native service registration on completion; fixture data expires with
the VM. It never touches the working host's installed runtime, profiles, daemon,
credentials or caches. Artifacts contain test logs and measured build provenance.

A future production proposal must name the exact service identity and signed
binary, protected installation/control paths, every ancestor ACL, the trusted
daemon's exclusive credential provisioning, revocation/rotation, IPC policy and
the task process identity. It must define a writable Codex home layout compatible
with all ordinary Codex writes while protecting both cache parents. Making an
entire home read-only is not that integration. It must preserve least-privilege producer acquisition,
enforce complete launch/restore/termination supervision, bound retained
quarantine and request journals, and specify cleanup authority for preserved data.

Upgrade must stop new reservations, drain or explicitly retain active leases,
verify the replacement binary and schema compatibility, atomically switch only
protected bootstrap configuration and start a new epoch. A new schema must have
an explicit migration/rollback record; this version refuses unknown schemas.
No update may infer terminal state from a stopped service or remove old generations.
An incompatible recipe uses a new service namespace instead of changing the
meaning of existing evidence.

Rollback uses `ReadOnly` mode (queries only) or stops the experimental service;
it preserves registry, generations and quarantine and keeps production populated
GC disabled. There is no fallback to legacy recursive deletion. Credential
rotation requires a drained epoch transition and new pins on both endpoints;
the lab does not implement an automatic rotation protocol.

Before any working Desktop deployment, return a separate concrete approval plan:
installation/update/rollback commands and identities, path/ACL inventory, native
crash and noninterruption evidence, exact release/installed commit, safe run →
restore → GC proof, coordinator/owner and a single continuation event. The current
experiment does not authorize that deployment. Existing L2 LYNAIX-1281 reviews
this exact delta; no new Ask3/Jev series is created.

## Runtime integration experiment

The disposable test now uses Multica's actual `agent.New("codex").Execute`
backend, its initialize/thread-start or resume/turn lifecycle and Result channel.
A measured stdio transport forwards it over pinned mTLS to a fixed offline
app-server fixture under a separate ordinary identity. The producer remains
pinned real Codex source; the runtime endpoint is a protocol fixture, not a full
installed Codex app-server or a paid model call. No caller chooses its executable,
arguments, UID/SID, paths or containment policy.

A persistent privileged supervisor owns launch authority and native containment.
The restartable driver uses a private client credential outside task read access;
the supervisor, service and host administrator remain in the privileged trust boundary. It reserves an exact run/lease/generation/identity durably before
launch, never reuses identities, and seals future launches before querying native
quiescence. Driver Result, leader exit, pipe closure, elapsed time and a restarted
cache service are insufficient evidence. Native query failures and a reopened
supervisor journal without its original domain retain data. This last check is a
journal reopen test, not a crash of the whole supervisor test harness.

On Windows, an ordinary token starts suspended, is assigned to a private,
non-inherited Job without breakaway, then resumes. The supervisor queries actual
Job ActiveProcesses after the launch authority is sealed. Child/grandchild
processes inherit the Job; breakaway is tested. The fixed offline fixture cannot
create an external process broker. **A Job alone is not a sandbox for arbitrary
untrusted code**: delegation to another system service or user session would
require additional IPC/identity confinement before production authority is safe.
No generic Windows sandbox or installed Codex compatibility is claimed.

On macOS, every run receives a fresh nonlogin UID/GID and an inherited sandbox:
only the measured fixture may execute, no network/external launch broker or
privilege transition is allowed, and writes are limited to the home/workspace.
A root-only libproc helper uses `proc_listpids(PROC_RUID_ONLY)` to count every
process with that immutable real UID, including detached and reparented ones.
The bounded 65,536-PID buffer rejects errors, malformed results and possible
truncation. Fresh-UID census must be empty before launch. Sole launch authority,
no external delegator and the inability to change RUID are essential assumptions;
PID, PPID or process-group absence is never substituted for that census. The
native artifact records OS/kernel version; other kernels need their own proof.

The exact home, `.tmp` and `cache` parents permit creation of ordinary task
outputs while remaining protected against replacement. On macOS they are
root-owned sticky 01770 with the exact run GID, no ACL and no other-writes. On
Windows the exact SID receives create-file/create-directory rights, but no
DELETE_CHILD, DELETE, WRITE_DAC or WRITE_OWNER on those parents. Published cache
children remain privileged-owned and immutable to task identities. Task-created
siblings remain outside reclamation. Every other ancestor keeps the original
strict policy. This proves only the tested writable layout, not compatibility
with every Codex home write (for example a plugin self-update).

The native matrix runs both GC routes, both exact artifacts and two fresh
generations. After a real adapter Result it kills the driver while an ordinary
double-forked descendant still reads the cache, starts a recovery driver, restarts
the cache service and checks zero deletion/credit. Only a successful native query
after the fixture child exits allows end/reconcile, a new idle period and GC.

## Service capacity and remaining production storage plan

Implemented admission bounds per experimental service namespace are 64 homes,
64 generations per home, 1,024 retained request outcomes and a 4 MiB registry on
both read and write. At 16 quarantined artifacts, new registration/begin/prepare
is refused. Existing outcomes remain queryable; no journal or uncertain cache is
evicted to make room. A configured finite runtime identity list permits one home
and refuses further generations when exhausted. Supervisor journals accept at
most 64 never-reused run identities and 1 MiB on read. A registry write failure
(including a byte cap) latches mutation authority; recovery preserves uncertainty.
A full request journal stops mutations, including end/reconcile: an operator must
retain that namespace and resolve it, never truncate its idempotency history.

These are metadata/admission limits, **not an enforced aggregate disk quota**.
Partial producer output, writable task outputs and unknown historical files can
exceed them. Disposable VMs bound this lab's lifetime and disk; they do not prove
production storage bounds. Before production, provision a separate quota-backed
staging/retention volume: proposed maximum 1 GiB per producer generation and
16 GiB retained quarantine per namespace, with refusal of new acquisition on
exhaustion and no automatic deletion of unknown data. Keep task-owned outputs in
a separate quota domain. Review these values against measured real workloads;
changing them grants no cleanup authority. Rotation requires explicit draining,
a verified archive of outcomes/evidence and a new namespace/epoch; no expiry may
turn an active/unknown lease into terminal. Expired uncertain data is an operator
obligation, not deletion permission.

Service `prepare` uses the staged acquisition path below with a measured
recipe under a fresh producer identity and the same closed native containment, with access only
to protected read-only source, recipe, approved network fixture and bounded
staging. It cannot read service/driver credentials or write publication/journals.
Before publication, seal launch authority and prove the entire producer domain
quiescent. Reject links, unsupported metadata, unexpected Git/content, output
outside staging or incomplete acquisition/readback evidence. Move/copy validated
bytes into a new privileged-owned namespace with mutation rights revoked, then
inventory that immutable publication and commit its receipt. Never treat producer
Wait alone as proof or adopt output after uncertain termination. A malicious or
compromised pinned producer remains outside this experiment's proven boundary.
The direct historical `Broker.Materialize` tests remain a separate privileged
control path; the service never falls back to them. Production retention limits
and Windows IPC controls still require review before any working deployment.


## Isolated staged acquisition and supervisor-process crash

`Broker.StartProduction` and its live `Production` capability implement the
experimental service acquisition path as well as the direct producer probes. A fresh ordinary producer UID/SID runs the pinned
Rust acquisition/restore implementation in an empty staging directory. The
privileged supervisor durably binds run/lease/generation/identity before launch;
its identity cannot be reused. The native test trampoline adds controlled
failure and detached-writer probes around the measured producer. The trampoline,
producer and native census are recorded in build provenance. No service/driver
credential is supplied to the ordinary process.

macOS staging uses a 128 MiB fixed APFS disk image; Windows uses a 128 MiB fixed
NTFS VHD. The image is provisioned only in a disposable hosted VM. The importer
queries native filesystem identity and capacity before launch and publication,
requires a different filesystem from task results/publication, and refuses a
changed, unbounded or incorrectly located stage. All successful and failed
stages on that volume remain present and consume its finite aggregate capacity.
Native tests actually write until the OS returns capacity exhaustion; an ordinary
successful process exit or a configured byte limit is not that proof. Each stage
also has an 8 MiB logical-byte and 2,048-entry import limit, which rejects sparse
oversized files. A volume is detached at test teardown; its image and retained
bytes expire with the disposable VM. There is no automatic eviction.

On macOS, the inherited producer sandbox permits Git/helper execution, writes
stored data only under the exact staging directory, permits the `/dev/null` sink,
and networking only on loopback for the catalog fixture. Bootstrap resolves Git
before launch and copies its executable and local upload-pack helper into a
protected directory. A measured wrapper fixes that helper path, avoiding Xcode
discovery under a producer UID. Native evidence records the selected executable,
protected copies and wrapper hashes. The native platform toolchain remains
trusted. The sandbox denies external launch-service IPC; the RUID census still
requires every process under that identity to exit, including any platform
service started indirectly. On Windows the
ordinary token enters a private, noninherited, kill-on-close Job while suspended,
then resumes. ACLs exclude other ordinary staging writers and all ordinary
publication/control writers. As in the runtime fixture, a Windows Job does not
confine arbitrary delegation to external process brokers. The pinned, measured
producer is trusted not to perform such delegation; generic confinement remains
unproven.

Publication requires a successful leader exit AND native quiescence of its whole
sealed domain. A live detached writer, failed query, recovered journal, partial
acquisition evidence or exceeded physical/logical capacity cannot mint a receipt.
The importer rejects nonregular entries, links, foreign owners/writers, unsupported
native metadata and incomplete/wrong source evidence. It copies bounded bytes
through `os.Root` into a new privileged tree, never changes source ownership, and
publishes with native no-replace rename. Unknown destination instances survive.
Only the immutable-to-ordinary-identities publication is inventoried for GC;
staging and task outputs are never GC targets. Copy/rename/journal uncertainty
retains an unproven intent and partial publication. Sixteen retained partial
publication roots stop admission; this is a count limit, not an aggregate
publication filesystem quota.

The native suite verifies two artifacts, two GC routes and two generations, with
a live detached writer denying each initial publication. The incomplete-evidence,
symlink/hardlink, logical-capacity and physical-capacity matrix preserves unknown staging bytes,
task results and the other exact cache. Content/link/logical-quota probes must
first establish whole-domain quiescence so an unrelated live-process refusal
cannot count as input-validation coverage. A separate four-case matrix kills the
actual supervisor process after leader exit while its descendant is active.
macOS demonstrates the orphan survives and must be preserved; Windows demonstrates
Job closure terminates it. In both cases a new supervisor process cannot recover
native proof, replay launch or adopt the stale broker lease, and no receipt is
committed. Manual recovery of that retained lease/stage is deliberately unresolved.
This tests process death, not machine/power loss.

Service `prepare` and its runtime-adapter fixtures now use staged acquisition.
Production GC and installed Desktop remain unchanged. Real external source/auth
and installed Codex home compatibility remain unverified. Aggregate publication/retention quotas, image
lifecycle in a real deployment, signed executable identity, credential rotation,
Windows external IPC isolation and installer/update/rollback remain separate
production obligations. None of these test results authorizes merge, installation
or live cleanup of user environments.

## Staged acquisition in the native service

The protected bootstrap supplies a finite generation/artifact slot table with
fresh ordinary identities and empty stages on fixed 128 MiB volumes. One service
home owns that table. No IPC field selects an identity, path, executable or quota.
`ServiceConfig.Production` is an in-process bootstrap factory excluded from JSON
requests; a missing factory refuses preparation before any intent. The native
service command supplies the factory, with no privileged-producer fallback.

The service binds each acquisition to its current run, lease and generation.
Receipts carry the service generation, even though each generation has a new
broker namespace. Its supervisor reserves the binding durably before launch. The
ordinary launcher creates only its staging prerequisites, then starts the pinned
producer in the same inherited native domain. Source evidence is copied into the
protected receipt outside the task cache. A bounded wait may observe native
quiescence, but timeout/error never counts as proof; Publish queries again.

The bootstrap provides two generations for each artifact per test installation.
A restarted service cannot reuse a used slot: PREPARING becomes QUARANTINED, its
native proof is lost, and old epoch/revision requests cannot launch it again. A
new generation uses different stage and identity. Supervisor journals, failed
stages, broker leases left by process death and partial publications stay intact.
A completed READY result survives a lost response, but its current lifecycle is
UNKNOWN until the independent trusted runtime authority reconciles it.

The publication fault matrix covers both GC routes, both artifacts and both
generations, with real service exit and injected IO error at each boundary:

| Boundary | Durable receipt | Service recovery |
| --- | --- | --- |
| After bounded copy | UNPROVEN; protected partial tree remains | QUARANTINED |
| After no-replace rename | UNPROVEN; final tree remains | QUARANTINED |
| Before receipt commit | UNPROVEN; final tree remains | QUARANTINED |
| After receipt commit | COMMITTED; final tree remains | QUARANTINED |
| Before final service registry commit | COMMITTED; final tree remains | QUARANTINED |
| After final service registry commit | COMMITTED with committed request outcome | READY, lifecycle UNKNOWN |

Tests verify that the requested fault actually fired after producer completion,
check receipt and service states separately, query the outcome, refuse old
requests/replay, and invoke both daemon GC routes. Unknown lifecycle grants zero
credit. Quarantine remains excluded even after trusted reconciliation; a fully
committed outcome can be reclaimed after reconciliation and idle TTL. Snapshots
cover staging, partial/final publications, unknown content in the other exact
cache and task results through restart and replacement. Existing native service
cycles, fault/IO tests and runtime cycles also execute this staged prepare path.

These are process-crash/IO results on disposable machines. They do not establish
power-loss ordering, generic Windows IPC containment, publication/retention/log
volume limits, installed Codex compatibility or production deployment authority.
