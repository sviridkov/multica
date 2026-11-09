# Profile writer contract

Every supported writer of Multica `config.json` uses `LoadCLIConfigForProfile`
then `SaveCLIConfigForProfile`. Loading retains the path, existence and exact
bytes. Saving acquires the permanent `.config.lock` in that profile directory,
compares the snapshot while holding the lock, validates known settings, writes
and syncs a private temporary file, and atomically renames it over the config.
The lock covers comparison through publication. Closing it releases the lock;
**never unlink the lock file**. Unix uses flock, Windows uses LockFileEx.

A competing writer, different profile, stale snapshot or create-only literal
against an existing file returns `ErrCLIConfigConflict`. Reload and reapply the
intended edit, including any confirmation/auth decision that depended on the
old data. Do not retry the old snapshot. Every successful save consumes the
snapshot in effect: loading again is required for another save. Unchanged-byte
ABA writes are equivalent under this content-revision contract; it is not a
monotonic audit/version counter.

The contract requires local filesystems with the documented lock/rename
semantics and cooperating writers. Older binaries, hand-written scripts,
editors, symlink replacement and arbitrary same-user processes can bypass
advisory locking. This is not a filesystem CAS against those processes. During
future rollout, quiesce all supported old writers and upgrade Desktop with its
matching CLI; do not mix a new Desktop with an old CLI. No profile mutation or
rollout is performed by this change.

## JSON behavior

Unknown keys are stored as `json.RawMessage` at the root, `backends` and
`backends.openclaw` typed-object levels. Their nested objects, arrays, nulls and
exact numeric values survive serialization. Recognized keys are excluded from
the unknown map, so clearing an override cannot merge it back. Whitespace/key
order is not preserved. Malformed JSON, wrong known types and invalid duration
or concurrency settings fail without rewriting the original file. OpenClaw's
`cli_timeout` retains its documented ignore/clamp behavior.

Setup deliberately resets known settings, including credentials, but preserves
unknown fields via `ResetSettings`. It retains the snapshot shown during
confirmation, so a change during the server probe also conflicts. A failure to
load config is an error, not permission to replace it.

| Supported writer | Transaction entry |
| --- | --- |
| `config set` / empty-value unset | `cmd_config.go`: load, optional expected revision, edit, save |
| Browser/token login and logout | `cmd_auth.go`: propagate load errors, edit loaded snapshot, save |
| Login URL and PAT persistence | `cmd_login.go`: load, edit, save |
| Cloud/self-host setup | `cmd_setup.go`: confirmed snapshot, reset known settings, save |
| Workspace selection | `cmd_workspace.go`: load, edit, save |
| Runtime profile executable set/unset | `cmd_runtime_profile.go`: load, edit, save |
| Desktop initialize/sync-token/clear-token | `daemon-profile-config.ts` → hidden `config patch-auth` → same Go save |

Desktop reads only its two owned fields into JavaScript and hashes the original
bytes. It sends the expected revision and a patch of `token`/`server_url` on
stdin to the selected CLI. It never serializes the whole parsed document (which
would round large unknown numbers). Go loads the entire document, validates the
revision and publishes under the same sidecar lock. Invalid config, unavailable
CLI, old CLI without the command and conflicts all fail without a direct-write
fallback. Credentials are not in argv or output. The unresolved-profile guard
remains in `daemon-profile.ts`; Desktop still owns only its selected profile.
The `.desktop-user-id` sidecar and Desktop preferences are separate files, not
part of an atomic multi-file auth transaction.

## Conditional rollback

Before any later authorized rollout, privately retain the exact original bytes,
permissions and revision outside cleanup roots. Do not log or attach a real
credential-bearing backup. Record the post-write SHA256 and verify the intended
diff/readback. To undo a setting, use the original value (empty for absent) with
`config set KEY ORIGINAL --expect-revision POST_WRITE_SHA256`. This compares
under the shared save lock as well as at command load; if any newer bytes exist,
rollback refuses. Reload, inspect privately and reconcile instead of restoring
a stale backup. The conditional-rollback regression demonstrates that refusal.

Whole-file recovery must also run under the permanent sidecar lock, require
exact expected current bytes, validate the backup and atomically publish it.
A raw `cp backup config.json`, pre-check followed by an unlocked rename, or
reverting the binary alone is **not** an approved rollback procedure. This
package implements conditional setting rollback, not an unrestricted backup
restore command. No live backup was created.

## Focused verification

Go coverage includes unknown-value preservation (the former SKIP is active),
unset, malformed/invalid profiles, publication failure, two writer processes
for new/existing profiles, distinct profiles, stale snapshots with a newly
introduced key, setup reset/probe conflicts, Desktop patch validation and
conditional rollback. Run `internal/cli` tests plus the config/setup/auth/runtime
profile selections in `cmd/multica`, and TTL/LoadConfig/ParseFlexDuration in
`internal/daemon`, with race; vet those three packages. Tests must execute in a
new temp working directory outside managed task roots. Compile with `go test -c`
first if the checkout inherits a real task-context marker; never remove or
weaken the production guard.

Build the CLI to an explicit fixture path, then run the Desktop bridge tests:

```sh
MULTICA_PROFILE_TEST_BINARY=/absolute/path/to/fixture-cli \
  node --test apps/desktop/src/main/daemon-profile-config.node-test.mjs
```

This dependency-free smoke test uses Node's TypeScript stripping (Node 22.18+
recommended), synthetic HOME/profile/cwd and the explicit built CLI, never a
PATH-resolved real CLI. The new helper can be typechecked independently of the
Electron/UI suite. Windows locking is crosscompiled; native Windows validation
must be reported separately.
