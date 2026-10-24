"""Exercise the official send helper with a registered external-sender fixture, never Weixin network."""

import argparse
from dataclasses import replace
import hashlib
import json
import os
from pathlib import Path
import stat
import sys
import tempfile


def smoke(source, report_path=None):
    with tempfile.TemporaryDirectory() as directory:
        home = Path(directory)
        os.environ["HERMES_HOME"] = str(home)
        sys.path.insert(0, str(source.resolve()))
        os.chdir(home)
        (home / "config.yaml").write_text("""platforms:
  weixin:
    enabled: true
    token: fixture-only-not-a-credential
    extra:
      account_id: fixture-account
plugins:
  enabled: [weixin]
""")
        from hermes_cli.plugins import discover_plugins
        from gateway.platform_registry import platform_registry
        discover_plugins()
        entry = platform_registry.get("weixin")
        if entry is None:
            raise RuntimeError("Official Weixin platform was not registered")
        received = []
        receipt_state = {"value": {"success": True, "platform": "weixin", "chat_id": "wxid_fixture_only", "message_id": "fixture-media-1"}}

        async def fixture_sender(pconfig, chat_id, message, *, thread_id=None, media_files=None, **kwargs):
            if receipt_state.get("replace_original"):
                path.write_bytes(b"x" * len(data))
            for media_path, voice in media_files or []:
                snapshot = Path(media_path)
                if snapshot.parent.parent != files or snapshot.name != path.name or snapshot.stat().st_mode & 0o222:
                    raise RuntimeError("Native media is not a read-only profile-private snapshot")
                if os.name == "posix" and stat.S_IMODE(snapshot.parent.stat().st_mode) != 0o700:
                    raise RuntimeError("Native snapshot directory is not private")
            received.append({"chat_id": chat_id, "message": message, "thread_id": thread_id,
                             "media": [(str(path), Path(path).read_bytes().hex()) for path, voice in media_files or []]})
            return dict(receipt_state["value"])

        # Public platform registry replaces ONLY the network sender boundary.
        platform_registry.register(replace(entry, standalone_sender_fn=fixture_sender))
        from dots_lite_hermes.delivery_helper import deliver
        files = home / "files"
        files.mkdir(mode=0o700)
        path = files / "fixture.bin"
        data = b"\x00\xfftrusted-artifact\x80"
        path.write_bytes(data)
        path.chmod(0o600)
        payload = {
            "id": "D1", "attempt_id": "A1", "task_id": "Q1", "kind": "artifact",
            "target": {"platform": "weixin", "chat_id": "wxid_fixture_only"},
            "artifact": {"id": "F1", "path": "result.bin", "local_path": str(path),
                         "sha256": hashlib.sha256(data).hexdigest(), "size": len(data)},
        }
        result = deliver(payload, source, files)
        if result["status"] != "delivered":
            raise RuntimeError("Official helper did not confirm fixture media: " + json.dumps(result))
        first = received[0]
        if (first["chat_id"], first["message"], first["thread_id"]) != ("wxid_fixture_only", "", None) or len(first["media"]) != 1:
            raise RuntimeError("Official sender did not receive exactly the authorized file and empty text")
        snapshot_path, sent_hex = first["media"][0]
        if Path(snapshot_path) == path or Path(snapshot_path).name != path.name or sent_hex != data.hex() or Path(snapshot_path).exists():
            raise RuntimeError("Official sender did not consume and clean a private snapshot with the same name and bytes")
        if result["receipt"] != {"success": True, "platform": "weixin", "chat_id": "wxid_fixture_only", "message_id": "fixture-media-1"}:
            raise RuntimeError("Native receipt was fabricated or changed")
        if result["verification"] != {"media_only": True, "artifact_id": "F1", "sha256": hashlib.sha256(data).hexdigest(),
                                      "size": len(data), "target_exact": True}:
            raise RuntimeError("Media verification does not match the actual fixture file")
        receipt_state["replace_original"] = True
        raced = deliver(payload, source, files)
        receipt_state["replace_original"] = False
        path.write_bytes(data)
        if raced["status"] != "delivered" or received[-1]["media"][0][1] != data.hex():
            raise RuntimeError("Native sender read replaced original bytes after validation: " + json.dumps(raced))
        outside = home / ".env"
        outside.write_text("fixture-only")
        outside.chmod(0o600)
        invalid_payloads = [
            {**payload, "artifact": {**payload["artifact"], "sha256": "0" * 64}},
            {**payload, "artifact": {**payload["artifact"], "local_path": str(outside)}},
            {**payload, "target": {"platform": "weixin", "chat_id": ""}},
            {**payload, "target": {"platform": "weixin", "chat_id": "wxid_fixture_only", "thread_id": "other-thread"}},
            {**payload, "kind": "text", "text": "MEDIA:" + str(path)},
        ]
        for invalid in invalid_payloads:
            before = len(received)
            failed = deliver(invalid, source, files)
            if failed["status"] != "failed" or failed.get("definitely_not_sent") is not True or len(received) != before:
                raise RuntimeError("Pre-send validation allowed a wrong file, target or injected media directive")
        for changes in (
            {"partial_success": True}, {"media_dropped": [str(path)]}, {"warnings": ["media omitted"]},
            {"message_id": ""}, {"message_id": " "}, {"message_id": 3}, {"media_delivered": False}, {"success": False},
        ):
            receipt_state["value"] = {"success": True, "platform": "weixin", "chat_id": "wxid_fixture_only", "message_id": "fixture-media-1", **changes}
            unknown = deliver(payload, source, files)
            if unknown["status"] != "unknown" or "definitely_not_sent" in unknown or "verification" in unknown:
                raise RuntimeError("Partial or insufficient native receipt was incorrectly treated as delivered")
        receipt_state["value"] = {"success": True, "platform": "weixin", "chat_id": "wxid_fixture_only", "message_id": "fixture-text-1"}
        text = deliver({**payload, "kind": "text", "text": "A question needs your answer"}, source, files)
        if text["status"] != "delivered" or received[-1]["message"] != "A question needs your answer" or received[-1]["media"]:
            raise RuntimeError("Text notification was not sent as exactly one text-only unit")
        from dots_lite_hermes.delivery import NativeSender
        process_result = NativeSender(sys.executable, source, files).send({
            **payload, "target": {"platform": "weixin", "chat_id": ""},
        })
        if process_result.get("definitely_not_sent") is not True or "exact platform and chat" not in process_result.get("error", ""):
            raise RuntimeError("Fixed native Python/helper subprocess did not preserve its runtime or stdin boundary")
        report = {"official_helper_fixture_media": True, "native_receipt_preserved": True,
                  "actual_media_bytes": len(data), "pre_send_rejections": len(invalid_payloads),
                  "insufficient_receipts_unknown": 8, "text_only_notification": True,
                  "fixed_native_subprocess": True, "mutable_original_replacement_isolated": True,
                  "real_weixin_sent": False}
        if report_path:
            report_path.write_text(json.dumps(report, indent=2))
            report_path.chmod(0o600)
        print(json.dumps(report))


if __name__ == "__main__":
    parser = argparse.ArgumentParser()
    parser.add_argument("--hermes-source", required=True, type=Path)
    parser.add_argument("--report", type=Path)
    options = parser.parse_args()
    smoke(options.hermes_source, options.report)
