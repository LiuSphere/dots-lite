"""Opt-in real official Hermes model/MCP/plugin/Go-Hub integration in a private home."""

import argparse
import asyncio
import hashlib
import json
import os
from pathlib import Path
import socket
import sys
import time
from urllib.parse import quote
import uuid

from dots_lite_hermes.client import HubClient
from dots_lite_hermes.runs import RunsClient
from dots_lite_hermes.worker import FollowupWorker


async def smoke(options):
    home = options.home.resolve()
    os.environ["HERMES_HOME"] = str(home)
    os.environ["DOTS_LITE_SOCKET"] = options.socket
    os.chdir(home)
    sys.path.insert(0, str(options.hermes_source.resolve()))
    from gateway.config import PlatformConfig
    from gateway.platforms.api_server import APIServerAdapter
    from hermes_cli.plugins import discover_plugins, get_plugin_manager

    discover_plugins()
    plugins = get_plugin_manager().list_plugins()
    if not any(p.get("name") == "dots-lite" and p.get("hooks") == 3 and not p.get("error") for p in plugins):
        raise RuntimeError("Official Hermes source plugin is not loaded")
    # A full Hermes gateway performs this startup step. Direct APIServerAdapter
    # embedding must call the same official discovery entry point before admission.
    from tools.mcp_tool_discovery import discover_mcp_tools
    discovered = await asyncio.to_thread(discover_mcp_tools, ["dots_lite"])
    if not {"mcp__dots_lite__dots_lite_query", "mcp__dots_lite__dots_lite_record"}.issubset(discovered):
        raise RuntimeError("Native MCP discovery did not expose the required real tools")
    from model_tools import get_tool_definitions
    definitions = get_tool_definitions(enabled_toolsets=["dots_lite"], quiet_mode=True)
    available = {tool.get("function", {}).get("name") for tool in definitions}
    if not {"mcp__dots_lite__dots_lite_query", "mcp__dots_lite__dots_lite_record"}.issubset(available):
        raise RuntimeError("Native model tool selection omitted the required real tools")
    print(json.dumps({"native_bootstrap_tool_count": len(available)}), flush=True)
    token = (home / "api-token").read_text().strip()
    with socket.socket() as probe:
        probe.bind(("127.0.0.1", 0))
        port = probe.getsockname()[1]
    adapter = APIServerAdapter(PlatformConfig(enabled=True, extra={
        "host": "127.0.0.1", "port": port, "key": token,
    }))
    if not await adapter.connect():
        raise RuntimeError("Private native API did not start")
    native = RunsClient(f"http://127.0.0.1:{port}", token)
    hub = HubClient(options.socket)
    worker = FollowupWorker(hub, native)

    def query(name):
        arguments = {"query": name, "task_id": options.task_id}
        reference = hub.request("POST", "/v1/invocations", {
            "tool": "dots_lite_query", "arguments": arguments,
            "source": {
                "kind": "gateway", "platform": "fixture", "sender_id": "sender-native",
                "chat_id": "chat-native", "session_id": "native-smoke-observer",
                "native_task_id": "explicit-test-observer", "tool_call_id": uuid.uuid4().hex,
            },
        })
        return hub.request("POST", "/v1/tools/call", {
            "tool": "dots_lite_query", "arguments": arguments, "context_ref": reference["context_ref"],
        })

    try:
        initial = await asyncio.to_thread(query, "followups")
        immutable_fields = ("run_id", "session_id", "idempotency_key", "request")
        previous = {
            f["id"]: {key: f.get(key) for key in immutable_fields}
            for f in initial or [] if f["task_id"] == options.task_id and f.get("run_id")
        }
        deadline = time.monotonic() + options.timeout
        selected = None
        while time.monotonic() < deadline:
            await asyncio.to_thread(worker.step)
            followups = await asyncio.to_thread(query, "followups")
            candidates = [f for f in followups or [] if f["task_id"] == options.task_id]
            if candidates:
                selected = max(candidates, key=lambda f: f["created_at"])
                if selected["status"] in {"handled", "blocked"}:
                    break
            await asyncio.sleep(2)
        else:
            raise RuntimeError("Real native followup did not reach a business result before the test deadline")
        if selected["status"] != "handled":
            raise RuntimeError("Real native followup requires user input: " + json.dumps(selected.get("evidence", {})))
        while time.monotonic() < deadline:
            native_state = await asyncio.to_thread(native.request, "GET", "/v1/runs/" + quote(selected["run_id"], safe=""))
            if native_state["status"] in {"completed", "failed", "cancelled"}:
                break
            await asyncio.sleep(1)
        if native_state["status"] != "completed":
            raise RuntimeError("Native followup did not complete: " + str(native_state.get("error", native_state["status"])))
        messages = await asyncio.to_thread(native.request, "GET", "/api/sessions/" + quote(selected["session_id"], safe="") + "/messages")
        calls = []
        text_call_ids = set()
        verification_evidence = []
        for message in messages["data"]:
            for call in message.get("tool_calls") or []:
                function = call.get("function", {})
                arguments = function.get("arguments", {})
                if isinstance(arguments, str):
                    arguments = json.loads(arguments)
                calls.append({
                    "name": function.get("name"), "query": arguments.get("query"),
                    "kind": arguments.get("kind"), "path": arguments.get("path"),
                    "status": arguments.get("status"),
                })
                if function.get("name") == "mcp__dots_lite__dots_lite_query" and arguments.get("query") == "text" and arguments.get("path") == options.artifact_path:
                    text_call_ids.add(call["id"])
                if function.get("name") == "mcp__dots_lite__dots_lite_record" and arguments.get("kind") == "verification" and arguments.get("status") == "passed":
                    verification_evidence.append(arguments.get("evidence", {}))
        queries = {call["query"] for call in calls if call["name"] == "mcp__dots_lite__dots_lite_query"}
        if not {"session", "goal"}.issubset(queries):
            raise RuntimeError("Native transcript does not prove both session and Goal queries: " + json.dumps(calls))
        if not any(c["name"] == "mcp__dots_lite__dots_lite_record" and c["kind"] == "followup" for c in calls):
            raise RuntimeError("Native transcript has no actual followup evidence record")
        if not any(message.get("role") == "tool" and message.get("tool_call_id") in text_call_ids and options.expected_marker in str(message.get("content", "")) for message in messages["data"]):
            raise RuntimeError("Native transcript does not prove Hermes actually read the declared marker file")
        artifacts = await asyncio.to_thread(query, "artifacts")
        acquired = []
        marker_acquired = False
        for artifact in artifacts.get("items", []):
            if artifact.get("status") != "acquired":
                raise RuntimeError("Expected automatic artifact remains unacquired")
            path = Path(artifact["local_path"])
            data = path.read_bytes()
            metadata = artifact["file"]
            if len(data) != metadata["size"] or hashlib.sha256(data).hexdigest() != metadata["sha256"]:
                raise RuntimeError("Actual private artifact differs from Hub receipt")
            if metadata["path"] == options.artifact_path:
                marker_acquired = True
                if data != (options.expected_marker + "\n").encode():
                    raise RuntimeError("Actually acquired file differs from the exact marker plus one LF")
                if not any(options.expected_marker in json.dumps(evidence) and metadata["sha256"] in json.dumps(evidence) and evidence.get("checks") for evidence in verification_evidence):
                    raise RuntimeError("Hermes did not record passing checks with the actual text and hash")
            acquired.append({"local_path": str(path), "sha256": metadata["sha256"], "size": len(data)})
        if not marker_acquired:
            raise RuntimeError("Declared marker artifact was not actually acquired")
        task = await asyncio.to_thread(query, "task")
        if task["verification"] != "passed":
            raise RuntimeError("Hub did not accept actual Hermes verification")
        final_followups = await asyncio.to_thread(query, "followups")
        retained = []
        for identifier, baseline in previous.items():
            current = next(f for f in final_followups if f["id"] == identifier)
            if {key: current.get(key) for key in immutable_fields} != baseline:
                raise RuntimeError("A prior admitted native request/run binding was changed")
            retained.append({"id": identifier, "run_id": current["run_id"], "status": current["status"]})
        report = {
            "uid": os.getuid(), "task_id": options.task_id, "run_id": selected["run_id"],
            "followup_id": selected["id"], "parent_id": selected.get("parent_id"),
            "prior_attempts_preserved": retained,
            "native_status": native_state["status"], "followup_status": selected["status"],
            "actual_native_calls": calls, "artifacts": acquired,
            "evidence": selected.get("evidence"), "wechat_delivery_verified": False,
        }
        for name, payload in (("native-hub-result.json", report), ("native-hub-transcript.json", messages)):
            path = home / name
            path.write_text(json.dumps(payload, ensure_ascii=False, indent=2))
            path.chmod(0o600)
        print(json.dumps(report, ensure_ascii=False))
    finally:
        await adapter.disconnect()


if __name__ == "__main__":
    parser = argparse.ArgumentParser()
    parser.add_argument("--home", required=True, type=Path)
    parser.add_argument("--hermes-source", required=True, type=Path)
    parser.add_argument("--socket", required=True)
    parser.add_argument("--task-id", required=True)
    parser.add_argument("--artifact-path", required=True)
    parser.add_argument("--expected-marker", required=True)
    parser.add_argument("--timeout", type=int, default=240)
    asyncio.run(smoke(parser.parse_args()))
