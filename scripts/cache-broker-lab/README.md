# Isolated cache broker experiment

This is an experimental implementation for LYNAIX-1249. It does not install a
service on the working host and does not enable populated-cache deletion in the
Multica runtime. The ordinary GC implementation remains unchanged.

The protected helper executes only in disposable native CI machines, as root
(macOS) or LocalSystem (Windows). A newly created ordinary task account runs both
a sample requester and an unrestricted neighbouring attacker. The bootstrap
administrator is outside the threat model. The current desktop user is not
protected merely because these tests pass.

`server/internal/daemon/cachebrokerlab` contains the measured producer transaction
and an experimental service. `server/cmd/cache-broker-lab` is excluded from normal
builds and is installed only in the disposable runner: launchd on macOS, SCM as
LocalSystem on Windows. The SYSTEM scheduled task on Windows runs the trusted
test driver; the broker itself is a separate SCM service.

The privileged driver registers fresh homes and reserves use leases through
loopback TLS 1.3 with mutual authentication and exact peer certificate pins.
Private credentials live in a root-only / SYSTEM-and-administrator-only control
directory. This authenticates possession of the daemon credential, not the
identity of an installed Multica executable. Ordinary requester and neighbour
processes share the same unprivileged identity and cannot steal the credential,
modify protected paths, or call registration/lifecycle/reclamation operations.

The test-only patch connects authenticated IPC to both existing direct and walk
GC routes in a separate source copy. Neither a receipt nor a boolean enables
production GC. A stable kernel lock excludes competing service writers. Immutable
generation paths, service epochs, home revisions, persisted request results and
conservative recovery reject stale operations and retain uncertain remnants.
See [the service contract](service-contract.md) for exact authority and recovery
rules, including the remaining production requirements.

`codex.patch` applies to source 83086188ae0614bd882818ff1aa23f3194962012.
The patch makes cache write success observable and adds an explicitly gated test
entry point. Marketplace tests execute actual Git clone/install and await restore
before the pinned discovery function. Git transport maps one fixed test URL to a
local fixture at an immutable OID. Catalog tests invoke actual fetch-on-miss and
cache read code against a local HTTP mock; there are no model or account calls.
The upstream release manifest says 0.163.0 while its lock records workspace
packages as 0.0.0. The lock delta updates those workspace versions; external
package versions remain pinned.

Run the workflow on the public fork with standard hosted macOS/Windows runners.
`build.py --output <new-directory>` builds isolated copies. Native bootstrap
scripts require GitHub-hosted disposable environments. Never execute those
bootstrap scripts against a working installation. The test VM and test identities
expire with the job. Logs and build provenance are saved; fixture account
credentials and cache contents are excluded from uploaded artifacts.

This experiment is not an independent review or a complete production broker.
Native service tests add registration, authenticated lifecycle, actual ordinary
readers, two GC routes and second cycles, crash/restart boundaries, and injected
journal IO errors. Process termination tests do not establish power-loss
durability. Production daemon credential provisioning, task isolation, privilege
reduction, lifecycle supervision, storage bounds and installed Desktop integration
still need implementation and the existing L2 review. Historical/unknown caches
remain preserved. Read exact-commit native results before claiming platform
acceptance; cross-compilation is not a native result.
