"""Bind task directory selections to an admitted message, not model fields."""

from collections import OrderedDict
import threading
import re
import time

from .attachments import route


class InboundSelections:
    def __init__(self):
        self.candidates = OrderedDict()
        self.admitted = OrderedDict()
        self.lock = threading.Lock()

    def collect(self, event):
        if not event.message_id:
            return
        with self.lock:
            self.candidates[(route(event.source.to_dict()), event.message_id)] = (str(event.text or ""), time.monotonic())
            if len(self.candidates) > 128:
                self.candidates.popitem(last=False)

    def admit(self, session_key, source, message_id):
        key = (route(source), message_id)
        with self.lock:
            candidate = self.candidates.pop(key, None)
            self.admitted.pop(session_key, None)
            if candidate and time.monotonic() - candidate[1] < 300:
                self.admitted[session_key] = (key, *candidate)
                if len(self.admitted) > 16:
                    self.admitted.popitem(last=False)

    def bind(self, session_key, source, message_id, arguments):
        action = arguments.get("action")
        if action not in {"task.create", "task.attach"}:
            return None
        value = arguments.get("thread_id" if action == "task.attach" else "working_directory", "")
        if not value:
            return None
        with self.lock:
            admitted = self.admitted.get(session_key)
        if admitted is None or admitted[0] != (route(source), message_id) or time.monotonic() - admitted[2] >= 300:
            return None
        # Hermes interprets the user's intent. This exact-message check prevents
        # an invented path or another session's choice becoming authorization.
        if not re.search(r"(?<![\w./\\:-])" + re.escape(value) + r"(?![\w./\\:-])", admitted[1]):
            return None
        return {"thread_id" if action == "task.attach" else "path": value, "message_id": message_id}
