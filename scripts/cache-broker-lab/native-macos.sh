#!/bin/bash
set -euo pipefail
build_dir="$1"
case "$build_dir" in /*) ;; *) exit 2;; esac
# This script is intentionally restricted to an ephemeral GitHub-hosted VM.
[[ "${GITHUB_ACTIONS:-}" == true && "${RUNNER_ENVIRONMENT:-}" == github-hosted ]]
lab_root="/private/var/db/multica-cache-lab-${GITHUB_RUN_ID}-${GITHUB_RUN_ATTEMPT}"
lab_user="cachelabtask"
lab_uid=490
if dscl . -search /Users UniqueID "$lab_uid" | rg .; then exit 3; fi
sudo dscl . -create "/Users/$lab_user"
sudo dscl . -create "/Users/$lab_user" UniqueID "$lab_uid"
sudo dscl . -create "/Users/$lab_user" PrimaryGroupID 20
sudo dscl . -create "/Users/$lab_user" UserShell /bin/bash
sudo dscl . -create "/Users/$lab_user" NFSHomeDirectory /var/empty
sudo mkdir -m 0755 "$lab_root"
sudo cp -X "$build_dir/producer" "$build_dir/broker-test" "$build_dir/broker-service" "$build_dir/runtime-census" "$lab_root/"
sudo chown -R root:wheel "$lab_root"
sudo chmod 0755 "$lab_root/producer" "$lab_root/broker-test" "$lab_root/broker-service" "$lab_root/runtime-census"
for index in $(seq 0 47); do
  runtime_uid=$((600+index))
  runtime_user="cachelabrun$index"
  if dscl . -search /Users UniqueID "$runtime_uid" | rg .; then exit 5; fi
  if dscl . -search /Groups PrimaryGroupID "$runtime_uid" | rg .; then exit 5; fi
  sudo dscl . -create "/Groups/$runtime_user"
  sudo dscl . -create "/Groups/$runtime_user" PrimaryGroupID "$runtime_uid"
  sudo dscl . -create "/Users/$runtime_user"
  sudo dscl . -create "/Users/$runtime_user" UniqueID "$runtime_uid"
  sudo dscl . -create "/Users/$runtime_user" PrimaryGroupID "$runtime_uid"
  sudo dscl . -create "/Users/$runtime_user" UserShell /usr/bin/false
  sudo dscl . -create "/Users/$runtime_user" NFSHomeDirectory /var/empty
  sudo dscl . -create "/Users/$runtime_user" AuthenticationAuthority ';DisabledUser;'
done
# Register the protected OS shim; the selected Xcode path need not satisfy
# the broker's strict root ownership policy. Record both executable identities.
producer_git="/usr/bin/git"
selected_git="$(/usr/bin/xcrun --find git)"
python3 - "$lab_root" "$lab_uid" "$producer_git" > "$build_dir/native-config.json" <<'PYCONFIG'
import json,sys
print(json.dumps(dict(Root=sys.argv[1],Producer=sys.argv[1]+"/producer",Service=sys.argv[1]+"/broker-service",Census=sys.argv[1]+"/runtime-census",Git=sys.argv[3],UID=int(sys.argv[2]),GID=20,RuntimeIdentities=[dict(UID=600+i,GID=600+i) for i in range(48)])))
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
sudo -u "$lab_user" sudo -n true && exit 4
set +e
sudo env MULTICA_NATIVE_BROKER_CONFIG="$lab_root/config.json" "$lab_root/broker-test" -test.run '^TestBrokerNative' -test.v -test.timeout 25m > "$build_dir/native.log" 2>&1
result=$?
set -e
cat "$build_dir/native.log"
exit "$result"
