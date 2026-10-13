"""SSH orchestration and private artifacts for bounded foreground captures."""
import hashlib
import json
import math
import os
from pathlib import Path
import re
import secrets
import shlex
import stat
import subprocess
import time


def ssh_command(host, via, tokens):
    """Nest SSH hops so each hop can use its own configured key, quoting every remote argument."""
    for target in [host, *via]:
        if target.startswith("-") or not re.fullmatch(r"[A-Za-z0-9_@.\[\]:-]{1,255}", target):
            raise ValueError("invalid_ssh_target")
    options = ["ssh", "-T", "-o", "BatchMode=yes", "-o", "ConnectTimeout=12",
               "-o", "ServerAliveInterval=15", "-o", "ServerAliveCountMax=2"]
    command = options + [host, shlex.join(tokens)]
    for hop in reversed(via):
        command = options + [hop, shlex.join(command)]
    return command


def private_directory(path):
    """Require an owned 0700 artifact directory outside every Git worktree."""
    path = Path(path).expanduser().resolve()
    if any((parent / ".git").exists() for parent in [path, *path.parents]):
        raise ValueError("artifact_directory_must_be_outside_git")
    path.mkdir(parents=True, mode=0o700, exist_ok=True)
    info = path.stat()
    if info.st_uid != os.getuid() or stat.S_IMODE(info.st_mode) & 0o077:
        raise ValueError("artifact_directory_must_be_owned_and_private_0700")
    return path


def private_json(path, value):
    """Create a 0600 JSON artifact without overwriting an existing run."""
    descriptor = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
    with os.fdopen(descriptor, "w", encoding="utf-8") as output:
        json.dump(value, output, ensure_ascii=False, indent=2)
        output.write("\n")


def capture_event(value):
    """Accept only finite capture statistics, never arbitrary JSON from SSH stderr."""
    if not isinstance(value, dict):
        return None
    kind = "captureReady" if value.get("captureReady") is True else "captureFinished"
    if value.get(kind) is not True:
        return None
    result = {kind: True}
    for key in ["startedUtcEpoch", "endedUtcEpoch", "durationSeconds", "maxBytes", "gatewayPort",
                "embyPort", "tlsPort", "snaplen", "packets", "capturedBytes", "socketPackets", "socketDroppedPackets"]:
        number = value.get(key)
        if type(number) in (int, float) and math.isfinite(number) and number >= 0:
            result[key] = number
    if value.get("stopReason") in {"duration_limit", "byte_limit", "signal"}:
        result["stopReason"] = value["stopReason"]
    return result


def remote_python_tokens(filename, options, sudo=False, pid=None):
    """Load reviewed standard-library code locally; leave no helper file on the remote host."""
    source = Path(__file__).with_name(filename).read_text(encoding="utf-8")
    tokens = ["sudo", "-n"] if sudo else []
    if pid is not None:
        tokens += ["nsenter", "-t", str(pid), "-n"]
    return tokens + ["python3", "-u", "-c", source, json.dumps(options)], source


def container_pid(host, via, container, sudo):
    """Inspect only the existing container's running state and PID, never its environment."""
    if container.startswith("-") or not re.fullmatch(r"[A-Za-z0-9_.-]{1,128}", container):
        raise ValueError("invalid_container_name")
    tokens = (["sudo", "-n"] if sudo else []) + ["docker", "inspect", "--format",
                                                     "{{.State.Pid}} {{.State.Running}}", container]
    output = subprocess.check_output(ssh_command(host, via, tokens), stdin=subprocess.DEVNULL,
                                     stderr=subprocess.DEVNULL, timeout=30 * (len(via) + 1), text=True)
    match = re.fullmatch(r"([1-9][0-9]*) true\s*", output)
    if not match:
        raise ValueError("gateway_container_not_running")
    return int(match.group(1))


def run_capture(args):
    """Stream a finite remote capture into a local PCAP; record sanitized completion metadata."""
    directory = private_directory(args.output)
    pid = container_pid(args.host, args.via, args.container, args.sudo)
    options = {"durationSeconds": args.duration, "maxBytes": args.max_mib * 1024 * 1024,
               "interface": args.interface, "gatewayPort": args.gateway_port,
               "embyPort": args.emby_port, "tlsPort": args.tls_port, "snaplen": args.snaplen}
    tokens, source = remote_python_tokens("remote_capture.py", options, args.sudo, pid)
    stem = f"gateway-{int(time.time())}-{secrets.token_hex(3)}"
    path = directory / (stem + ".pcap")
    metadata = {"formatVersion": 1, "pcapFile": path.name, "host": args.host, "via": args.via,
                "container": args.container, "gatewayPid": pid, "options": options,
                "remoteSourceSHA256": hashlib.sha256(source.encode()).hexdigest(), "events": []}
    descriptor = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
    returncode, interrupted = 1, False
    try:
        with os.fdopen(descriptor, "wb") as target:
            process = subprocess.Popen(ssh_command(args.host, args.via, tokens), stdin=subprocess.DEVNULL,
                                       stdout=target, stderr=subprocess.PIPE, text=True)
            try:
                for line in process.stderr:
                    try:
                        event = capture_event(json.loads(line))
                    except ValueError:
                        continue  # SSH banners and tracebacks are not capture evidence.
                    if event:
                        metadata["events"].append(event)
                        print(json.dumps(event), flush=True)
                returncode = process.wait()
            except KeyboardInterrupt:
                interrupted = True
                process.terminate()
                try:
                    process.wait(timeout=5)
                except subprocess.TimeoutExpired:
                    process.kill()
                    process.wait()
                returncode = 130
            finally:
                process.stderr.close()
    finally:
        metadata.update(sshExitCode=returncode, interrupted=interrupted, pcapBytes=path.stat().st_size)
        metadata_path = path.with_suffix(".metadata.json")
        private_json(metadata_path, metadata)
        print(json.dumps({"pcapPath": str(path), "metadataPath": str(metadata_path),
                          "sshExitCode": returncode}), flush=True)
    if returncode:
        raise ValueError("capture_interrupted" if interrupted else "remote_capture_failed_check_ssh_and_permissions")
    if not all(any(event.get(kind) for event in metadata["events"]) for kind in ["captureReady", "captureFinished"]):
        raise ValueError("capture_completion_not_observed")
    if metadata["pcapBytes"] < 24:
        raise ValueError("empty_capture_artifact")


def fetch_logs(args):
    """Fetch only selected, source-filtered Gateway logs over the configured SSH chain."""
    directory = private_directory(args.output)
    options = {"container": args.container, "since": args.since, "until": args.until,
               "tail": args.tail, "sudo": args.sudo}
    tokens, _ = remote_python_tokens("logs.py", options)
    output = subprocess.check_output(ssh_command(args.host, args.via, tokens), stdin=subprocess.DEVNULL,
                                     stderr=subprocess.DEVNULL, timeout=60 * (len(args.via) + 1), text=True)
    rows = json.loads(output)
    path = directory / f"gateway-logs-{int(time.time())}-{secrets.token_hex(3)}.json"
    private_json(path, rows)
    print(json.dumps({"logsPath": str(path), "rows": len(rows)}))
