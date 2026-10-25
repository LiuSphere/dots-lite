import unittest
from types import SimpleNamespace
from unittest.mock import patch

from dots_lite_hermes.selections import InboundSelections
from dots_lite_hermes.plugin import SourcePlugin


class SelectionTests(unittest.TestCase):
    def test_selection_is_bound_to_exact_admitted_message_and_full_path(self):
        selections = InboundSelections()
        source = {"platform": "weixin", "user_id": "u", "chat_id": "c"}
        event = SimpleNamespace(source=SimpleNamespace(to_dict=lambda: source), message_id="m", text="请在 `C:/projects/demo` 修复", media_urls=[])
        selections.collect(event)
        args = {"action": "task.create", "working_directory": "C:/projects/demo"}
        self.assertIsNone(selections.bind("s", source, "m", args))
        selections.admit("s", source, "m")
        self.assertEqual(selections.bind("s", source, "m", args), {"path": "C:/projects/demo", "message_id": "m"})
        self.assertIsNone(selections.bind("s", source, "other", args))
        self.assertIsNone(selections.bind("other", source, "m", args))
        self.assertIsNone(selections.bind("s", {**source, "user_id": "other"}, "m", args))
        self.assertIsNone(selections.bind("s", source, "m", {**args, "working_directory": "C:/projects"}))
        with patch("dots_lite_hermes.selections.time.monotonic", return_value=10**12):
            self.assertIsNone(selections.bind("s", source, "m", args))

    def test_plugin_registers_directory_selection_privately(self):
        source = {"platform": "weixin", "user_id": "u", "chat_id": "c"}
        values = {"HERMES_SESSION_PLATFORM": "weixin", "HERMES_SESSION_USER_ID": "u", "HERMES_SESSION_CHAT_ID": "c", "HERMES_SESSION_ID": "s", "HERMES_SESSION_MESSAGE_ID": "m"}
        session = SimpleNamespace(bound_session_key=lambda: "key", get_session_env=lambda name, default="": values.get(name, default))

        class Hub:
            def request(self, method, path, payload):
                self.payload = payload
                return {"context_ref": "ref"}

        hub = Hub()
        plugin = SourcePlugin(hub=hub, session_context=session)
        plugin.pre_gateway_dispatch(event=SimpleNamespace(source=SimpleNamespace(to_dict=lambda: source), message_id="m", text="继续会话 `native-1`", media_urls=[]))
        plugin.post_gateway_admission(session_key="key", source=source, message_id="m")
        result = plugin.pre_tool_call(tool_name="mcp__dots_lite__dots_lite_prepare", args={"action": "task.attach", "thread_id": "native-1", "input": "继续"}, task_id="native-run", tool_call_id="tool")
        self.assertEqual(result["action"], "modify")
        self.assertEqual(hub.payload["directory_selection"], {"thread_id": "native-1", "message_id": "m"})
        self.assertNotIn("directory_selection", result["args"])


if __name__ == "__main__":
    unittest.main()
