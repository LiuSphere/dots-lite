"""Opt-in real MCP/Hub followup reads using explicitly declared API source fixtures."""

import argparse
import asyncio
import json
import os
from pathlib import Path
import sys
import uuid

from mcp import ClientSession, StdioServerParameters
from mcp.client.stdio import stdio_client

from dots_lite_hermes.client import HubClient, HubError


async def smoke(options):
    hub = HubClient(options.socket)

    def register(arguments, source):
        return hub.request("POST", "/v1/invocations", {
            "tool": "dots_lite_query", "arguments": arguments, "source": source,
        })["context_ref"]

    arguments = {"query": "followups", "task_id": options.task_id}
    observer = {
        "kind": "gateway", "platform": "fixture", "sender_id": "sender-native",
        "chat_id": "chat-native", "session_id": "explicit-followups-observer",
        "native_task_id": "explicit-test-fixture", "tool_call_id": uuid.uuid4().hex,
    }
    params = StdioServerParameters(
        command=sys.executable, args=["-m", "dots_lite_hermes"],
        env={**os.environ, "DOTS_DUET_SOCKET": options.socket},
    )
    async with asyncio.timeout(20):
        async with stdio_client(params) as streams:
            async with ClientSession(*streams) as session:
                await session.initialize()
                async def read(source):
                    result = await session.call_tool("dots_lite_query", {
                        **arguments, "_context_ref": register(arguments, source),
                    })
                    if result.is_error:
                        raise RuntimeError(result.content[0].text)
                    rows = json.loads(result.content[0].text)
                    if not rows or any(row["task_id"] != options.task_id for row in rows):
                        raise RuntimeError("Followup query is empty or contains another task")
                    return rows

                rows = await read(observer)
                original = next(row for row in rows if row["id"] == options.original_followup)
                diagnostic = next(row for row in rows if row.get("parent_id") == original["id"])
                if original["status"] != "superseded" or diagnostic["status"] != "handled":
                    raise RuntimeError("Expected original and handled diagnostic records were not retained")
                tested = []
                for attempt in (original, diagnostic):
                    api_source = {
                        "kind": "api", "platform": "api_server", "chat_id": attempt["session_id"],
                        "session_id": attempt["session_id"], "native_task_id": "explicit-test-fixture",
                        "tool_call_id": uuid.uuid4().hex,
                    }
                    actual = await read(api_source)
                    if {row["id"] for row in actual} != {row["id"] for row in rows}:
                        raise RuntimeError("Bound API source did not receive its complete task followup history")
                    for invalid in ({"query": "followups"}, {"query": "followups", "task_id": "other-task-" + uuid.uuid4().hex}):
                        try:
                            register(invalid, api_source)
                        except HubError as error:
                            if "binding_conflict" not in str(error):
                                raise
                        else:
                            raise RuntimeError("Bound API source accepted a global or another-task followup query")
                    tested.append({"followup_id": attempt["id"], "status": attempt["status"], "run_id": attempt["run_id"]})
    report = {
        "uid": os.getuid(), "task_id": options.task_id, "attempts_read": tested,
        "real_mcp_stdio_to_hub": True, "global_query_rejected": True,
        "other_task_query_rejected": True, "source_is_explicit_test_fixture": True,
        "model_rerun": False,
    }
    options.report.write_text(json.dumps(report, indent=2))
    options.report.chmod(0o600)
    print(json.dumps(report))


if __name__ == "__main__":
    parser = argparse.ArgumentParser()
    parser.add_argument("--socket", required=True)
    parser.add_argument("--task-id", required=True)
    parser.add_argument("--original-followup", required=True)
    parser.add_argument("--report", required=True, type=Path)
    asyncio.run(smoke(parser.parse_args()))
