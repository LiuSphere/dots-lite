"""Official Hermes plugin lifecycle and ContextVars against an external registration peer."""

import argparse
import asyncio
import base64
import hashlib
from http.server import BaseHTTPRequestHandler
import json
import os
from pathlib import Path
import socketserver
import sys
import tempfile
import threading


async def smoke(options):
    with tempfile.TemporaryDirectory() as directory:
        home = Path(directory)
        os.environ["HERMES_HOME"] = str(home)
        os.environ["DOTS_DUET_SOCKET"] = str(home / "registration.sock")
        sys.path.insert(0, str(options.hermes_source.absolute()))
        os.chdir(home)
        (home / "config.yaml").write_text("plugins:\n  enabled: [dots-duet]\n")
        plugins = home / "plugins"
        plugins.mkdir()
        import dots_lite_hermes
        (plugins / "dots-duet").symlink_to(Path(dots_lite_hermes.__file__).parent, target_is_directory=True)
        from hermes_cli.plugins import discover_plugins, get_plugin_manager
        from hermes_cli.lifecycle import ainvoke_hook, invoke_hook
        from gateway.session import SessionSource
        from gateway.config import Platform
        from gateway.platforms.event import MessageEvent
        from gateway.platforms.base import cache_document_from_bytes
        from gateway.session_context import set_session_vars, clear_session_vars
        from gateway.session_ingress_media import capture_native_media
        requests = []
        class Handler(BaseHTTPRequestHandler):
            def do_POST(self):
                body = json.loads(self.rfile.read(int(self.headers["Content-Length"])))
                requests.append((self.path, body))
                data = b'{"context_ref":"fixture-registration"}'
                self.send_response(200)
                self.send_header("Content-Length", str(len(data)))
                self.end_headers()
                self.wfile.write(data)
            def log_message(self, *args):
                pass
        server = socketserver.UnixStreamServer(os.environ["DOTS_DUET_SOCKET"], Handler)
        thread = threading.Thread(target=server.serve_forever, daemon=True)
        thread.start()
        try:
            discover_plugins()
            if not any(p.get("name") == "dots-duet" and p.get("hooks") == 3 and not p.get("error")
                       for p in get_plugin_manager().list_plugins()):
                raise RuntimeError("Official PluginManager did not load all three hooks")
            data = b"\x00\xfftrusted-artifact\x80"
            staged = cache_document_from_bytes(data, "input.bin")
            source = SessionSource(platform=Platform.TELEGRAM, chat_id="fixture-chat", user_id="fixture-user",
                                   message_id="fixture-message")
            event = MessageEvent(text="fixture", source=source, message_id="fixture-message", media_urls=[staged])
            await ainvoke_hook("pre_gateway_dispatch", event=event, gateway=None, session_store=None)
            await ainvoke_hook("post_gateway_admission", session_key="fixture-key", platform="telegram",
                               source=source.to_dict(), message_id="fixture-message", text="fixture")
            retained = capture_native_media([staged])[0]
            args = {"action": "files.upload", "task_id": "Q1", "upload": {"name": "input.bin", "local_path": retained["path"]}}
            def tool():
                return invoke_hook("pre_tool_call", tool_name="mcp__dots_duet__duet_prepare", args=args,
                                   task_id="fixture-task", tool_call_id="fixture-call")
            clear_session_vars([])
            if not any(row.get("action") == "block" for row in tool() if isinstance(row, dict)):
                raise RuntimeError("Unbound native call was not blocked")
            set_session_vars(platform="telegram", chat_id="fixture-chat", user_id="fixture-user",
                             session_key="fixture-key", session_id="fixture-session", message_id="fixture-message")
            rows = tool()
            expected = {**args, "_context_ref": "fixture-registration"}
            if not any(row.get("args") == expected for row in rows if isinstance(row, dict)):
                raise RuntimeError("Official lifecycle did not produce exact path-only modified arguments")
            if len(requests) != 1 or requests[0][0] != "/v1/invocations":
                raise RuntimeError("Trusted registration was not confined to its private HTTP route")
            private = requests[0][1]
            file = private["attachment_files"][0]
            if private["arguments"] != args or file["local_path"] != retained["path"] or base64.b64decode(file["file"]["data_base64"]) != data:
                raise RuntimeError("Private ingress bytes or original reference changed")
            if file["file"]["sha256"] != hashlib.sha256(data).hexdigest() or file["file"]["size"] != 19:
                raise RuntimeError("Private ingress hash or size differs")
            clear_session_vars([])
            report = {"official_plugin_three_hooks": True, "official_retained_media": True,
                      "actual_contextvar_binding": True, "unbound_rejected": True, "path_only_model_args": True,
                      "private_registration_size": 19, "real_hub": False, "model_run": False, "real_weixin_sent": False}
            if options.report:
                options.report.write_text(json.dumps(report, indent=2))
                options.report.chmod(0o600)
            print(json.dumps(report))
        finally:
            clear_session_vars([])
            server.shutdown()
            server.server_close()
            thread.join()


if __name__ == "__main__":
    parser = argparse.ArgumentParser()
    parser.add_argument("--hermes-source", required=True, type=Path)
    parser.add_argument("--report", type=Path)
    asyncio.run(smoke(parser.parse_args()))
