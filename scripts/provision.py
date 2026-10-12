#!/usr/bin/env python3
"""Provision the optional AWS host through Pulumi, then bootstrap saild."""
import ipaddress
import json
import os
from pathlib import Path
import subprocess
import urllib.request

ROOT = Path(__file__).resolve().parents[1]


def run(*args, capture=False, cwd=ROOT / "infra"):
    return subprocess.run(args, cwd=cwd, check=True, text=True,
                          stdout=subprocess.PIPE if capture else None).stdout


def main():
    os.environ.setdefault("AWS_PROFILE", "sail")
    os.environ.setdefault("AWS_REGION", "us-west-2")
    stack = os.environ.get("SAILD_STACK", "dev")
    key = ROOT / "artifacts" / "ssh_key"
    key.parent.mkdir(exist_ok=True)
    if not key.exists():
        run("ssh-keygen", "-t", "ed25519", "-N", "", "-f", str(key))
    stacks = json.loads(run("pulumi", "stack", "ls", "--json", capture=True))
    names = [s["name"].split("/")[-1] for s in stacks]
    run("pulumi", "stack", "select" if stack.split("/")[-1] in names else "init", stack)
    cidr = os.environ.get("SAILD_SSH_CIDR")
    if not cidr:
        with urllib.request.urlopen("https://checkip.amazonaws.com", timeout=10) as r:
            cidr = str(ipaddress.ip_address(r.read().decode().strip())) + "/32"
    run("pulumi", "config", "set", "aws:region", os.environ["AWS_REGION"])
    run("pulumi", "config", "set", "publicKey", key.with_suffix(".pub").read_text().strip())
    run("pulumi", "config", "set", "sshCidr", cidr)
    run("pulumi", "up", "--yes")
    outputs = json.loads(run("pulumi", "stack", "output", "--json", capture=True))
    (ROOT / "artifacts" / "host.json").write_text(json.dumps(outputs, indent=2) + "\n")
    print(f"Host: ubuntu@{outputs['host']}; auto-terminates in 24 hours.")
    run("python3", "scripts/deploy.py", "bootstrap", cwd=ROOT)


if __name__ == "__main__":
    main()
