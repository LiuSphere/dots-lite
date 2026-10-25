import base64
import hashlib
from pathlib import Path
import tempfile
from types import SimpleNamespace
import unittest
from unittest.mock import patch

from dots_lite_hermes.plugin import SourcePlugin


class AttachmentTests(unittest.TestCase):
    def test_multiple_hard_links_cannot_be_admitted_as_message_media(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            cache = root / "documents"
            cache.mkdir()
            private = root / ".env"
            private.write_bytes(b"fixture only")
            linked = cache / "download.bin"
            linked.hardlink_to(private)
            source = {"platform": "weixin", "user_id": "user", "chat_id": "chat"}
            plugin = SourcePlugin(media_roots=lambda: (cache, cache, cache))
            event = SimpleNamespace(text="", source=SimpleNamespace(to_dict=lambda: source),
                                    message_id="message", media_urls=[str(linked)])
            plugin.pre_gateway_dispatch(event=event)
            with self.assertRaises(ValueError):
                plugin.post_gateway_admission(session_key="key", source=source, message_id="message")

    def test_message_candidate_and_admitted_registration_expire_after_five_minutes(self):
        with tempfile.TemporaryDirectory() as directory:
            cache = Path(directory)
            path = cache / "input.bin"
            path.write_bytes(b"")
            source = {"platform": "weixin", "user_id": "user", "chat_id": "chat"}
            values = {"HERMES_SESSION_PLATFORM": "weixin", "HERMES_SESSION_USER_ID": "user",
                      "HERMES_SESSION_CHAT_ID": "chat", "HERMES_SESSION_ID": "session",
                      "HERMES_SESSION_MESSAGE_ID": "message"}
            session = SimpleNamespace(bound_session_key=lambda: "key",
                                      get_session_env=lambda name, default="": values.get(name, default))
            class Hub:
                def request(self, *args):
                    return {"context_ref": "valid"}
            for expire_at_admission in (False, True):
                with self.subTest(expire_at_admission=expire_at_admission):
                    plugin = SourcePlugin(hub=Hub(), session_context=session,
                                          media_roots=lambda: (cache, cache, cache))
                    event = SimpleNamespace(text="", source=SimpleNamespace(to_dict=lambda: source),
                                            message_id="message", media_urls=[str(path)])
                    with patch("time.monotonic", return_value=100):
                        plugin.pre_gateway_dispatch(event=event)
                    with patch("time.monotonic", return_value=401 if expire_at_admission else 100):
                        plugin.post_gateway_admission(session_key="key", source=source, message_id="message")
                    args = {"action": "files.upload", "task_id": "Q1", "upload": {"name": "empty.bin", "local_path": str(path)}}
                    if not expire_at_admission:
                        with patch("time.monotonic", return_value=100):
                            valid = plugin.pre_tool_call(tool_name="mcp__dots_lite__dots_lite_prepare", args=args,
                                                         task_id="native", tool_call_id="valid")
                        self.assertEqual(valid["action"], "modify", valid)
                    with patch("time.monotonic", return_value=401):
                        denied = plugin.pre_tool_call(tool_name="mcp__dots_lite__dots_lite_prepare", args=args,
                                                      task_id="native", tool_call_id="call")
                    self.assertEqual(denied["action"], "block", denied)

    def test_admitted_message_registers_bytes_privately_and_preserves_model_reference(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            cache = root / "documents"
            cache.mkdir()
            path = cache / "doc_inbound.bin"
            path.write_bytes(b"\x00\xffinbound-data\x80")
            source = {"platform": "weixin", "user_id": "sender-a", "chat_id": "chat-a",
                      "thread_id": "", "profile": "private"}
            values = {"HERMES_SESSION_PLATFORM": "weixin", "HERMES_SESSION_USER_ID": "sender-a",
                      "HERMES_SESSION_CHAT_ID": "chat-a", "HERMES_SESSION_THREAD_ID": "",
                      "HERMES_SESSION_PROFILE": "private", "HERMES_SESSION_MESSAGE_ID": "message-a",
                      "HERMES_SESSION_ID": "session-a"}
            session = SimpleNamespace(bound_session_key=lambda: "key-a",
                                      get_session_env=lambda name, default="": values.get(name, default))
            class Hub:
                def request(self, method, route, body):
                    self.body = body
                    return {"context_ref": "registered"}
            hub = Hub()
            plugin = SourcePlugin(hub=hub, session_context=session,
                                  media_roots=lambda: (cache, cache, cache))
            event = SimpleNamespace(text="", source=SimpleNamespace(to_dict=lambda: dict(source)),
                                    message_id="message-a", media_urls=[str(path)])
            plugin.pre_gateway_dispatch(event=event)
            plugin.post_gateway_admission(session_key="key-a", platform="weixin", source=source,
                                          message_id="message-a")
            # Authority's retained alias is the path shown to the model on execution.
            reference = str(cache / "native-inputs" / hashlib.sha256(path.read_bytes()).hexdigest() / path.name)
            args = {"action": "task.create", "folder_id": "demo", "input": "Read the attachment",
                    "inputs": [{"name": "input.bin", "local_path": reference}]}
            result = plugin.pre_tool_call(tool_name="mcp__dots_lite__dots_lite_prepare", args=args,
                                          task_id="native-a", tool_call_id="call-a")
            self.assertEqual(result["action"], "modify", result)
            self.assertEqual(result["args"], {**args, "_context_ref": "registered"})
            self.assertEqual(hub.body["arguments"], args)
            self.assertEqual(hub.body["attachment_files"], [{"local_path": reference, "file": {
                "name": "input.bin", "data_base64": base64.b64encode(b"\x00\xffinbound-data\x80").decode(),
                "sha256": hashlib.sha256(b"\x00\xffinbound-data\x80").hexdigest(), "size": 15,
            }}])
            # A later disk write cannot replace the already admitted byte snapshot.
            path.write_bytes(b"changed-after-admission")
            upload = {"action": "files.upload", "task_id": "Q1",
                      "upload": {"name": "later.bin", "local_path": str(path)}}
            result = plugin.pre_tool_call(tool_name="mcp__dots_lite__dots_lite_prepare", args=upload,
                                          task_id="native-a", tool_call_id="call-b")
            self.assertEqual(result["action"], "modify", result)
            self.assertEqual(base64.b64decode(hub.body["attachment_files"][0]["file"]["data_base64"]), b"\x00\xffinbound-data\x80")
            for field, value in (("HERMES_SESSION_CHAT_ID", "chat-other"),
                                 ("HERMES_SESSION_MESSAGE_ID", "message-other"),
                                 ("HERMES_SESSION_PROFILE", "another-profile"),
                                 ("HERMES_SESSION_USER_ID", "other-sender"),
                                 ("HERMES_SESSION_THREAD_ID", "another-thread")):
                with self.subTest(field=field):
                    original = values[field]
                    values[field] = value
                    denied = plugin.pre_tool_call(tool_name="mcp__dots_lite__dots_lite_prepare", args=upload,
                                                  task_id="native-a", tool_call_id="denied")
                    values[field] = original
                    self.assertEqual(denied["action"], "block", denied)
            for reference in (str(root / ".env"), str(cache / "other.bin")):
                denied = plugin.pre_tool_call(tool_name="mcp__dots_lite__dots_lite_prepare",
                                              args={**upload, "upload": {"name": "x.bin", "local_path": reference}},
                                              task_id="native-a", tool_call_id="denied")
                self.assertEqual(denied["action"], "block", denied)
            restarted = SourcePlugin(hub=hub, session_context=session, media_roots=lambda: (cache, cache, cache))
            denied = restarted.pre_tool_call(tool_name="mcp__dots_lite__dots_lite_prepare", args=upload,
                                            task_id="native-a", tool_call_id="denied")
            self.assertEqual(denied["action"], "block", denied)

    def test_pre_admission_candidate_cannot_authorize_a_different_message(self):
        with tempfile.TemporaryDirectory() as directory:
            cache = Path(directory)
            path = cache / "file.bin"
            path.write_bytes(b"trusted input")
            source = {"platform": "weixin", "user_id": "user", "chat_id": "chat"}
            values = {"HERMES_SESSION_PLATFORM": "weixin", "HERMES_SESSION_USER_ID": "user",
                      "HERMES_SESSION_CHAT_ID": "chat", "HERMES_SESSION_ID": "session",
                      "HERMES_SESSION_MESSAGE_ID": "wrong-message"}
            session = SimpleNamespace(bound_session_key=lambda: "key",
                                      get_session_env=lambda name, default="": values.get(name, default))
            plugin = SourcePlugin(session_context=session, media_roots=lambda: (cache, cache, cache))
            event = SimpleNamespace(text="", source=SimpleNamespace(to_dict=lambda: source),
                                    message_id="original-message", media_urls=[str(path)])
            plugin.pre_gateway_dispatch(event=event)
            plugin.post_gateway_admission(session_key="key", source=source, message_id="wrong-message")
            args = {"action": "files.upload", "task_id": "Q1", "upload": {"name": "file.bin", "local_path": str(path)}}
            denied = plugin.pre_tool_call(tool_name="mcp__dots_lite__dots_lite_prepare", args=args,
                                          task_id="native", tool_call_id="call")
            self.assertEqual(denied["action"], "block", denied)

    @unittest.skipUnless(__import__("os").name == "posix", "POSIX no-follow handles")
    def test_symlink_candidate_cannot_read_private_file(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            cache = root / "documents"
            cache.mkdir()
            private = root / ".env"
            private.write_bytes(b"fixture only")
            link = cache / "image.png"
            link.symlink_to(private)
            source = {"platform": "weixin", "user_id": "user", "chat_id": "chat"}
            plugin = SourcePlugin(media_roots=lambda: (cache, cache, cache))
            event = SimpleNamespace(text="", source=SimpleNamespace(to_dict=lambda: source),
                                    message_id="message", media_urls=[str(link)])
            plugin.pre_gateway_dispatch(event=event)
            with self.assertRaises(OSError):
                plugin.post_gateway_admission(session_key="key", source=source, message_id="message")


if __name__ == "__main__":
    unittest.main()
