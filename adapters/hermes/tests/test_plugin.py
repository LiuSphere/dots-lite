import os
import unittest
from types import SimpleNamespace
from unittest.mock import patch

from dots_duet_lite_hermes.plugin import SourcePlugin


class PluginTests(unittest.TestCase):
    def test_missing_native_bound_reader_blocks_even_with_stale_environment(self):
        def forbidden_read(name, default=""):
            raise AssertionError("unbound native context must not fall back to environment")

        session = SimpleNamespace(get_session_env=forbidden_read)
        plugin = SourcePlugin(hub=None, session_context=session)
        with patch.dict(os.environ, {
            "HERMES_SESSION_KEY": "old-session", "HERMES_SESSION_CHAT_ID": "old-chat",
        }):
            result = plugin.pre_tool_call(
                tool_name="mcp__dots_duet__duet_query", args={"query": "tasks"},
                task_id="task-1", session_id="session-1", tool_call_id="call-1",
            )
        self.assertEqual(result["action"], "block")
        self.assertIn("bound_session_key", result["message"])

    def test_bound_call_overwrites_forged_reference_with_current_native_source(self):
        values = {
            "HERMES_SESSION_PLATFORM": "weixin", "HERMES_SESSION_CHAT_ID": "chat-a",
            "HERMES_SESSION_USER_ID": "sender-a", "HERMES_SESSION_ID": "session-a",
            "HERMES_SESSION_THREAD_ID": "thread-a", "HERMES_SESSION_MESSAGE_ID": "message-a",
        }
        session = SimpleNamespace(
            bound_session_key=lambda: "native-key-a",
            get_session_env=lambda name, default="": values.get(name, default),
        )

        class Hub:
            def request(self, method, path, payload):
                self.registered = (method, path, payload)
                return {"context_ref": "trusted-reference"}

        hub = Hub()
        plugin = SourcePlugin(hub=hub, session_context=session)
        result = plugin.pre_tool_call(
            tool_name="mcp__dots_duet__duet_query",
            args={"query": "tasks", "_context_ref": "forged-reference"},
            task_id="native-task-a", session_id="session-a", tool_call_id="call-a",
        )
        self.assertEqual(result["action"], "modify")
        self.assertEqual(result["args"]["_context_ref"], "trusted-reference")
        method, path, payload = hub.registered
        self.assertEqual((method, path), ("POST", "/v1/invocations"))
        self.assertEqual(payload["tool"], "duet_query")
        self.assertEqual(payload["arguments"], {"query": "tasks"})
        self.assertEqual(payload["source"]["sender_id"], "sender-a")
        self.assertEqual(payload["source"]["chat_id"], "chat-a")
        self.assertEqual(payload["source"]["tool_call_id"], "call-a")

    def test_native_source_failure_returns_a_block_directive(self):
        def unavailable(name, default=""):
            raise RuntimeError("native source is unavailable")

        plugin = SourcePlugin(hub=None, session_context=SimpleNamespace(
            bound_session_key=lambda: "native-key", get_session_env=unavailable,
        ))
        result = plugin.pre_tool_call(
            tool_name="mcp__dots_duet__duet_query",
            args={"query": "tasks", "_context_ref": "stale-reference"},
            task_id="task-a", session_id="session-a", tool_call_id="call-a",
        )
        self.assertEqual(result["action"], "block")
        self.assertIn("unavailable", result["message"])


if __name__ == "__main__":
    unittest.main()
