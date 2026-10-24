"""One native Hermes send for one trusted Hub outbox unit; no model or message loop."""

import argparse
from contextlib import contextmanager, ExitStack, redirect_stdout
import hashlib
import json
import os
from pathlib import Path
import stat
import sys
import tempfile


@contextmanager
def private_artifact(artifact, files_root):
    root = Path(files_root).resolve(strict=True)
    target = Path(artifact["local_path"])
    if target.is_symlink() or not target.resolve(strict=True).is_relative_to(root):
        raise ValueError("Artifact copy is outside its configured private directory")
    if os.name == "posix" and (root.stat().st_mode & 0o077 or root.stat().st_uid != os.geteuid()):
        raise PermissionError("Artifact directory must be owned by this profile and private")
    descriptor = os.open(target, os.O_RDONLY | getattr(os, "O_NOFOLLOW", 0))
    with os.fdopen(descriptor, "rb") as source:
        metadata = os.fstat(source.fileno())
        if not stat.S_ISREG(metadata.st_mode):
            raise ValueError("Artifact copy is not a regular file")
        if os.name == "posix" and (metadata.st_mode & 0o077 or metadata.st_uid != os.geteuid()):
            raise PermissionError("Artifact copy must be owned by this profile and private")
        if artifact["size"] < 0 or artifact["size"] > 8 * 1024 * 1024:
            raise ValueError("Artifact copy exceeds the authorized file limit")
        data = source.read(artifact["size"] + 1)
    if len(data) != artifact["size"] or hashlib.sha256(data).hexdigest() != artifact["sha256"].lower():
        raise ValueError("Artifact copy differs from its acquired size or SHA-256")
    # Native senders reopen a MEDIA path later. Send a private snapshot of the
    # bytes verified above, so replacing the original path cannot change them.
    with tempfile.TemporaryDirectory(prefix=".send-", dir=root) as directory:
        snapshot = Path(directory) / target.name
        descriptor = os.open(snapshot, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
        with os.fdopen(descriptor, "wb") as output:
            output.write(data)
            output.flush()
            os.fsync(output.fileno())
        snapshot.chmod(0o400)
        try:
            with snapshot.open("rb") as fixed:
                identity = os.fstat(fixed.fileno())

                def verify_snapshot():
                    current = snapshot.lstat()
                    if not stat.S_ISREG(current.st_mode) or (current.st_dev, current.st_ino) != (identity.st_dev, identity.st_ino):
                        raise ValueError("Validated artifact snapshot path was replaced")
                    fixed.seek(0)
                    content = fixed.read(artifact["size"] + 1)
                    if len(content) != artifact["size"] or hashlib.sha256(content).hexdigest() != artifact["sha256"].lower():
                        raise ValueError("Validated artifact snapshot bytes changed")

                verify_snapshot()
                yield snapshot
                verify_snapshot()
        finally:
            snapshot.chmod(0o600)


def deliver(delivery, hermes_source, files_root):
    try:
        with ExitStack() as snapshots:
            return _deliver(delivery, hermes_source, files_root, snapshots)
    except Exception as error:
        return {"status": "unknown", "error": "Artifact snapshot could not be retained or cleaned; delivery outcome is unknown: " + str(error)}


def _deliver(delivery, hermes_source, files_root, snapshots):
    try:
        sys.path.insert(0, str(Path(hermes_source).resolve(strict=True)))
        from tools.send_message_tool import prepare_send_message_platforms, send_message_tool
        from tools.send_message_targets import resolve_send_target
        from gateway.platforms.base import BasePlatformAdapter

        target = delivery["target"]
        platform, chat = target["platform"], target["chat_id"]
        thread = target.get("thread_id") or None
        if not platform or platform != platform.strip().lower() or not chat or chat != chat.strip():
            raise ValueError("Trusted delivery target must contain an exact platform and chat")
        reference = chat + (":" + thread if thread else "")
        prepare_send_message_platforms()
        resolved_chat, resolved_thread, error = resolve_send_target(platform, reference)
        if error or resolved_chat != chat or (resolved_thread or None) != thread:
            raise ValueError("Native target resolution did not match the original chat and thread")
        artifact = delivery.get("artifact")
        if delivery["kind"] == "artifact":
            path = snapshots.enter_context(private_artifact(artifact, files_root))
            message = "MEDIA:" + str(path)
            media, cleaned = BasePlatformAdapter.extract_media(message)
            if len(media) != 1 or media[0][0] != str(path) or cleaned.strip():
                raise ValueError("Native media parser did not preserve one file and empty text")
        elif delivery["kind"] == "text":
            message = delivery["text"]
            media, cleaned = BasePlatformAdapter.extract_media(message)
            if media or not message.strip():
                raise ValueError("Text delivery must not contain media directives or empty text")
        else:
            raise ValueError("Unknown delivery kind")
    except Exception as error:
        return {"status": "failed", "definitely_not_sent": True, "error": str(error)}
    try:
        raw = send_message_tool({"action": "send", "target": platform + ":" + reference, "message": message})
        receipt = json.loads(raw) if isinstance(raw, str) else raw
    except Exception as error:
        return {"status": "unknown", "error": "Native sender raised " + type(error).__name__ + "; send outcome is unknown"}
    good = (
        isinstance(receipt, dict) and receipt.get("success") is True
        and isinstance(receipt.get("message_id"), str) and receipt["message_id"].strip()
        and not receipt.get("partial_success") and not receipt.get("media_dropped") and not receipt.get("warnings")
        and receipt.get("platform") == platform and receipt.get("chat_id") == chat
    )
    if not good:
        return {"status": "unknown", "receipt": receipt, "error": "Native receipt does not prove complete delivery"}
    result = {"status": "delivered", "receipt": receipt}
    if delivery["kind"] == "artifact":
        if platform != "weixin" or ("media_delivered" in receipt and receipt["media_delivered"] is not True):
            return {"status": "unknown", "receipt": receipt, "error": "Native receipt does not prove this platform delivered the file"}
        result["verification"] = {
            "media_only": True, "artifact_id": artifact["id"], "sha256": artifact["sha256"],
            "size": artifact["size"], "target_exact": True,
        }
    return result


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--hermes-source", required=True, type=Path)
    parser.add_argument("--files-root", required=True, type=Path)
    options = parser.parse_args()
    delivery = json.load(sys.stdin)
    with redirect_stdout(sys.stderr):
        result = deliver(delivery, options.hermes_source, options.files_root)
    print(json.dumps(result, ensure_ascii=False, allow_nan=False))


if __name__ == "__main__":
    main()
