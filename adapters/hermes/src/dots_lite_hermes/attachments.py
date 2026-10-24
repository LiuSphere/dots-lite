"""Per-message ingress snapshots. Bytes never enter model-visible arguments."""

import base64
from collections import OrderedDict
import hashlib
import os
from pathlib import Path
import stat
import threading
import time


LIMIT = 8 * 1024 * 1024
ROUTE_FIELDS = ("platform", "user_id", "chat_id", "thread_id", "profile", "scope_id", "parent_chat_id")


def native_media_roots():
    from gateway.platforms.base import get_audio_cache_dir, get_document_cache_dir, get_image_cache_dir
    return get_image_cache_dir(), get_audio_cache_dir(), get_document_cache_dir()


def route(source):
    return tuple(source.get(name) or "" for name in ROUTE_FIELDS)


def read_candidate(value, roots):
    path = Path(value)
    if not path.is_absolute() or ".." in path.parts or not any(path.is_relative_to(root) for root in roots):
        raise ValueError("Attachment is outside native media caches")
    # Traverse by handles: replacing a parent with a symlink cannot redirect a read.
    if os.name == "posix":
        descriptor = os.open(path.anchor, os.O_RDONLY | os.O_DIRECTORY)
        try:
            for part in path.parts[1:-1]:
                child = os.open(part, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW, dir_fd=descriptor)
                os.close(descriptor)
                descriptor = child
            opened = os.open(path.name, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK, dir_fd=descriptor)
        finally:
            os.close(descriptor)
    else:
        # Production is POSIX. Windows integration still rejects visible links.
        if any(item.is_symlink() for item in (path, *path.parents)):
            raise ValueError("Attachment symlinks are forbidden")
        opened = os.open(path, os.O_RDONLY | os.O_BINARY)
    with os.fdopen(opened, "rb") as stream:
        before = os.fstat(stream.fileno())
        if not stat.S_ISREG(before.st_mode) or before.st_size > LIMIT or before.st_nlink != 1:
            raise ValueError("Attachment must be a single-link regular file of at most 8 MiB")
        if os.name == "posix" and before.st_uid != os.geteuid():
            raise ValueError("Attachment belongs to a different operating-system identity")
        data = stream.read(LIMIT + 1)
        after = os.fstat(stream.fileno())
        identity = lambda info: (info.st_dev, info.st_ino, info.st_size, info.st_mtime_ns, info.st_ctime_ns, info.st_nlink)
        if len(data) > LIMIT or len(data) != before.st_size or identity(before) != identity(after):
            raise ValueError("Attachment changed while it was read")
    return data


class InboundAttachments:
    def __init__(self, roots=None):
        self.roots = roots or native_media_roots
        self.candidates = OrderedDict()
        self.admitted = OrderedDict()
        self.lock = threading.Lock()

    def collect(self, event):
        source = event.source.to_dict()
        message = event.message_id
        if not message or not event.media_urls:
            return
        roots = tuple(Path(value).absolute() for value in self.roots())
        with self.lock:
            self.candidates[(route(source), message)] = (tuple(event.media_urls), roots, time.monotonic())
            if len(self.candidates) > 128:
                self.candidates.popitem(last=False)

    def admit(self, session_key, source, message):
        key = (route(source), message)
        with self.lock:
            candidate = self.candidates.pop(key, None)
            self.admitted.pop(session_key, None)
        if candidate is None:
            return
        values, roots, created = candidate
        if time.monotonic() - created >= 300:
            return
        retained = Path(self.roots()[2]).absolute() / "native-inputs"
        files = {}
        total = 0
        for value in values:
            data = read_candidate(value, roots)
            total += len(data)
            if total > LIMIT:
                raise ValueError("Combined message attachments exceed 8 MiB")
            digest = hashlib.sha256(data).hexdigest()
            snapshot = {"data_base64": base64.b64encode(data).decode("ascii"), "sha256": digest, "size": len(data)}
            files[str(value)] = snapshot
            files[str(retained / digest / Path(value).name)] = snapshot
        with self.lock:
            self.admitted[session_key] = (key, files, created)
            # This is a process-local authorization cache. Eviction fails closed.
            if len(self.admitted) > 16:
                self.admitted.popitem(last=False)

    def bind(self, session_key, source, message, arguments):
        refs = arguments.get("inputs", [])
        upload = arguments.get("upload")
        if upload is not None:
            refs = [*refs, upload]
        if not refs:
            return []
        if arguments.get("inputs") and arguments.get("action") != "task.create":
            raise ValueError("inputs are supported only for task.create")
        if upload is not None and arguments.get("action") != "files.upload":
            raise ValueError("upload is supported only for files.upload")
        with self.lock:
            authorized = self.admitted.get(session_key)
        if authorized is None or authorized[0] != (route(source), message):
            raise ValueError("No admitted attachments belong to this exact source and message")
        if time.monotonic() - authorized[2] >= 300:
            with self.lock:
                self.admitted.pop(session_key, None)
            raise ValueError("Admitted attachment registration expired after five minutes")
        result = []
        total = 0
        names = set()
        for ref in refs:
            if not isinstance(ref, dict) or set(ref) != {"name", "local_path"}:
                raise ValueError("Attachments require only name and local_path references")
            name, path = ref["name"], ref["local_path"]
            if not isinstance(name, str) or not name or name in {".", ".."} or any(ch in name for ch in "/\\\x00") or name in names:
                raise ValueError("Attachment names must be distinct plain file names")
            names.add(name)
            snapshot = authorized[1].get(path)
            if snapshot is None:
                raise ValueError("Attachment path was not admitted with this message")
            total += snapshot["size"]
            if total > LIMIT:
                raise ValueError("Combined input attachments exceed 8 MiB")
            result.append({"local_path": path, "file": {"name": name, **snapshot}})
        return result
