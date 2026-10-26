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
the threat model. Every ancestor must exclude ordinary mutations. The control
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
3. `prepare(leaseID, runID, artifact)` persists PREPARING, runs only the measured
   recipe, validates publication/readback and commits READY. Restore precedes
   discovery. The native driver starts an ordinary reader only after this returns.
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
entire home read-only is not that integration. It must also reduce producer
privileges, enforce complete launch/restore/termination supervision, bound retained
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
