#!/usr/bin/env python3
"""Build locally and deploy over SSH to any configured Linux/KVM host."""
import json
import os
from pathlib import Path
import shlex
import subprocess
import sys
import time
import uuid

ROOT = Path(__file__).resolve().parents[1]


def run(args, **kwargs):
    return subprocess.run(args, cwd=ROOT, check=True, **kwargs)


def main():
    mode = sys.argv[1] if len(sys.argv) > 1 else "redeploy"
    config_path = ROOT / "artifacts" / "host.json"
    config = json.loads(config_path.read_text()) if config_path.exists() else {}
    host = os.environ.get("SAILD_HOST", config.get("host"))
    if not host:
        raise SystemExit("Set SAILD_HOST or run just provision first")
    user = os.environ.get("SAILD_USER", config.get("sshUser", "ubuntu"))
    key = os.environ.get("SAILD_SSH_KEY", str(ROOT / "artifacts" / "ssh_key"))
    destination = f"{user}@{host}"
    if destination.startswith("-") or any(c.isspace() for c in destination):
        raise SystemExit("Invalid SSH destination")
    opts = ["-i", key, "-o", "StrictHostKeyChecking=accept-new", "-o", "ConnectTimeout=10"]
    ssh = ["ssh", *opts, destination]
    if mode == "tunnel":
        run(["ssh", *opts, "-o", "ExitOnForwardFailure=yes", "-N", "-L",
             "127.0.0.1:50051:127.0.0.1:50051", destination])
        return
    if mode == "ssh":
        run(["ssh", *opts, "-t", destination])
        return
    if mode not in ("bootstrap", "redeploy"):
        raise SystemExit("usage: deploy.py bootstrap|redeploy|tunnel|ssh")
    (ROOT / "bin").mkdir(exist_ok=True)
    env = dict(os.environ, GOOS="linux", GOARCH="amd64", CGO_ENABLED="0")
    for name in ("saild", "guest-agent", "sail"):
        run(["go", "build", "-trimpath", "-o", f"bin/{name}-linux", f"./cmd/{name}"], env=env)
    run(["go", "build", "-trimpath", "-o", "bin/sail", "./cmd/sail"])
    if mode == "bootstrap":
        deadline = time.monotonic() + 600
        while True:
            ready = subprocess.run([*ssh, "true"], stdout=subprocess.DEVNULL,
                                   stderr=subprocess.DEVNULL)
            if ready.returncode == 0:
                break
            if time.monotonic() >= deadline:
                raise SystemExit("Host did not become reachable over SSH within 10 minutes")
            time.sleep(3)
        run([*ssh, "if command -v cloud-init >/dev/null; then sudo cloud-init status --wait; fi"])
    stage = "/tmp/saild-lite-upload-" + uuid.uuid4().hex
    run([*ssh, shlex.join(["mkdir", "-m", "700", stage])])
    try:
        files = ["bin/saild-linux", "bin/guest-agent-linux", "bin/sail-linux",
                 "guest/Dockerfile", "guest/init", "deploy/saild-lite.service",
                 "scripts/remote-deploy.sh", "deploy/artifacts.env"]
        run(["scp", *opts, *files, f"{destination}:{stage}/"])
        run([*ssh, shlex.join(["sudo", "bash", f"{stage}/remote-deploy.sh", mode, stage])])
    finally:
        run([*ssh, shlex.join(["sudo", "rm", "-rf", "--", stage])])


if __name__ == "__main__":
    main()
