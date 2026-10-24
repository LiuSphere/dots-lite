import asyncio
import base64
from contextlib import contextmanager
import json
import hashlib
import os
from pathlib import Path
import socket
import socketserver
import sys
import tempfile
import threading
import unittest
from http.server import BaseHTTPRequestHandler

from mcp import ClientSession, StdioServerParameters
from mcp import types
from mcp.client.stdio import stdio_client


class MCPTests(unittest.IsolatedAsyncioTestCase):
    @unittest.skipUnless(hasattr(socket, "AF_UNIX"), "UID socket integration runs on Linux")
    async def test_attachment_tools_forward_references_and_reject_model_supplied_bytes(self):
        with hub_peer(responder=lambda path, body: {"operation_id": "attachment-operation"}) as (socket_path, requests):
            params = StdioServerParameters(
                command=sys.executable, args=["-m", "dots_duet_lite_hermes"],
                env={**os.environ, "DOTS_DUET_SOCKET": socket_path},
            )
            async with asyncio.timeout(10):
                async with stdio_client(params) as streams:
                    async with ClientSession(*streams) as session:
                        await session.initialize()
                        for args in (
                            {"action": "task.create", "project_id": "demo", "inputs": [{"name": "input.bin", "local_path": "/native/input.bin"}]},
                            {"action": "files.upload", "task_id": "Q1", "upload": {"name": "input.bin", "local_path": "/native/input.bin"}},
                        ):
                            result = await session.call_tool("duet_prepare", {**args, "_context_ref": "private-registration"})
                            self.assertFalse(result.is_error, result)
                            self.assertEqual(requests[-1][1]["arguments"], args)
                        raw = await session.call_tool("duet_prepare", {
                            "action": "files.upload", "task_id": "Q1", "_context_ref": "private-registration",
                            "upload": {"name": "input.bin", "data_base64": "YWJj", "sha256": "0" * 64, "size": 3},
                        })
                        self.assertTrue(raw.is_error)
                        self.assertEqual(len(requests), 2)

    async def test_unbound_tool_call_is_rejected_over_real_stdio(self):
        params = StdioServerParameters(
            command=sys.executable,
            args=["-m", "dots_duet_lite_hermes"],
            env=dict(os.environ),
        )
        async with asyncio.timeout(10):
            async with stdio_client(params) as streams:
                async with ClientSession(*streams) as session:
                    await session.initialize()
                    listed = await session.list_tools()
                    self.assertIn("duet_query", {tool.name for tool in listed.tools})
                    result = await session.call_tool("duet_query", {"query": "tasks"})
                    self.assertTrue(result.is_error)
                    self.assertIn("context", result.content[0].text.lower())

    @unittest.skipUnless(hasattr(socket, "AF_UNIX"), "UID socket integration runs on Linux")
    async def test_bound_query_reaches_hub_with_original_arguments(self):
        with hub_peer() as (socket_path, requests):
            params = StdioServerParameters(
                command=sys.executable, args=["-m", "dots_duet_lite_hermes"],
                env={**os.environ, "DOTS_DUET_SOCKET": socket_path},
            )
            async with asyncio.timeout(10):
                async with stdio_client(params) as streams:
                    async with ClientSession(*streams) as session:
                        await session.initialize()
                        result = await session.call_tool("duet_query", {
                            "query": "tasks", "_context_ref": "registered-reference",
                        })
                        self.assertFalse(result.is_error)
                        self.assertEqual(json.loads(result.content[0].text), {"tasks": [{"task_id": "Q1"}]})
            self.assertEqual(requests, [("/v1/tools/call", {
                "tool": "duet_query", "arguments": {"query": "tasks"},
                "context_ref": "registered-reference",
            })])

    @unittest.skipUnless(hasattr(socket, "AF_UNIX"), "UID socket integration runs on Linux")
    async def test_approval_uses_real_mcp_elicitation_and_only_confirms_that_request(self):
        prompts = []

        async def approve(context, params):
            prompts.append(params.message)
            return types.ElicitResult(action="accept", content={})

        def responder(path, body):
            if path == "/v1/tools/call":
                return {"approval": {
                    "task_id": "Q1", "request_id": "P1", "instance_id": "instance-a",
                    "method": "item/commandExecution/requestApproval",
                    "params": {"command": "echo harmless", "availableDecisions": ["accept", "decline"]},
                }}
            return {"submitted": True, "request_id": "P1"}

        with hub_peer(responder) as (socket_path, requests):
            params = StdioServerParameters(
                command=sys.executable, args=["-m", "dots_duet_lite_hermes"],
                env={**os.environ, "DOTS_DUET_SOCKET": socket_path},
            )
            async with asyncio.timeout(10):
                async with stdio_client(params) as streams:
                    async with ClientSession(*streams, elicitation_callback=approve) as session:
                        await session.initialize()
                        result = await session.call_tool("duet_approve", {
                            "task_id": "Q1", "request_id": "P1", "_context_ref": "approval-reference",
                        })
                        self.assertFalse(result.is_error)
            self.assertEqual(len(prompts), 1)
            self.assertIn("echo harmless", prompts[0])
            self.assertEqual(requests[-1], ("/v1/approvals/confirm", {
                "tool": "duet_approve", "arguments": {"task_id": "Q1", "request_id": "P1"},
                "context_ref": "approval-reference", "request_id": "P1",
                "instance_id": "instance-a", "decision": "accept",
            }))

    @unittest.skipUnless(hasattr(socket, "AF_UNIX"), "UID socket integration runs on Linux")
    async def test_download_materializes_binary_and_empty_files_without_returning_base64(self):
        for data in (b"\x00\xffbinary\x80", b""):
            with self.subTest(size=len(data)):
                payload = {
                    "path": "out/result.bin", "data_base64": base64.b64encode(data).decode(),
                    "sha256": hashlib.sha256(data).hexdigest(), "size": len(data),
                    "returned_bytes": len(data), "offset": 0, "truncated": False,
                }
                with tempfile.TemporaryDirectory() as files:
                    with hub_peer(lambda path, body: payload) as (socket_path, requests):
                        params = StdioServerParameters(
                            command=sys.executable, args=["-m", "dots_duet_lite_hermes"],
                            env={**os.environ, "DOTS_DUET_SOCKET": socket_path, "DOTS_DUET_FILES_DIR": files},
                        )
                        async with asyncio.timeout(10):
                            async with stdio_client(params) as streams:
                                async with ClientSession(*streams) as session:
                                    await session.initialize()
                                    result = await session.call_tool("duet_query", {
                                        "query": "download", "task_id": "Q1", "path": "out/result.bin",
                                        "_context_ref": "file-reference",
                                    })
                                    self.assertFalse(result.is_error)
                                    saved = json.loads(result.content[0].text)
                                    self.assertNotIn("data_base64", saved)
                                    target = Path(saved["local_path"])
                                    self.assertEqual(target.parent, Path(files))
                                    self.assertEqual(target.read_bytes(), data)
                                    self.assertEqual(target.stat().st_mode & 0o777, 0o600)
                                    self.assertEqual(saved["acquired"], "complete")

    @unittest.skipUnless(hasattr(socket, "AF_UNIX"), "UID socket integration runs on Linux")
    async def test_decline_is_forwarded_and_cancel_leaves_native_approval_pending(self):
        for action in ("decline", "cancel"):
            with self.subTest(action=action):
                async def answer(context, params):
                    return types.ElicitResult(action=action)

                def responder(path, body):
                    if path == "/v1/tools/call":
                        return {"approval": {
                            "task_id": "Q1", "request_id": "P1", "instance_id": "instance-a",
                            "method": "item/fileChange/requestApproval", "params": {"path": "result.txt"},
                        }}
                    return {"submitted": True}

                with hub_peer(responder) as (socket_path, requests):
                    result = await self.call_bound(socket_path, "duet_approve", {
                        "task_id": "Q1", "request_id": "P1",
                    }, answer)
                    self.assertFalse(result.is_error)
                    if action == "decline":
                        self.assertEqual(requests[-1][1]["decision"], "decline")
                    else:
                        self.assertEqual(len(requests), 1)
                        self.assertEqual(json.loads(result.content[0].text)["status"], "pending")

    @unittest.skipUnless(hasattr(socket, "AF_UNIX"), "UID socket integration runs on Linux")
    async def test_missing_human_confirmation_and_model_decisions_never_confirm_approval(self):
        def responder(path, body):
            return {"approval": {
                "task_id": "Q1", "request_id": "P1", "instance_id": "instance-a",
                "method": "item/commandExecution/requestApproval", "params": {"command": "echo harmless"},
            }}

        for extra in ({}, {"decision": "accept"}, {"approved": True}):
            with self.subTest(extra=extra):
                with hub_peer(responder) as (socket_path, requests):
                    result = await self.call_bound(socket_path, "duet_approve", {
                        "task_id": "Q1", "request_id": "P1", **extra,
                    })
                    self.assertTrue(result.is_error)
                    self.assertFalse(any(path == "/v1/approvals/confirm" for path, body in requests))

    @unittest.skipUnless(hasattr(socket, "AF_UNIX"), "UID socket integration runs on Linux")
    async def test_corrupt_download_or_http_failure_does_not_acquire_a_file(self):
        payload = {
            "path": "out/result.bin", "data_base64": "YQ==", "sha256": "0" * 64,
            "size": 1, "returned_bytes": 1, "offset": 0, "truncated": False,
        }
        for response in (payload, (403, {"error": {"code": "denied", "message": "not authorized"}})):
            with self.subTest(response=response):
                with tempfile.TemporaryDirectory() as files:
                    with hub_peer(lambda path, body: response) as (socket_path, requests):
                        result = await self.call_bound(socket_path, "duet_query", {
                            "query": "download", "task_id": "Q1", "path": "out/result.bin",
                        }, files=files)
                        self.assertTrue(result.is_error)
                        self.assertEqual(list(Path(files).iterdir()), [])

    @unittest.skipUnless(hasattr(socket, "AF_UNIX"), "UID socket integration runs on Linux")
    async def test_partial_download_remains_partial_after_verified_write(self):
        payload = {
            "path": "out/result.bin", "data_base64": "YQ==",
            "sha256": "ca978112ca1bbdcafac231b39a23dc4da786eff8147c4e72b9807785afee48bb",
            "size": 2, "returned_bytes": 1, "offset": 1, "truncated": False,
        }
        with tempfile.TemporaryDirectory() as files:
            with hub_peer(lambda path, body: payload) as (socket_path, requests):
                result = await self.call_bound(socket_path, "duet_query", {
                    "query": "download", "task_id": "Q1", "path": "out/result.bin", "offset": 1,
                }, files=files)
                self.assertFalse(result.is_error)
                saved = json.loads(result.content[0].text)
                self.assertEqual(saved["acquired"], "partial")
                self.assertEqual(Path(saved["local_path"]).read_bytes(), b"a")

    @unittest.skipUnless(hasattr(socket, "AF_UNIX"), "UID socket integration runs on Linux")
    async def test_attachment_prepare_rejects_paths_or_missing_task_before_hub_call(self):
        valid = {"action": "files.upload", "task_id": "Q1", "upload": {
            "name": "input.bin", "local_path": "/native/input.bin",
        }}
        for arguments in (
            {**valid, "upload": {**valid["upload"], "name": "../input.bin"}},
            {key: value for key, value in valid.items() if key != "task_id"},
        ):
            with self.subTest(arguments=arguments):
                with hub_peer() as (socket_path, requests):
                    result = await self.call_bound(socket_path, "duet_prepare", arguments)
                    self.assertTrue(result.is_error)
                    self.assertEqual(requests, [])

    async def call_bound(self, socket_path, tool, arguments, elicitation=None, files=None):
        environment = {**os.environ, "DOTS_DUET_SOCKET": socket_path}
        if files:
            environment["DOTS_DUET_FILES_DIR"] = files
        params = StdioServerParameters(
            command=sys.executable, args=["-m", "dots_duet_lite_hermes"], env=environment,
        )
        async with asyncio.timeout(10):
            async with stdio_client(params) as streams:
                async with ClientSession(*streams, elicitation_callback=elicitation) as session:
                    await session.initialize()
                    return await session.call_tool(tool, {**arguments, "_context_ref": "test-reference"})


@contextmanager
def hub_peer(responder=None):
    """An external HTTP peer on the same transport used by the real Linux hub."""
    requests = []

    class Handler(BaseHTTPRequestHandler):
        def do_POST(self):
            body = json.loads(self.rfile.read(int(self.headers["Content-Length"])))
            requests.append((self.path, body))
            payload = responder(self.path, body) if responder else {"tasks": [{"task_id": "Q1"}]}
            status, payload = payload if isinstance(payload, tuple) else (200, payload)
            encoded = json.dumps(payload).encode()
            self.send_response(status)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(encoded)))
            self.end_headers()
            self.wfile.write(encoded)

        def log_message(self, *args):
            pass

    class Server(socketserver.ThreadingMixIn, socketserver.UnixStreamServer):
        daemon_threads = True

    with tempfile.TemporaryDirectory() as temporary:
        path = str(Path(temporary) / "hub.sock")
        server = Server(path, Handler)
        thread = threading.Thread(target=server.serve_forever, daemon=True)
        thread.start()
        try:
            yield path, requests
        finally:
            server.shutdown()
            server.server_close()
            thread.join()


if __name__ == "__main__":
    unittest.main()
