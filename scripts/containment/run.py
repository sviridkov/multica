#!/usr/bin/env python3
"""Bounded offline experiment. Never a production GC/launcher or host fallback."""
import argparse
import concurrent.futures
import datetime
import hashlib
import io
import json
import os
from pathlib import Path
import subprocess
import tarfile
import time
import uuid

from verifier import AdmissionGate, verdict


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--context", required=True)
    parser.add_argument("--probe-binary", type=Path, required=True)
    parser.add_argument("--probe-sha256", required=True)
    parser.add_argument("--output", type=Path, required=True)
    args = parser.parse_args()
    args.output.mkdir(parents=True, exist_ok=False)
    absolute_deadline = time.monotonic() + 20 * 60
    deadline = absolute_deadline - 60
    prefix = "multica-containment-" + uuid.uuid4().hex[:12]
    label = "multica.synthetic-run=" + prefix
    containers = []
    record = {"namespace": prefix, "context": args.context, "containers": containers,
              "budget": {"containers": 2, "cpus": 2, "memory_bytes": 1073741824,
                         "deadline_seconds": 1200}, "volume": None, "image": None}
    evidence = {}
    gate = AdmissionGate()
    journal = (args.output / "commands.jsonl").open("w")

    def save():
        (args.output / "handles.json").write_text(json.dumps(record, indent=2) + "\n")
        (args.output / "result.json").write_text(json.dumps({"evidence": evidence, **verdict(evidence)}, indent=2) + "\n")

    def docker(*command, timeout=30, input_bytes=None, check=True):
        remaining = deadline - time.monotonic()
        if remaining <= 0:
            raise TimeoutError("experiment deadline exhausted")
        result = subprocess.run(["docker", "--context", args.context, *command],
                                input=input_bytes, stdout=subprocess.PIPE, stderr=subprocess.PIPE,
                                timeout=min(timeout, remaining))
        journal.write(json.dumps({"args": command, "exit": result.returncode,
                                  "stdout": result.stdout.decode(errors="replace"),
                                  "stderr": result.stderr.decode(errors="replace")}) + "\n")
        journal.flush()
        if check and result.returncode:
            raise RuntimeError(f"Docker {command[0]} failed: {result.stderr.decode(errors='replace')}")
        return result.stdout.decode().strip()

    def events(container):
        raw = docker("logs", container)
        parsed = []
        for line in raw.splitlines():
            try:
                parsed.append(json.loads(line))
            except ValueError:
                pass
        return parsed

    def create(role, image, *command):
        if len(containers) >= 2:
            raise RuntimeError("two-container limit")
        flags = ["create", "--name", prefix + "-" + role, "--label", label,
                 "--network=none", "--cpus=1", "--memory=512m", "--memory-swap=512m",
                 "--pids-limit=128", "--cap-drop=ALL", "--security-opt=no-new-privileges:true",
                 "--read-only", "--restart=no", "--user=0:0"]
        if role == "task":
            flags += ["--cgroupns=private", "--mount", f"type=volume,src={record['volume']},dst=/data"]
        else:
            # Read-only kernel cgroup view; no host PID, capabilities or mounts.
            flags += ["--cgroupns=host"]
        cid = docker(*flags, image, *command)
        containers.append(cid)
        save()
        inspected = json.loads(docker("inspect", cid))[0]
        host = inspected["HostConfig"]
        if (host["NetworkMode"] != "none" or host["Privileged"] or host["PidMode"] == "host"
                or host.get("CapAdd") or host["CapDrop"] != ["ALL"] or host.get("Binds")
                or host["NanoCpus"] != 1000000000 or host["Memory"] != 536870912
                or not host["ReadonlyRootfs"] or "no-new-privileges:true" not in host["SecurityOpt"]):
            raise RuntimeError("container boundary verification failed")
        (args.output / f"{role}-inspect.json").write_text(json.dumps(inspected, indent=2) + "\n")
        docker("start", cid)
        return cid

    try:
        context = json.loads(docker("context", "inspect", args.context))[0]
        if not context["Endpoints"]["docker"]["Host"].startswith("unix://"):
            raise RuntimeError("only the existing local engine is allowed")
        (args.output / "context.json").write_text(json.dumps(context, indent=2) + "\n")
        info = json.loads(docker("info", "--format", "{{json .}}"))
        if info["Architecture"] not in ("aarch64", "arm64") or info["CgroupVersion"] != "2":
            raise RuntimeError("expected local arm64/cgroup-v2 engine")
        (args.output / "engine.json").write_text(json.dumps(info, indent=2) + "\n")
        probe = args.probe_binary.read_bytes()
        if hashlib.sha256(probe).hexdigest() != args.probe_sha256:
            raise RuntimeError("probe digest mismatch")
        # Import a one-file rootfs. No registry, unverified base image, build daemon,
        # network, shell or package manager is needed in the container.
        archive = io.BytesIO()
        with tarfile.open(fileobj=archive, mode="w") as tar:
            entry = tarfile.TarInfo("probe")
            entry.size, entry.mode, entry.mtime = len(probe), 0o755, 0
            tar.addfile(entry, io.BytesIO(probe))
        rootfs = archive.getvalue()
        record["rootfs_sha256"] = hashlib.sha256(rootfs).hexdigest()
        record["probe_sha256"] = args.probe_sha256
        image = docker("import", "--platform=linux/arm64", "--change", 'ENTRYPOINT ["/probe"]',
                       "--change", "LABEL " + label, "-", prefix, input_bytes=rootfs)
        record["image"] = image
        save()
        image_info = json.loads(docker("image", "inspect", image))[0]
        if image_info["Architecture"] != "arm64" or image_info["Id"] != image:
            raise RuntimeError("imported image identity mismatch")
        (args.output / "image.json").write_text(json.dumps(image_info, indent=2) + "\n")
        record["volume"] = docker("volume", "create", "--label", label, prefix + "-data")
        save()
        task = create("task", image, "task")
        observer = create("observer", image, "observe", task)
        observed = []
        until = time.monotonic() + 8
        while time.monotonic() < until:
            observed = events(observer)
            if any(e["event"] in ("kernel_populated", "error") for e in observed):
                break
            time.sleep(.05)
        evidence["kernel_populated"] = any(e["event"] == "kernel_populated" for e in observed)
        # Race acknowledged launches against gate closure. Start one known late
        # writer so graceful-stop time cannot masquerade as writer quiescence.
        with concurrent.futures.ThreadPoolExecutor(max_workers=4) as pool:
            attempts = [pool.submit(gate.admit, lambda i=i: docker(
                "exec", "-d", task, "/probe", "writer", f"race-{i}", "0")) for i in range(8)]
            gate.close()
            record["admission_closed_at"] = datetime.datetime.now(datetime.timezone.utc).isoformat()
            admitted = [f.result() for f in attempts]
        denied = [gate.admit(lambda: (_ for _ in ()).throw(RuntimeError("post-close launch"))) for _ in range(8)]
        record["admission_race"] = {"accepted_before_close": sum(admitted),
                                    "denied_after_close": denied.count(False)}
        evidence["admission_closed"] = gate.closed and not any(denied)
        evidence["admissions_drained"] = True
        docker("stop", "--time", "2", task, timeout=15)
        evidence["stop_succeeded"] = True
        docker("wait", task, timeout=10)
        state = json.loads(docker("inspect", "--format", "{{json .State}}", task))
        evidence["container_exited"] = state["Running"] is False
        # Collect observer synchronously, with no long-running service handoff.
        docker("wait", observer, timeout=50)
        observed = events(observer)
        writer_events = events(task)
        (args.output / "observer-events.json").write_text(json.dumps(observed, indent=2) + "\n")
        (args.output / "writer-events.json").write_text(json.dumps(writer_events, indent=2) + "\n")
        evidence["kernel_empty"] = any(e["event"] == "kernel_empty" and e["target"] == task for e in observed)
        observer_state = json.loads(docker("inspect", "--format", "{{json .State}}", observer))
        evidence["observer_succeeded"] = observer_state["ExitCode"] == 0 and not observer_state["Running"]
        evidence["no_observation_error"] = not any(e["event"] == "error" for e in observed)
        writers = {e["name"]: e for e in writer_events if e["event"] == "writer"}
        record["cases"] = {
            "direct": "direct" in writers,
            "setsid": "setsid" in writers and writers["setsid"]["sid"] == writers["setsid"]["pid"],
            "double_fork_reparent": writers.get("double-fork", {}).get("ppid") == 1,
            # The late writer waits for a file created only by PID1's TERM handler.
            # The controller sends TERM only after close/drain; this causal
            # ordering does not depend on host/VM wall clocks or ISO parsers.
            "late_after_admission_closed": "late-stop" in writers and any(e["event"] == "stop_signal" for e in writer_events),
            "admission_stop_race": not any(denied),
        }
        evidence["operations_succeeded"] = True
    except Exception as error:
        evidence["operations_succeeded"] = False
        record["error"] = str(error)
    finally:
        gate.close()
        record["admission_held_closed_through_cleanup"] = gate.closed
        # Only exact, labelled objects created above; never prune or touch peers.
        # The final minute of the same 20-minute budget is reserved for cleanup.
        deadline = absolute_deadline
        removed = []
        for cid in containers:
            try:
                inspected = json.loads(docker("inspect", cid))[0]
                if inspected["Config"]["Labels"].get("multica.synthetic-run") != prefix:
                    raise RuntimeError("cleanup ownership mismatch")
                if inspected["State"]["Running"]:
                    docker("stop", "--time", "1", cid, timeout=10)
                docker("wait", cid, timeout=10)
                inspected = json.loads(docker("inspect", cid))[0]
                if inspected["State"]["Running"]:
                    raise RuntimeError("own container did not stop")
                docker("rm", cid)
                removed.append(cid)
            except Exception as error:
                record.setdefault("cleanup_errors", []).append(str(error))
        record["removed_containers"] = removed
        # No data cleanup without the kernel signal, even if Docker says exited.
        if verdict(evidence)["synthetic_cleanup_allowed"] and not record.get("error") and len(removed) == len(containers):
            docker("volume", "rm", record["volume"])
            record["volume_disposition"] = "removed after kernel proof with gate closed"
        else:
            record["volume_disposition"] = "retained; verifier forbids data cleanup"
        save()
        journal.close()
    print(json.dumps({**verdict(evidence), "cases": record.get("cases"), "error": record.get("error"),
                      "handles": str(args.output / "handles.json")}))
    return 1 if record.get("error") or record.get("cleanup_errors") else 0


if __name__ == "__main__":
    raise SystemExit(main())
