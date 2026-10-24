"""Root-side bootstrap payload. Invoked only by prepare_access.py's commands."""

import base64
from datetime import datetime, timezone
import json
import os
from pathlib import Path
import pwd
import re
import secrets
import shutil
import subprocess
import sys
import tempfile


def run(*args, **kwargs):
    try:
        return subprocess.run(args, check=True, text=True, capture_output=True, **kwargs)
    except subprocess.CalledProcessError as error:
        if error.stderr:
            print(error.stderr, file=sys.stderr, end="")
        raise


def main():
    action, identifier, public_blob, expires = sys.argv[1:]
    if os.geteuid() != 0:
        raise RuntimeError("Run this payload through sudo")
    if action not in {"install", "revoke"} or not re.fullmatch(r"[a-f0-9]{12}", identifier):
        raise ValueError("Invalid action or deployment identifier")
    raw_key = base64.b64decode(public_blob, validate=True)
    if len(raw_key) != 51 or raw_key[:19] != b"\0\0\0\x0bssh-ed25519\0\0\0\x20":
        raise ValueError("Expected an Ed25519 public key")
    deadline = datetime.strptime(expires, "%Y%m%d%H%M%SZ").replace(tzinfo=timezone.utc)
    user = "dl-deploy-" + identifier
    home = Path("/var/lib") / user
    rule = Path("/etc/sudoers.d") / user
    rule_text = f"{user} ALL=(root) NOPASSWD: ALL\n"
    key_text = f'restrict,expiry-time="{expires}" ssh-ed25519 {public_blob} dots-lite-deploy-{identifier}\n'
    marker = {"id": identifier, "public_key": public_blob, "expires": expires}
    for executable in ("useradd", "userdel", "usermod", "visudo", "sudo", "runuser", "openssl", "nologin"):
        if not shutil.which(executable):
            raise RuntimeError(f"Missing prerequisite: {executable}")
    run("visudo", "-c")
    if not rule.parent.is_dir() or rule.parent.is_symlink():
        raise RuntimeError("Expected a real /etc/sudoers.d directory")

    if action == "revoke":
        if home.is_symlink() or (home / "deployment.json").is_symlink():
            raise RuntimeError("Unexpected deployment marker path")
        if json.loads((home / "deployment.json").read_text()) != marker:
            raise RuntimeError("Deployment marker mismatch; refusing unrelated cleanup")
        account = pwd.getpwnam(user)
        if account.pw_dir != str(home) or account.pw_uid == 0:
            raise RuntimeError("Deployment account changed")
        if rule.is_symlink() or (rule.exists() and rule.read_text() != rule_text):
            raise RuntimeError("Deployment sudo rule changed; inspect before cleanup")
        keys = home / ".ssh/authorized_keys"
        if keys.is_symlink() or (keys.exists() and keys.read_text() != key_text):
            raise RuntimeError("Deployment key changed; inspect before cleanup")
        # Revoke privileges before removing the local identity. Keep a disabled
        # account as an audit record; userdel refuses an account with this SSH open.
        rule.unlink(missing_ok=True)
        keys.unlink(missing_ok=True)
        run("visudo", "-c")
        policy = subprocess.run(["sudo", "-n", "-l", "-U", user], capture_output=True, text=True,
                                env={**os.environ, "LC_ALL": "C"})
        if (policy.returncode not in (0, 1) or
                f"User {user} is not allowed to run sudo on " not in policy.stdout + policy.stderr):
            raise RuntimeError("Could not verify sudo policy removal: " + policy.stdout + policy.stderr)
        run("usermod", "--lock", "--expiredate", "1", "--shell", shutil.which("nologin"), user)
        print(json.dumps({"revoked": True, "account_disabled": True}))
        return

    remaining = (deadline - datetime.now(timezone.utc)).total_seconds()
    if not 0 < remaining <= 24 * 60 * 60:
        raise ValueError("Key expiry must be in the next 24 hours")
    try:
        pwd.getpwnam(user)
    except KeyError:
        pass
    else:
        raise RuntimeError("Deployment account already exists")
    if home.exists() or home.is_symlink() or rule.exists() or rule.is_symlink():
        raise RuntimeError("Deployment paths already exist")
    # An unknown random password hash avoids locked-account SSH differences.
    # The password is neither retained nor disclosed. This is not an empty password.
    password_hash = run("openssl", "passwd", "-6", "-stdin", input=secrets.token_hex(48)).stdout.strip()
    created_user = False
    staged = None
    try:
        run("useradd", "--system", "--user-group", "--no-create-home", "--home-dir", str(home),
            "--shell", "/bin/sh", "--password", password_hash, user)
        created_user = True
        home.mkdir(mode=0o755)
        home.chmod(0o755)
        (home / ".ssh").mkdir(mode=0o755)
        (home / ".ssh").chmod(0o755)
        (home / "deployment.json").write_text(json.dumps(marker) + "\n")
        (home / "deployment.json").chmod(0o600)
        with tempfile.NamedTemporaryFile(mode="w", dir=rule.parent, prefix=".dots-lite-", delete=False) as stream:
            staged = Path(stream.name)
            stream.write(rule_text)
        staged.chmod(0o440)
        run("visudo", "-cf", str(staged))
        staged.replace(rule)
        staged = None
        run("visudo", "-c")
        run("runuser", "-u", user, "--", "sudo", "-n", "true")
        # Publish authentication only after sudo policy and ownership are checked.
        (home / ".ssh/authorized_keys").write_text(key_text)
        (home / ".ssh/authorized_keys").chmod(0o644)
    except BaseException:
        if staged is not None:
            staged.unlink(missing_ok=True)
        rule.unlink(missing_ok=True)
        if created_user:
            for relative in (".ssh/authorized_keys", "deployment.json"):
                (home / relative).unlink(missing_ok=True)
            if (home / ".ssh").exists():
                (home / ".ssh").rmdir()
            if home.exists():
                home.rmdir()
            run("userdel", user)
        raise
    print(json.dumps({"created": True, "account": user, "expires_utc": expires}))


if __name__ == "__main__":
    main()
