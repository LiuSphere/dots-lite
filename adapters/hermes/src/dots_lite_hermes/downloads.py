"""Acquire authorized bytes in this profile's private directory."""

import base64
import binascii
import hashlib
import os
from pathlib import Path, PurePosixPath, PureWindowsPath
import re
import stat
import uuid


def materialize_download(payload):
    try:
        data = base64.b64decode(payload["data_base64"], validate=True)
    except (KeyError, ValueError, binascii.Error) as error:
        raise ValueError("Download did not contain valid complete Base64 bytes") from error
    digest = hashlib.sha256(data).hexdigest()
    size, offset = payload.get("size"), payload.get("offset")
    if digest != payload.get("sha256") or len(data) != payload.get("returned_bytes"):
        raise ValueError("Download hash or returned byte count does not match; no file was acquired")
    if not isinstance(size, int) or not isinstance(offset, int) or offset < 0 or size < offset + len(data):
        raise ValueError("Download range metadata is invalid; no file was acquired")
    if size > 8 * 1024 * 1024:
        raise ValueError("Download exceeds the 8 MiB file limit")
    home = Path(os.environ.get("HERMES_HOME", Path.home() / ".hermes"))
    root = Path(os.environ.get("DOTS_LITE_FILES_DIR", home / "dots-lite" / "files"))
    root.mkdir(mode=0o700, parents=True, exist_ok=True)
    if os.name == "posix" and stat.S_IMODE(root.stat().st_mode) & 0o077:
        raise PermissionError("Download directory must be private (mode 700)")
    original = payload.get("path", "")
    suffix = (PureWindowsPath(original) if "\\" in original else PurePosixPath(original)).suffix
    if not re.fullmatch(r"\.[A-Za-z0-9]{1,12}", suffix):
        suffix = ".bin"
    target = root.resolve() / (uuid.uuid4().hex + suffix)
    descriptor = os.open(target, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
    try:
        with os.fdopen(descriptor, "wb") as output:
            output.write(data)
            output.flush()
            os.fsync(output.fileno())
        if hashlib.sha256(target.read_bytes()).hexdigest() != digest:
            raise OSError("Private file hash differs after writing")
    except BaseException:
        target.unlink(missing_ok=True)
        raise
    result = {key: value for key, value in payload.items() if key != "data_base64"}
    result["local_path"] = str(target)
    result["acquired"] = "complete" if offset == 0 and len(data) == size and payload.get("truncated") is False else "partial"
    return result
