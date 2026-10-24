"""Opt-in MCP-to-real-Hub check; run as the explicitly bound test Linux UID."""

import argparse
import asyncio
import json
import os
import sys
import uuid

from mcp import ClientSession, StdioServerParameters
from mcp.client.stdio import stdio_client

from dots_lite_hermes.client import HubClient, HubError


async def smoke(options):
    hub = HubClient(options.socket)
    arguments = {"query": "tasks"}
    source = {
        "kind": "gateway", "platform": options.platform, "sender_id": options.sender_id,
        "chat_id": options.chat_id, "session_id": "duet-integration-" + uuid.uuid4().hex,
        "native_task_id": "explicit-test-fixture", "tool_call_id": "query-tasks",
    }
    registered = hub.request("POST", "/v1/invocations", {
        "tool": "dots_lite_query", "arguments": arguments, "source": source,
    })
    params = StdioServerParameters(
        command=sys.executable, args=["-m", "dots_lite_hermes"],
        env={**os.environ, "DOTS_DUET_SOCKET": options.socket},
    )
    async with asyncio.timeout(15):
        async with stdio_client(params) as streams:
            async with ClientSession(*streams) as session:
                await session.initialize()
                result = await session.call_tool("dots_lite_query", {
                    **arguments, "_context_ref": registered["context_ref"],
                })
                if result.is_error:
                    raise RuntimeError(result.content[0].text)
                tasks = json.loads(result.content[0].text)
                changed = await session.call_tool("dots_lite_query", {
                    "query": "projects", "_context_ref": registered["context_ref"],
                })
                if not changed.is_error or "invalid_context" not in changed.content[0].text:
                    raise RuntimeError("Hub did not reject altered arguments for the same context")
    try:
        hub.request("POST", "/v1/invocations", {
            "tool": "dots_lite_query", "arguments": arguments,
            "source": {**source, "sender_id": "unbound-sender-" + uuid.uuid4().hex},
        })
    except HubError as error:
        if "forbidden" not in str(error):
            raise
    else:
        raise RuntimeError("Hub accepted an unbound source")
    print(json.dumps({
        "uid": os.getuid(), "real_hub_mcp_query": True, "changed_arguments_rejected": True,
        "unbound_sender_rejected": True, "tasks": tasks, "source_is_explicit_test_fixture": True,
    }))


if __name__ == "__main__":
    parser = argparse.ArgumentParser()
    parser.add_argument("--socket", required=True)
    parser.add_argument("--platform", required=True)
    parser.add_argument("--sender-id", required=True)
    parser.add_argument("--chat-id", required=True)
    asyncio.run(smoke(parser.parse_args()))
