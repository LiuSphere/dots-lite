"""Hermes official pre_tool_call hook. No MCP SDK is imported in Hermes itself."""

import importlib

from .client import HubClient, HubError
from .attachments import InboundAttachments
from .schemas import TOOL_SCHEMAS


class SourcePlugin:
    def __init__(self, hub=None, session_context=None, media_roots=None):
        self.hub = hub
        self.session_context = session_context
        self.attachments = InboundAttachments(media_roots)

    def pre_gateway_dispatch(self, *, event, **hook):
        self.attachments.collect(event)

    def post_gateway_admission(self, *, session_key, source, message_id, **hook):
        self.attachments.admit(session_key, source, message_id)

    def pre_tool_call(self, *, tool_name="", args=None, **hook):
        if not tool_name.startswith("mcp__dots_duet__"):
            return None
        try:
            return self._bind_call(tool_name, args, hook)
        except Exception as error:
            # Hermes isolates callback exceptions. Return an explicit veto so a
            # failed source reader cannot become an ignored observer failure.
            return {"action": "block", "message": f"Trusted source registration failed ({type(error).__name__}): {error}"}

    def _bind_call(self, tool_name, args, hook):
        tool = tool_name.removeprefix("mcp__dots_duet__")
        if tool not in TOOL_SCHEMAS:
            return {"action": "block", "message": "Unknown dots-duet tool; trusted registration refused"}
        session = self.session_context or importlib.import_module("gateway.session_context")
        bound_reader = getattr(session, "bound_session_key", None)
        if not callable(bound_reader):
            return {"action": "block", "message": "Hermes lacks bound_session_key; trusted source binding is unavailable"}
        if not bound_reader():
            return {"action": "block", "message": "Current Hermes task has no bound session"}
        arguments = dict(args or {})
        arguments.pop("_context_ref", None)
        if any(name in arguments for name in ("source", "_source", "scope", "user_id", "profile_id", "executor_id")):
            return {"action": "block", "message": "Identity and source are not model-supplied duet arguments"}
        read = session.get_session_env
        platform = read("HERMES_SESSION_PLATFORM", "")
        source = {
            "kind": "api" if platform == "api_server" else "gateway",
            "platform": platform,
            "sender_id": read("HERMES_SESSION_USER_ID", ""),
            "chat_id": read("HERMES_SESSION_CHAT_ID", ""),
            "thread_id": read("HERMES_SESSION_THREAD_ID", ""),
            "session_id": read("HERMES_SESSION_ID", ""),
            "message_id": read("HERMES_SESSION_MESSAGE_ID", ""),
            "native_task_id": hook.get("task_id", ""),
            "tool_call_id": hook.get("tool_call_id", ""),
        }
        if not all(source[name] for name in ("platform", "chat_id", "session_id", "native_task_id", "tool_call_id")):
            return {"action": "block", "message": "Current Hermes source is incomplete; no previous source was reused"}
        if source["kind"] == "gateway" and (
            not source["sender_id"] or platform in {"cli", "desktop", "gateway", "kanban", "local", "tui", "webhook"}
        ):
            return {"action": "block", "message": "Current Hermes source is not an authenticated messaging origin"}
        try:
            hub = self.hub or HubClient.from_environment()
            private_files = []
            if tool == "duet_prepare":
                attachment_source = {
                    "platform": platform, "user_id": source["sender_id"], "chat_id": source["chat_id"],
                    "thread_id": source["thread_id"], "profile": read("HERMES_SESSION_PROFILE", ""),
                    "scope_id": read("HERMES_SESSION_SCOPE_ID", ""),
                    "parent_chat_id": read("HERMES_SESSION_PARENT_CHAT_ID", ""),
                }
                private_files = self.attachments.bind(bound_reader(), attachment_source, source["message_id"], arguments)
            registered = hub.request("POST", "/v1/invocations", {
                "tool": tool, "arguments": arguments, "source": source,
                **({"attachment_files": private_files} if private_files else {}),
            })
            context_ref = registered.get("context_ref", "")
            if not context_ref:
                raise HubError("Hub did not return a trusted context reference")
        except HubError as error:
            return {"action": "block", "message": str(error)}
        return {"action": "modify", "args": {**arguments, "_context_ref": context_ref}}


def register(ctx):
    plugin = SourcePlugin()
    ctx.register_hook("pre_gateway_dispatch", plugin.pre_gateway_dispatch)
    ctx.register_hook("post_gateway_admission", plugin.post_gateway_admission)
    ctx.register_hook("pre_tool_call", plugin.pre_tool_call)
