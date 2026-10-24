"""Native MCP stdio service; task state and authorization stay in the Go hub."""

import json
from functools import partial

import anyio

from jsonschema import ValidationError, validate
from mcp import types
from mcp.server import Server
from mcp.server.stdio import stdio_server
from mcp.shared.exceptions import MCPError

from .client import HubClient, HubError
from .downloads import materialize_download
from .schemas import DESCRIPTIONS, TOOL_SCHEMAS


def error_result(message):
    return types.CallToolResult(
        content=[types.TextContent(type="text", text=message)], isError=True,
    )


def create_server():
    approval_lock = anyio.Lock()

    async def list_tools(context, params):
        return types.ListToolsResult(tools=[
            types.Tool(name=name, description=DESCRIPTIONS[name], inputSchema=value)
            for name, value in TOOL_SCHEMAS.items()
        ])

    async def call_tool(context, params):
        if params.name not in TOOL_SCHEMAS:
            return error_result("Unknown dots-lite tool")
        arguments = dict(params.arguments or {})
        try:
            validate(arguments, TOOL_SCHEMAS[params.name])
        except ValidationError as error:
            return error_result("Invalid dots-lite arguments: " + error.message)
        context_ref = arguments.pop("_context_ref", "")
        if not context_ref:
            return error_result("Missing trusted context reference; the Hermes source plugin is required")
        try:
            hub = HubClient.from_environment()
            result = await anyio.to_thread.run_sync(partial(hub.request, "POST", "/v1/tools/call", {
                "tool": params.name, "arguments": arguments, "context_ref": context_ref,
            }))
            if params.name == "dots_lite_approve":
                async with approval_lock:
                    result = await confirm_approval(context, hub, result, arguments, context_ref)
            if params.name == "dots_lite_query" and arguments.get("query") == "download":
                result = await anyio.to_thread.run_sync(materialize_download, result)
        except (HubError, ValueError, OSError) as error:
            return error_result(str(error))
        return types.CallToolResult(content=[types.TextContent(type="text", text=json.dumps(result, ensure_ascii=False))])

    return Server(
        "dots-lite", version="0.2.0",
        on_list_tools=list_tools, on_call_tool=call_tool,
    )


async def confirm_approval(context, hub, result, arguments, context_ref):
    approval = result.get("approval", {})
    if approval.get("method") not in {
        "item/commandExecution/requestApproval", "item/fileChange/requestApproval",
    }:
        raise HubError("This approval requires a structured permission form; it remains pending")
    decisions = approval.get("params", {}).get("availableDecisions")
    if decisions is not None and not all(choice in decisions for choice in ("accept", "decline")):
        raise HubError("This native approval cannot be represented by a single confirmation; it remains pending")
    message = "Approve this exact Codex request once?\n" + json.dumps(approval, ensure_ascii=False, indent=2)
    try:
        answer = await context.session.elicit_form(
            message=message,
            requested_schema={"type": "object", "properties": {}, "additionalProperties": False},
            related_request_id=context.request_id,
        )
    except MCPError as error:
        raise HubError(f"Human confirmation is unavailable; the request remains pending: {error}") from error
    if answer.action == "cancel":
        return {"request_id": arguments["request_id"], "status": "pending", "confirmation": "cancelled"}
    if answer.action not in {"accept", "decline"}:
        raise HubError("Unsupported human confirmation result; the request remains pending")
    return await anyio.to_thread.run_sync(partial(hub.request, "POST", "/v1/approvals/confirm", {
        "tool": "dots_lite_approve", "arguments": arguments, "context_ref": context_ref,
        "request_id": approval["request_id"], "instance_id": approval["instance_id"],
        "decision": answer.action,
    }))


async def serve():
    server = create_server()
    async with stdio_server() as streams:
        await server.run(*streams, server.create_initialization_options())
