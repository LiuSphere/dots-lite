"""Exercise real account/sudo changes inside disposable Linux namespaces.

Run as root: python3 linux_access_smoke.py
Requires unshare, mount and the prerequisites of remote_access.py.
Host accounts, sudoers, /var/lib, /var/log, /run and network are isolated.
"""

from datetime import datetime, timedelta, timezone
import json
import importlib.util
import os
from pathlib import Path
import secrets
import shlex
import shutil
import socket
import stat
import subprocess
import sys
import tempfile
import time
from types import SimpleNamespace


PAYLOAD = Path(__file__).resolve().parents[1] / "remote_access.py"


def run(args, **kwargs):
    return subprocess.run(args, check=True, text=True, capture_output=True, **kwargs)


def ssh_roundtrip(root):
    run(["ip", "link", "set", "lo", "up"])
    Path("/run/sshd").mkdir(exist_ok=True)
    host_key = root / "host-key"
    run(["ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", str(host_key)])
    key_type, blob, *_ = host_key.with_suffix(".pub").read_text().split()
    known_hosts = root / "known_hosts"
    known_hosts.write_text(f"[127.0.0.1]:23222 {key_type} {blob}\n")
    wrapper = root / "ssh-client"
    wrapper.write_text(f"#!/bin/sh\nexec {shlex.quote(shutil.which('ssh'))} -F /dev/null -o UserKnownHostsFile={shlex.quote(str(known_hosts))} \"$@\"\n")
    wrapper.chmod(0o700)
    spec = importlib.util.spec_from_file_location("prepare_access", PAYLOAD.with_name("prepare_access.py"))
    helper = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(helper)
    directory = root / "client"
    helper.prepare(SimpleNamespace(risk_acknowledged=True, host="127.0.0.1", admin="root", port=23222,
                                   output=str(directory), ssh=str(wrapper), keygen="ssh-keygen", admin_identity=None))
    manifest_path = directory / "access.json"
    manifest = json.loads(manifest_path.read_text())
    run([sys.executable, str(PAYLOAD), "install", manifest["identifier"], manifest["public_key"], manifest["expires"]])
    config = root / "sshd_config"
    config.write_text(f"Port 23222\nListenAddress 127.0.0.1\nHostKey {host_key}\nPidFile {root / 'sshd.pid'}\n"
                      "UsePAM no\nPasswordAuthentication no\nKbdInteractiveAuthentication no\n"
                      "AuthorizedKeysFile %h/.ssh/authorized_keys\nStrictModes yes\nAllowUsers dl-deploy-*\n")
    with (root / "sshd.log").open("w+") as log:
        server = subprocess.Popen([shutil.which("sshd"), "-D", "-e", "-f", str(config)], stdout=log, stderr=log)
        try:
            for _ in range(30):
                if server.poll() is not None:
                    log.seek(0)
                    raise RuntimeError(log.read())
                try:
                    with socket.create_connection(("127.0.0.1", 23222), timeout=0.1):
                        break
                except OSError:
                    time.sleep(0.1)
            else:
                raise RuntimeError("Isolated sshd did not start")
            result = run([*helper.ssh_arguments(manifest, deployment=True), "sudo -n id -u"])
            assert result.stdout.strip() == "0"
            # A local/network EACCES is not evidence of authentication refusal.
            failed_transport = root / "ssh-connect-denied"
            failed_transport.write_text("#!/bin/sh\necho 'ssh: connect to host 127.0.0.1 port 23222: Permission denied' >&2\nexit 255\n")
            failed_transport.chmod(0o700)
            manifest_path.write_text(json.dumps({**manifest, "ssh": str(failed_transport), "remote_revoked": True}))
            try:
                helper.revoke(SimpleNamespace(manifest=str(manifest_path)))
            except RuntimeError as error:
                assert "Expected authentication refusal" in str(error)
            else:
                raise AssertionError("Connection failure was treated as authentication denial")
            assert (directory / "identity").exists()
            manifest_path.write_text(json.dumps(manifest))
            helper.revoke(SimpleNamespace(manifest=str(manifest_path)))
            assert not (directory / "identity").exists() and not (directory / "identity.pub").exists()
            assert json.loads(manifest_path.read_text())["revoked"]
            print("PASS: real SSH authentication, root sudo, remote revocation, fresh authentication denial, local key deletion")
        finally:
            server.terminate()
            server.wait(timeout=5)


