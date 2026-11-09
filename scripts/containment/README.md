# Offline task cgroup experiment

This is a synthetic experiment, not a production launcher or GC implementation.
It can demonstrate containment of processes in **one task container**. It cannot
remove the host user's existing Docker authority. `production_cleanup_allowed`
is always false, even when task-cgroup evidence passes.

The controller uses the existing explicitly named local Docker context. It
imports a one-file rootfs containing a self-built static Linux/arm64 Go probe;
it does not pull images, use an unverified local base, fetch dependencies or
contact a provider/server. Keep compiler/source/binary/rootfs/image hashes with
the run. The Docker daemon and compiler remain part of the trusted environment;
this is not an independent attestation of either.

## Reproduce

Build using the already installed compatible Go toolchain, offline, then pass
the explicit digest (do not resolve a different Docker context implicitly):

```sh
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 GOPROXY=off GOSUMDB=off \
  go build -trimpath -ldflags=-buildid= -o /tmp/containment-probe scripts/containment/probe.go
shasum -a 256 /tmp/containment-probe
python3 -m unittest discover -s scripts/containment -p 'test_*.py' -v
python3 scripts/containment/run.py --context desktop-linux \
  --probe-binary /tmp/containment-probe --probe-sha256 DIGEST \
  --output /absolute/new/evidence-directory
```

The output directory must not exist. The controller records exact IDs as objects
are created, inspects the boundary before start, and collects all results before
returning. At most task + observer run concurrently: each 1 CPU, 512 MiB RAM/swap
limit, 128 PIDs, network none, read-only rootfs, all capabilities dropped,
no-new-privileges, no restart policy. No host PID namespace, socket, credentials,
host bind mount or writable cgroup mount is passed. Only the task gets its newly
created labelled synthetic volume. The observer has a host cgroup namespace
(read-only kernel view), private PID namespace and no task-volume mount. The
whole execution budget is 20 minutes, with its last minute reserved for cleanup.
Probe processes also have finite deadlines. No fallback runs on the host.

## Evidence and refusal

The task starts direct, setsid and double-fork writers. Exiting both intermediate
processes reparents the last writer to PID1; its PID/PPID/session ID is recorded
after a successful synthetic write. A delayed writer and a writer released by
PID1's TERM handler test writing during the graceful-stop window.

The host admission lock covers launch acknowledgement. Closing it waits for
accepted launches and rejects all later attempts. Racing `docker exec` admissions
are acknowledged before stop, and eight post-close attempts must be denied. The
TERM-triggered writer starts before closure and writes only after the stop marker
appears. Causality is used instead of comparing host/VM clocks. The gate stays
closed until cleanup is over.

The observer opens the exact task's `cgroup.events`, derived from its own sibling
path and the full task container ID. It requires `populated 1` followed by
`populated 0`, which covers descendants regardless of PID namespace, session or
parent. An unrecognized path/layout, vanished file without a zero observation,
permission error, failed stop, failed admission or observer error yields
**NOT PROVEN**. `docker wait`, Docker's exited state and the main PID alone
cannot authorize cleanup. Unit regressions remove/falsify each required signal,
including operation failure, and prove that cleanup is denied.

Only stopped, exact, labelled own containers are removed. The synthetic volume
is removed only with complete verifier evidence and a closed admission gate.
On failure it remains with its handle in `handles.json`; do not prune it or
reinterpret a later experiment as proof for it. Imported images are retained
with their exact IDs for reproducibility. No durable service is left running.

## Production coverage still required

Map checked against Desktop 0.8.1 base `b1ea927d`; these launchers are unchanged:

| Surface | Source entry | What remains |
| --- | --- | --- |
| Provider tasks and version/model probes | `server/pkg/agent/launch.go`: `Command.exec`, `execVia`, `newRuntimeCmd`, `runOwned`; `proc_other.go` | Move every launch/admission under an enforceable task boundary; Unix groups permit setsid/reparent escape. Windows Job Objects need native evidence. |
| Preparation/reuse and descendants | `daemon.go`: `prepareExecutionEnvironment`, `reuseExecutionEnvironment`; `execenv/isolation.go`, `isolation_unix.go` | Production uses isolated helper with Unix group; zero-valued test Daemon has an in-process path. Neither is this cgroup harness. No production host fallback may be introduced. |
| Plugin provisioning/discovery | `dsh_profile.go`: `dshProvisionCommand`; `agents_probe.go`; `processtree/run.go` | Include helper/provisioner subprocesses and descendants, not just provider leader. |
| CLI-provided plugins, MCP and hooks | `execenv/codex_home.go`, `daemon.go`: `cursorSupplementHookCommand`; `pkg/agent` adapters | Runtime/plugin shell descendants inherit boundary only if all entry points and writable paths are controlled. Native hook/MCP compatibility has not been tested. |
| Git and lifecycle metadata writers | `execenv/git.go`, `local_worktree.go`, `repocache/cache.go`, `daemon.go` completion path | Parent daemon/helper writes can occur after server terminal status. Drain writers and close admission across full lifecycle before GC. |
| Desktop and other host writers | `apps/desktop/src/main/daemon-manager.ts`, auto-update/probes, user tools | Config transaction fix does not restrict access to task roots, Docker or caches. |
| Host Docker authority | Existing UID/socket/context | Same-UID host process can issue engine commands or mount a volume; task-container success does not close this route. |

A process snapshot is not proof that these paths cannot write. The existing
profile reservation and checks immediately before unlink do not make arbitrary
external control-file replacement atomic. Candidate GC must remain entirely
disabled until independent acceptance of writer exclusion; TTL zero disables
only the age policy. This is a future rollout condition, not a claim that this
experiment changed or disabled current production GC.

## Concrete management-isolation proposal (not implemented)

Use a supervisor under a distinct OS principal, owning a separate local Linux VM
and its rootless engine/state. UID501 and task processes must not hold its engine
socket, credentials, sudo access or writable VM state. Expose a narrow authenticated
broker for immutable job specifications and readback, not raw Docker/exec/mount
API. The supervisor closes admission, stops/verifies the whole job cgroup, then
allows cleanup. Task mounts contain only copied/synthetic inputs and task-owned
outputs. Container failure has no host fallback.

Compatibility work: Linux/arm64 provider binaries, credentials scoped to a job,
MCP transports, plugin hooks and macOS-specific tools; native macOS-only hooks
would remain unavailable rather than silently run outside the boundary. Existing
Desktop profile selection and CLI config transaction stay local. Artifacts need
an explicit export path whose owner is outside deletion scope.

Rollback: close new-job admission, stop and verify jobs, preserve/export results,
keep candidate GC disabled, and switch Desktop/CLI together to the recorded
compatible version. Restore only specific config values with the expected
revision guard. Do not remove a VM/volume or overwrite a newer profile as part
of rollback without separate evidence and authorization.

Cost: local open-source VM/rootless-engine software can have zero licence cost;
CPU/RAM/disk and engineering/maintenance still have a cost. No paid service is
required by this proposal and no spending is authorized. Creating the principal,
VM/runtime/access boundary requires a separate concrete decision. No such object
or permission change is created here. The free immediate alternative is retaining
full task roots and keeping the future candidate's GC off while approved narrow
cache work continues separately; it sacrifices whole-root space reclamation.
