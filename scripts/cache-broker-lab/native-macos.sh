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
sudo cp -X "$build_dir/producer" "$build_dir/broker-test" "$lab_root/"
sudo chown -R root:wheel "$lab_root"
sudo chmod 0755 "$lab_root/producer" "$lab_root/broker-test"
python3 - "$lab_root" "$lab_uid" > "$build_dir/native-config.json" <<'PYCONFIG'
import json,sys
print(json.dumps(dict(Root=sys.argv[1],Producer=sys.argv[1]+"/producer",Git="/usr/bin/git",UID=int(sys.argv[2]),GID=20)))
PYCONFIG
sudo cp "$build_dir/native-config.json" "$lab_root/config.json"
sudo chown root:wheel "$lab_root/config.json"
sudo chmod 0600 "$lab_root/config.json"
id "$lab_user" > "$build_dir/native-identity.log"
sudo -u "$lab_user" sudo -n true && exit 4
set +e
sudo env MULTICA_NATIVE_BROKER_CONFIG="$lab_root/config.json" "$lab_root/broker-test" -test.run '^TestBrokerNative' -test.v -test.timeout 15m > "$build_dir/native.log" 2>&1
result=$?
set -e
cat "$build_dir/native.log"
exit "$result"
