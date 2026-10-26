"""Build the pinned test producer and Multica overlay in disposable copies."""
import argparse
import hashlib
import json
import os
from pathlib import Path
import shutil
import subprocess
import tarfile
import urllib.request

parser = argparse.ArgumentParser()
parser.add_argument("--output", type=Path, required=True)
args = parser.parse_args()
repo = Path(__file__).resolve().parents[2]
lab = Path(__file__).resolve().parent
out = args.output.resolve()
out.mkdir(parents=True, exist_ok=False)
source = json.loads((lab / "source.json").read_text())
archive = out / "codex.tar.gz"
url = "https://codeload.github.com/openai/codex/tar.gz/" + source["commit"]
urllib.request.urlretrieve(url, archive)
assert hashlib.sha256(archive.read_bytes()).hexdigest() == source["archive_sha256"]
with tarfile.open(archive) as tar:
    tar.extractall(out, filter="data")
codex = out / ("codex-" + source["commit"])
# Checkout may use CRLF while both source archives contain LF.
def patch_bytes(name):
    return (lab / name).read_bytes().replace(b"\r\n", b"\n")

subprocess.run(["git", "-c", "core.autocrlf=false", "apply", "-"], input=patch_bytes("codex.patch"), cwd=codex, check=True)
env = os.environ.copy()
env["CARGO_PROFILE_DEV_DEBUG"] = "0"
env["CARGO_PROFILE_TEST_DEBUG"] = "0"
with (out / "producer-build.jsonl").open("w") as log:
    subprocess.run(["cargo", "test", "-p", "codex-core-plugins", "--lib", "--no-run", "--locked", "--message-format=json"], cwd=codex / "codex-rs", env=env, stdout=log, check=True)
producer = None
for line in (out / "producer-build.jsonl").read_text().splitlines():
    item = json.loads(line)
    if item.get("reason") == "compiler-artifact" and item.get("target", {}).get("name") == "codex_core_plugins" and item.get("executable"):
        producer = Path(item["executable"])
assert producer is not None
suffix = ".exe" if os.name == "nt" else ""
shutil.copy2(producer, out / ("producer" + suffix))
multica = out / "multica"
multica.mkdir()
with (out / "multica.tar").open("wb") as archive_out:
    subprocess.run(["git", "archive", "HEAD"], cwd=repo, stdout=archive_out, check=True)
with tarfile.open(out / "multica.tar") as tar:
    tar.extractall(multica, filter="data")
subprocess.run(["git", "-c", "core.autocrlf=false", "apply", "-"], input=patch_bytes("multica-test-only.patch"), cwd=multica, check=True)
for test in lab.glob("*_test.go"):
    shutil.copy2(test, multica / "server/internal/daemon" / test.name)
subprocess.run(["go", "test", "-c", "-tags=cachebrokerlab", "./internal/daemon", "-o", str(out / ("broker-test" + suffix))], cwd=multica / "server", check=True)
(out / "build-provenance.json").write_text(json.dumps({
    **source,
    "patch_sha256": hashlib.sha256(patch_bytes("codex.patch")).hexdigest(),
    "producer_sha256": hashlib.sha256(producer.read_bytes()).hexdigest(),
    "multica_head": subprocess.check_output(["git", "rev-parse", "HEAD"], cwd=repo, text=True).strip(),
}, indent=2))