def isolated(root):
    # Prevent bind mounts from propagating back to the host namespace.
    run(["mount", "--make-rprivate", "/"])
    run(["mount", "--bind", str(root / "etc"), "/etc"])
    for directory in ("/var/lib", "/var/log", "/run"):
        run(["mount", "-t", "tmpfs", "tmpfs", directory])
    (Path("/run") / "sudo").mkdir()
    key = root / "key"
    run(["ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", str(key)])
    public_blob = key.with_suffix(".pub").read_text().split()[1]
    expires = (datetime.now(timezone.utc) + timedelta(hours=1)).strftime("%Y%m%d%H%M%SZ")
    identifier = secrets.token_hex(6)
    user = "dl-deploy-" + identifier

    def invoke(action, *, blob=public_blob, expiry=expires, deployment_id=identifier, succeeds=True):
        result = subprocess.run([sys.executable, str(PAYLOAD), action, deployment_id, blob, expiry],
                                capture_output=True, text=True)
        if (result.returncode == 0) != succeeds:
            raise AssertionError(result.stderr or result.stdout)
        return result

    def sudo_allowed():
        return subprocess.run(["runuser", "-u", user, "--", "sudo", "-n", "true"],
                              capture_output=True).returncode == 0

    invoke("install", blob="bad-key", succeeds=False)
    invoke("install", expiry="20000101000000Z", succeeds=False)
    assert not (Path("/var/lib") / user).exists()
    result = invoke("install")
    assert json.loads(result.stdout)["created"] and sudo_allowed()
    original = (Path("/var/lib") / user / "deployment.json").read_text()
    invoke("install", succeeds=False)
    assert (Path("/var/lib") / user / "deployment.json").read_text() == original
    invoke("revoke", blob=public_blob[:-2] + "AA", succeeds=False)
    assert sudo_allowed()
    result = invoke("revoke")
    assert json.loads(result.stdout)["revoked"] and not sudo_allowed()
    assert not (Path("/etc/sudoers.d") / user).exists()
    assert not (Path("/var/lib") / user / ".ssh/authorized_keys").exists()
    invoke("revoke")  # An ordinary administrator can finish or repeat cleanup.

    partial_id = secrets.token_hex(6)
    invoke("install", deployment_id=partial_id)
    (Path("/etc/sudoers.d") / ("dl-deploy-" + partial_id)).unlink()
    extra_rule = Path("/etc/sudoers.d/access-test-extra")
    extra_rule.write_text(f"dl-deploy-{partial_id} ALL=(root) NOPASSWD: /usr/bin/id\n")
    extra_rule.chmod(0o440)
    invoke("revoke", deployment_id=partial_id, succeeds=False)
    extra_rule.unlink()
    invoke("revoke", deployment_id=partial_id)
    assert not (Path("/var/lib") / ("dl-deploy-" + partial_id) / ".ssh/authorized_keys").exists()

    if shutil.which("sshd"):
        ssh_roundtrip(root)
    elif "--require-ssh" in sys.argv:
        raise RuntimeError("sshd required for complete authentication/deletion test")
    else:
        print("SKIP: real SSH roundtrip requires sshd; account/sudo checks still run")

    # A sudoers installation that ignores the drop-in must fail and roll back.
    sudoers = Path("/etc/sudoers")
    policy = sudoers.read_text()
    sudoers.write_text("\n".join(line for line in policy.splitlines() if "includedir" not in line) + "\n")
    other_id = secrets.token_hex(6)
    invoke("install", deployment_id=other_id, succeeds=False)
    assert not (Path("/etc/sudoers.d") / ("dl-deploy-" + other_id)).exists()
    assert not (Path("/var/lib") / ("dl-deploy-" + other_id)).exists()
    assert subprocess.run(["getent", "passwd", "dl-deploy-" + other_id], capture_output=True).returncode != 0
    print("PASS: invalid/expired input, real sudo grant, collision refusal, marker mismatch, revocation, interrupted cleanup, rollback")


def main():
    if os.geteuid() != 0:
        raise RuntimeError("Run as root; all mutations are confined to disposable namespaces")
    if len(sys.argv) >= 3 and sys.argv[1] == "--isolated":
        isolated(Path(sys.argv[2]))
        return
    with tempfile.TemporaryDirectory(prefix="dots-lite-access-test-") as directory:
        root = Path(directory)
        shutil.copytree("/etc", root / "etc", symlinks=True,
                        ignore=lambda directory, names: [name for name in names
                            if stat.S_ISSOCK((Path(directory) / name).lstat().st_mode)])
        result = subprocess.run(["unshare", "--mount", "--pid", "--fork", "--net", "--mount-proc",
                                 sys.executable, str(Path(__file__).resolve()), "--isolated", str(root),
                                 *(["--require-ssh"] if "--require-ssh" in sys.argv else [])],
                                capture_output=True, text=True)
        if result.returncode:
            raise RuntimeError(result.stderr)
        print(result.stdout.strip())


if __name__ == "__main__":
    main()
