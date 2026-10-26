# Isolated cache broker experiment

This is an experimental implementation for LYNAIX-1249. It does not install a
service on the working host and does not enable populated-cache deletion in the
Multica runtime. The ordinary GC implementation remains unchanged.

The protected helper executes only in disposable native CI machines, as root
(macOS) or LocalSystem (Windows). A newly created ordinary task account runs both
a sample requester and an unrestricted neighbouring attacker. The bootstrap
administrator is outside the threat model. The current desktop user is not
protected merely because these tests pass.

`server/internal/daemon/cachebrokerlab` owns one pre-registered namespace,
producer launch, generations, protected journal and validation/removal. It
rejects ordinary same-UID construction and unknown existing trees. The test-only
patch connects its transaction to both existing direct and walk GC routes in a
separate source copy. Neither a local receipt nor a boolean enables production
GC. After a crash, a stale exclusive lease or incomplete journal requires explicit
recovery; the helper does not adopt or delete the remaining data.

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
Dynamic trusted registration, production IPC and lifecycle authority, least
privilege deployment, full crash recovery and working-host installation still
need implementation and the existing L2 review. Historical/unknown caches remain
preserved. Native test results must be read from the exact commit's run before
claiming platform acceptance; cross-compilation is not a native result.
