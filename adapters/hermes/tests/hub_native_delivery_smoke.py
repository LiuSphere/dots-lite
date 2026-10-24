"""Opt-in real Hub/executor/worker/native-helper flow, with fixture channel I/O only."""

import argparse
import asyncio
from dataclasses import replace
import hashlib
import json
import os
from pathlib import Path
import sys
import tempfile
import time
import uuid

from dots_lite_hermes.client import HubClient
from dots_lite_hermes.worker import FollowupWorker


async def smoke(options):
    with tempfile.TemporaryDirectory(prefix="duet-channel-fixture-") as directory:
        home = Path(directory)
        os.environ["HERMES_HOME"] = str(home)
        os.environ["DOTS_DUET_SOCKET"] = options.socket
        os.environ["DOTS_DUET_FILES_DIR"] = str(home / "files")
        sys.path.insert(0, str(options.hermes_source.absolute()))
        os.chdir(home)
        (home / "config.yaml").write_text("""platforms:
  weixin:
    enabled: true
    token: fixture-only-not-a-credential
    extra:
      account_id: fixture-account
plugins:
  enabled: [weixin, dots-duet]
""")
        plugins = home / "plugins"
        plugins.mkdir()
        import dots_lite_hermes
        (plugins / "dots-duet").symlink_to(Path(dots_lite_hermes.__file__).parent, target_is_directory=True)
        from hermes_cli.plugins import discover_plugins, get_plugin_manager
        from hermes_cli.lifecycle import ainvoke_hook, invoke_hook
        from gateway.session import SessionSource
        from gateway.config import Platform
        from gateway.platforms.event import MessageEvent, MessageType
        from gateway.platforms.base import cache_document_from_bytes
        from gateway.session_context import set_session_vars, clear_session_vars
        from gateway.session_ingress_media import capture_native_media
        from gateway.platform_registry import platform_registry
        discover_plugins()
        if not any(p.get("name") == "dots-duet" and p.get("hooks") == 3 and not p.get("error")
                   for p in get_plugin_manager().list_plugins()):
            raise RuntimeError("Official PluginManager did not load all three trusted-source hooks")
        hub = HubClient(options.socket)
        origin = {"kind": "gateway", "platform": "weixin", "sender_id": "sender-fixture",
                  "chat_id": "wxid_fixture_only", "session_id": "fixture-session-" + uuid.uuid4().hex,
                  "native_task_id": "explicit-test-fixture"}

        def call(tool, args):
            registered = hub.request("POST", "/v1/invocations", {
                "tool": tool, "arguments": args, "source": {**origin, "tool_call_id": uuid.uuid4().hex},
            })
            return hub.request("POST", "/v1/tools/call", {"tool": tool, "arguments": args,
                                                          "context_ref": registered["context_ref"]})

        def query(name, task_id=None):
            return call("dots_lite_query", {"query": name, **({"task_id": task_id} if task_id else {})})

        # Read first. Never claim another test's artifact or delivery outbox unit.
        for task in query("tasks"):
            if task.get("paused"):
                continue
            pending = [row["id"] for row in query("deliveries", task["task_id"])
                       if row["status"] in {"pending", "dispatching"} or row.get("retryable")]
            artifacts = query("artifacts", task["task_id"]).get("items", [])
            pending += [row["id"] for row in artifacts if row["status"] == "available"]
            if pending:
                raise RuntimeError("Another task has unconsumed test queue units: " + json.dumps(pending))

        data = b"\x00\xfftrusted-artifact\x80"
        digest = hashlib.sha256(data).hexdigest()
        staged = cache_document_from_bytes(data, "input.bin")
        message_id, session_key = uuid.uuid4().hex, "fixture-ingress-" + uuid.uuid4().hex
        source = SessionSource(platform=Platform("weixin"), chat_id="wxid_fixture_only",
                               user_id="sender-fixture", message_id=message_id)
        event = MessageEvent(text="Copy this fixture attachment", message_type=MessageType.DOCUMENT,
                             source=source, message_id=message_id, media_urls=[staged])
        await ainvoke_hook("pre_gateway_dispatch", event=event, gateway=None, session_store=None)
        await ainvoke_hook("post_gateway_admission", session_key=session_key, platform="weixin",
                           source=source.to_dict(), message_id=message_id, text=event.text)
        retained = capture_native_media([staged])[0]
        if retained["sha256"] != digest or Path(retained["path"]).read_bytes() != data:
            raise RuntimeError("Official retained ingress media did not preserve fixture bytes")
        output_name = "delivery-fixture-" + uuid.uuid4().hex + ".bin"
        args = {
            "action": "task.create", "project_id": options.project, "name": "binary-channel-fixture",
            "input": "Copy the attached input.bin byte for byte to " + output_name + ". Do not modify or decode its binary bytes. Verify SHA-256 " + digest + ". Do not do any other work. Reply COPY_OK.",
            "inputs": [{"name": "input.bin", "local_path": retained["path"]}], "artifacts": [output_name],
        }
        set_session_vars(platform="weixin", chat_id="wxid_fixture_only", user_id="sender-fixture",
                         session_key=session_key, session_id=origin["session_id"], message_id=message_id)
        try:
            directives = invoke_hook("pre_tool_call", tool_name="mcp__dots_lite__dots_lite_prepare", args=args,
                                     task_id="explicit-test-fixture", tool_call_id=uuid.uuid4().hex)
        finally:
            clear_session_vars([])
        modified = next((row["args"] for row in directives if isinstance(row, dict) and row.get("action") == "modify"), None)
        if not modified or {key: value for key, value in modified.items() if key != "_context_ref"} != args:
            raise RuntimeError("Official trusted plugin did not register path-only model arguments: " + json.dumps(directives))
        prepared = hub.request("POST", "/v1/tools/call", {"tool": "dots_lite_prepare", "arguments": args,
                                                           "context_ref": modified["_context_ref"]})
        task_id = prepared["task_id"]
        sent = []
        entry = platform_registry.get("weixin")

        async def fixture_sender(pconfig, chat_id, message, *, thread_id=None, media_files=None, **kwargs):
            media = [(str(path), Path(path).read_bytes()) for path, voice in media_files or []]
            if chat_id != "wxid_fixture_only" or thread_id is not None:
                raise RuntimeError("Native sender changed the exact fixture destination")
            if media and (message != "" or len(media) != 1 or media[0][1] != data):
                raise RuntimeError("Native sender did not receive exactly the copied binary artifact")
            receipt = {"success": True, "platform": "weixin", "chat_id": chat_id,
                       "message_id": "fixture-only-" + str(len(sent) + 1)}
            sent.append({"text": message, "media_size": len(media[0][1]) if media else None, "receipt": receipt})
            return dict(receipt)

        # This is the only replaced boundary. No native channel network is called.
        platform_registry.register(replace(entry, standalone_sender_fn=fixture_sender))
        from dots_lite_hermes.delivery_helper import deliver
        class Sender:
            def send(self, item):
                if item["task_id"] != task_id:
                    raise RuntimeError("Worker claimed another task's send unit")
                return deliver(item, options.hermes_source, home / "files")
        worker = FollowupWorker(hub, None, Sender())
        report = None
        try:
            accepted = call("dots_lite_submit", {"operation_id": prepared["operation_id"]})
            if accepted.get("stage") != "accepted":
                raise RuntimeError("Actual executor did not accept fixture task: " + json.dumps(accepted))
            deadline = time.monotonic() + options.timeout
            acquired = None
            while time.monotonic() < deadline:
                artifacts = query("artifacts", task_id).get("items", [])
                if any(row["status"] == "failed" for row in artifacts):
                    raise RuntimeError("Actual executor artifact capture failed: " + json.dumps(artifacts))
                if any(row["status"] == "available" for row in artifacts):
                    worker.step()
                    continue
                acquired = next((row for row in artifacts if row["status"] == "acquired"), None)
                if acquired:
                    break
                await asyncio.sleep(1)
            if not acquired:
                raise RuntimeError("Actual executor did not produce an acquired artifact before timeout")
            actual = Path(acquired["local_path"]).read_bytes()
            if actual != data or hashlib.sha256(actual).hexdigest() != digest:
                raise RuntimeError("Actual private artifact does not equal original binary ingress bytes")
            call("dots_lite_record", {"task_id": task_id, "kind": "verification", "status": "passed",
                                 "evidence": {"checks": [{"observed": "Private artifact exactly equals original 19-byte binary input",
                                                          "sha256": digest, "size": len(actual)}]}})
            for _ in range(4):
                deliveries = query("deliveries", task_id)
                if len(deliveries) == 2 and all(row["status"] == "delivered" for row in deliveries):
                    break
                worker.step()
            else:
                raise RuntimeError("Hub did not confirm text and media fixture deliveries")
            if len(sent) != 2 or sorted(row["kind"] for row in deliveries) != ["artifact", "text"]:
                raise RuntimeError("Expected exactly one text and one binary media send")
            for row in deliveries:
                if row["attempts"] != 1 or row["receipt"] not in [unit["receipt"] for unit in sent]:
                    raise RuntimeError("Hub did not retain one exact native receipt per send attempt")
            if query("task", task_id)["delivery"] != "delivered":
                raise RuntimeError("Task remained unconfirmed after all exact native receipts")
            if hub.request("GET", "/v1/deliveries/next") is not None or len(sent) != 2:
                raise RuntimeError("Delivered fixture units were dispatched twice")
            report = {"uid": os.getuid(), "task_id": task_id, "original_size": len(data), "sha256": digest,
                      "official_three_hook_ingress": True, "official_retained_alias": True,
                      "model_args_path_only": True, "real_windows_executor_binary_copy": True,
                      "real_hub_worker_official_send_helper": True, "fixture_native_receipts": deliveries,
                      "real_weixin_sent": False, "source_is_explicit_test_fixture": True}
        finally:
            stopped = call("dots_lite_stop", {"task_id": task_id})
            if not stopped.get("pause_recorded") or not query("task", task_id).get("paused"):
                raise RuntimeError("Fixture task pause intent was not persisted")
        options.report.write_text(json.dumps(report, ensure_ascii=False, indent=2))
        options.report.chmod(0o600)
        print(json.dumps(report, ensure_ascii=False))


if __name__ == "__main__":
    parser = argparse.ArgumentParser()
    parser.add_argument("--hermes-source", required=True, type=Path)
    parser.add_argument("--socket", required=True)
    parser.add_argument("--project", default="acceptance")
    parser.add_argument("--report", required=True, type=Path)
    parser.add_argument("--timeout", type=int, default=180)
    asyncio.run(smoke(parser.parse_args()))
