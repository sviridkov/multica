#!/bin/bash
set -euo pipefail
build_dir="$1"
case "$build_dir" in /*) ;; *) exit 2;; esac
# This script is intentionally restricted to an ephemeral GitHub-hosted VM.
[[ "${GITHUB_ACTIONS:-}" == true && "${RUNNER_ENVIRONMENT:-}" == github-hosted ]]
lab_root="/private/var/db/multica-cache-lab-${GITHUB_RUN_ID}-${GITHUB_RUN_ATTEMPT}"
lab_user="cachelabtask"
lab_uid=490
runtime_uid_base=60000
require_unused_identity() {
  local found
  found="$(dscl . -search "$1" "$2" "$3")"
  if [[ -n "$found" ]]; then
    printf 'Identity already exists: %s %s %s\n' "$1" "$2" "$3" >&2
    exit 5
  fi
}
# Complete every directory query before creating anything. A failed query must
# abort too; missing optional search tools must never bypass collision checks.
require_unused_identity /Users UniqueID "$lab_uid"
require_unused_identity /Users RecordName "$lab_user"
for index in $(seq 0 511); do
  runtime_uid=$((runtime_uid_base+index))
  runtime_user="cachelabrun$index"
  require_unused_identity /Users UniqueID "$runtime_uid"
  require_unused_identity /Groups PrimaryGroupID "$runtime_uid"
  require_unused_identity /Users RecordName "$runtime_user"
  require_unused_identity /Groups RecordName "$runtime_user"
done
sudo dscl . -create "/Users/$lab_user"
sudo dscl . -create "/Users/$lab_user" UniqueID "$lab_uid"
sudo dscl . -create "/Users/$lab_user" PrimaryGroupID 20
sudo dscl . -create "/Users/$lab_user" UserShell /bin/bash
sudo dscl . -create "/Users/$lab_user" NFSHomeDirectory /var/empty
sudo mkdir -m 0755 "$lab_root"
sudo cp -X "$build_dir/producer" "$build_dir/broker-test" "$build_dir/broker-service" "$build_dir/runtime-census" "$lab_root/"
sudo chown -R root:wheel "$lab_root"
sudo chmod 0755 "$lab_root/producer" "$lab_root/broker-test" "$lab_root/broker-service" "$lab_root/runtime-census"
for index in $(seq 0 511); do
  runtime_uid=$((runtime_uid_base+index))
  runtime_user="cachelabrun$index"
  sudo dscl . -create "/Groups/$runtime_user"
  sudo dscl . -create "/Groups/$runtime_user" PrimaryGroupID "$runtime_uid"
  sudo dscl . -create "/Users/$runtime_user"
  sudo dscl . -create "/Users/$runtime_user" UniqueID "$runtime_uid"
  sudo dscl . -create "/Users/$runtime_user" PrimaryGroupID "$runtime_uid"
  sudo dscl . -create "/Users/$runtime_user" UserShell /usr/bin/false
  sudo dscl . -create "/Users/$runtime_user" NFSHomeDirectory /var/empty
  sudo dscl . -create "/Users/$runtime_user" AuthenticationAuthority ';DisabledUser;'
done
# Resolve the toolchain before entering any ordinary producer domain. Keep its
# executable and local transport helper in the protected lab namespace so Git
# never invokes the Xcode discovery shim under a producer UID.
selected_git="$(/usr/bin/xcrun --find git)"
git_core="$lab_root/git-core"
git_bin="$lab_root/git-bin"
sudo mkdir -m 0755 "$git_core" "$git_bin"
for command in git git-upload-pack; do
  sudo cp -X "$selected_git" "$git_core/$command"
  sudo chown root:wheel "$git_core/$command"
  sudo chmod 0755 "$git_core/$command"
done
python3 - "$git_core" > "$build_dir/git-wrapper" <<'PYGIT'
import shlex,sys
core = sys.argv[1]
print('#!/bin/sh')
print('export GIT_EXEC_PATH=' + shlex.quote(core))
print('exec ' + shlex.quote(core + '/git') + ' "$@"')
PYGIT
producer_git="$git_bin/git"
sudo cp -X "$build_dir/git-wrapper" "$producer_git"
sudo chown root:wheel "$producer_git"
sudo chmod 0755 "$producer_git"
[[ "$("$producer_git" --exec-path)" == "$git_core" ]]
python3 - "$lab_root" "$lab_uid" "$producer_git" "$runtime_uid_base" > "$build_dir/native-config.json" <<'PYCONFIG'
import json,sys
base = int(sys.argv[4])
print(json.dumps(dict(Root=sys.argv[1],Producer=sys.argv[1]+"/producer",Service=sys.argv[1]+"/broker-service",Census=sys.argv[1]+"/runtime-census",Git=sys.argv[3],UID=int(sys.argv[2]),GID=20,RuntimeIdentities=[dict(UID=base+i,GID=base+i) for i in range(512)])))
PYCONFIG
sudo cp "$build_dir/native-config.json" "$lab_root/config.json"
sudo chown root:wheel "$lab_root/config.json"
sudo chmod 0600 "$lab_root/config.json"
id "$lab_user" > "$build_dir/native-identity.log"
sw_vers >> "$build_dir/native-identity.log"
uname -a >> "$build_dir/native-identity.log"
printf 'producer_git=%s\n' "$producer_git" >> "$build_dir/native-identity.log"
shasum -a 256 "$producer_git" >> "$build_dir/native-identity.log"
printf 'selected_git=%s\n' "$selected_git" >> "$build_dir/native-identity.log"
shasum -a 256 "$selected_git" >> "$build_dir/native-identity.log"
printf 'git_exec_path=%s\n' "$git_core" >> "$build_dir/native-identity.log"
shasum -a 256 "$git_core/git" "$git_core/git-upload-pack" >> "$build_dir/native-identity.log"
"$producer_git" --version >> "$build_dir/native-identity.log"
sudo -u "$lab_user" sudo -n true && exit 4
set +e
sudo env MULTICA_NATIVE_BROKER_CONFIG="$lab_root/config.json" "$lab_root/broker-test" -test.run '^TestBrokerNative' -test.v -test.timeout 25m > "$build_dir/native.log" 2>&1
result=$?
set -e
cat "$build_dir/native.log"
exit "$result"
