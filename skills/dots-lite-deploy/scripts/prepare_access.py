"""Prepare a Windows user's one-command SSH/sudo bootstrap without granting access."""

import argparse
import base64
from datetime import datetime, timedelta, timezone
import json
import os
from pathlib import Path
import re
import secrets
import shlex
import shutil
import subprocess
import sys


def run(args, **kwargs):
    try:
        return subprocess.run(args, check=True, text=True, **kwargs)
    except subprocess.CalledProcessError as error:
        if error.stderr:
            print(error.stderr, file=sys.stderr, end="")
        raise


def ps_quote(value):
    return "'" + str(value).replace("'", "''") + "'"


def ssh_arguments(manifest, *, deployment=False):
    args = [manifest["ssh"], "-p", str(manifest["port"]), "-o", "StrictHostKeyChecking=yes",
            "-o", "ControlPath=none", "-o", "ControlMaster=no", "-o", "ForwardAgent=no",
            "-o", "ConnectTimeout=15", "-o", "ServerAliveInterval=15", "-o", "ServerAliveCountMax=2"]
    if deployment:
        args += ["-i", manifest["private_key"], "-o", "BatchMode=yes", "-o", "IdentitiesOnly=yes",
                 "-o", "IdentityAgent=none", "-o", "PreferredAuthentications=publickey"]
    else:
        args += ["-t"]
        if manifest["admin_identity"]:
            args += ["-i", manifest["admin_identity"], "-o", "IdentitiesOnly=yes"]
    args += ["-l", manifest["deployment_account"] if deployment else manifest["admin"], "--", manifest["host"]]
    return args


def prepare(args):
    if not args.risk_acknowledged:
        raise ValueError("Explain root-equivalent key access and obtain the user's acknowledgement first")
    if not re.fullmatch(r"[a-zA-Z0-9_][a-zA-Z0-9_.:-]*", args.host):
        raise ValueError("Use a hostname, IP or SSH alias without user/options")
    if not re.fullmatch(r"[a-z_][a-z0-9_-]*", args.admin) or not 1 <= args.port <= 65535:
        raise ValueError("Invalid administrator account or port")
    directory = Path(args.output).resolve()
    repositories = [parent for start in (Path(__file__).resolve().parent, Path.cwd())
                    for parent in (start, *start.parents) if (parent / ".git").exists()]
    if any(directory.is_relative_to(repository) for repository in repositories):
        raise ValueError("Keep deployment identities outside the repository")
    ssh = shutil.which(args.ssh)
    keygen = shutil.which(args.keygen)
    if not ssh or not keygen:
        raise RuntimeError("OpenSSH ssh and ssh-keygen are required")
    directory.mkdir(mode=0o700, parents=True, exist_ok=False)
    if os.name == "nt":
        # Use the actual user SID; the agent subprocess may have filtered env vars.
        sid = run(["whoami", "/user", "/fo", "csv", "/nh"], capture_output=True).stdout
        sid = re.search(r"S-1-[0-9-]+", sid).group()
        run(["icacls", str(directory), "/inheritance:r", "/grant:r", f"*{sid}:(OI)(CI)F"], capture_output=True)
    identifier = secrets.token_hex(6)
    key = directory / "identity"
    run([keygen, "-q", "-t", "ed25519", "-N", "", "-C", "dots-lite-deploy-" + identifier, "-f", str(key)])
    expires = (datetime.now(timezone.utc) + timedelta(hours=24)).strftime("%Y%m%d%H%M%SZ")
    public_blob = key.with_suffix(".pub").read_text().split()[1]
    payload = base64.b64encode(Path(__file__).with_name("remote_access.py").read_bytes()).decode()
    code = f'exec(__import__("base64").b64decode("{payload}"))'
    common = ["python3", "-c", code]
    manifest = {"host": args.host, "port": args.port, "admin": args.admin,
                "admin_identity": str(Path(args.admin_identity).resolve()) if args.admin_identity else None,
                "ssh": ssh, "private_key": str(key), "deployment_account": "dl-deploy-" + identifier,
                "identifier": identifier, "public_key": public_blob, "expires": expires,
                "install_command": shlex.join(["sudo", "--", *common, "install", identifier, public_blob, expires]),
                "revoke_command": shlex.join(["sudo", "-n", "--", *common, "revoke", identifier, public_blob, expires])}
    (directory / "access.json").write_text(json.dumps(manifest, indent=2) + "\n", encoding="utf-8")
    invocation = "& " + " ".join(ps_quote(part) for part in [*ssh_arguments(manifest), manifest["install_command"]])
    (directory / "approve.ps1").write_text(
        "$ErrorActionPreference = 'Stop'\n" + invocation + "\nif ($LASTEXITCODE -ne 0) { throw 'Remote provisioning failed' }\n",
        encoding="utf-8")
    print("Prepared locally; no remote access has been granted.")
    print("After reviewing the prepared payload, the user runs:")
    print("& " + ps_quote(directory / "approve.ps1"))


def revoke(args):
    path = Path(args.manifest).resolve()
    manifest = json.loads(path.read_text(encoding="utf-8"))
    key = Path(manifest["private_key"])
    if key != path.parent / "identity" or key.is_symlink():
        raise ValueError("Unexpected private key path")
    if not manifest.get("remote_revoked"):
        result = run([*ssh_arguments(manifest, deployment=True), manifest["revoke_command"]], capture_output=True)
        if json.loads(result.stdout.strip()).get("revoked") is not True:
            raise RuntimeError("Remote revocation was not confirmed; retain the key for recovery")
        manifest["remote_revoked"] = True
        path.write_text(json.dumps(manifest, indent=2) + "\n", encoding="utf-8")
    probe = subprocess.run([*ssh_arguments(manifest, deployment=True), "true"], capture_output=True, text=True)
    authentication_denied = re.search(
        r"(?:^|\n)" + re.escape(manifest["deployment_account"]) +
        r"@[^\s]+: Permission denied \([^\r\n)]+\)\.\s*$", probe.stderr,
    )
    if probe.returncode != 255 or not authentication_denied:
        raise RuntimeError("Expected authentication refusal; investigate before deleting the key")
    key.unlink(missing_ok=True)
    key.with_suffix(".pub").unlink(missing_ok=True)
    manifest["revoked"] = True
    path.write_text(json.dumps(manifest, indent=2) + "\n", encoding="utf-8")
    print("Remote access revoked; fresh SSH authentication denied; local private/public key files deleted.")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    commands = parser.add_subparsers(dest="command", required=True)
    create = commands.add_parser("prepare")
    create.add_argument("--host", required=True)
    create.add_argument("--port", type=int, default=22)
    create.add_argument("--admin", required=True)
    create.add_argument("--admin-identity")
    create.add_argument("--output", required=True, help="New private directory outside this repository")
    create.add_argument("--ssh", default="ssh")
    create.add_argument("--keygen", default="ssh-keygen")
    create.add_argument("--risk-acknowledged", action="store_true")
    cleanup = commands.add_parser("revoke")
    cleanup.add_argument("--manifest", required=True)
    args = parser.parse_args()
    (prepare if args.command == "prepare" else revoke)(args)


if __name__ == "__main__":
    main()
