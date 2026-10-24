"""Thin HTTP client for Hermes' existing durable run interface."""

import json
from pathlib import Path
import os
import stat
from urllib.error import HTTPError, URLError
from urllib.parse import urlsplit
from urllib.request import HTTPRedirectHandler, Request, build_opener


class RunsError(RuntimeError):
    pass


class _NoRedirect(HTTPRedirectHandler):
    def redirect_request(self, *args, **kwargs):
        return None


class RunsClient:
    def __init__(self, base_url, token, timeout=30):
        parsed = urlsplit(base_url)
        if parsed.scheme not in {"http", "https"} or not parsed.hostname or parsed.username or parsed.password or parsed.query or parsed.fragment:
            raise ValueError("Hermes API URL must be an explicitly configured HTTP(S) endpoint without credentials or query")
        if not token:
            raise ValueError("Hermes API bearer token is required")
        self.base_url = base_url.rstrip("/")
        self.token = token
        self.timeout = timeout
        self.opener = build_opener(_NoRedirect())

    @classmethod
    def from_token_file(cls, base_url, token_file):
        path = Path(token_file)
        if os.name == "posix" and stat.S_IMODE(path.stat().st_mode) & 0o077:
            raise PermissionError("Hermes API token file must be private (mode 600)")
        return cls(base_url, path.read_text().strip())

    def request(self, method, path, payload=None, idempotency_key=None):
        headers = {"Authorization": "Bearer " + self.token, "Content-Type": "application/json"}
        if idempotency_key:
            headers["Idempotency-Key"] = idempotency_key
        data = None if payload is None else json.dumps(
            payload, ensure_ascii=False, allow_nan=False, sort_keys=True, separators=(",", ":"),
        ).encode("utf-8")
        request = Request(self.base_url + path, data=data, headers=headers, method=method)
        try:
            with self.opener.open(request, timeout=self.timeout) as response:
                raw = response.read(1024 * 1024 + 1)
                if len(raw) > 1024 * 1024:
                    raise RunsError("Hermes API response exceeds the bounded status size")
                return json.loads(raw)
        except HTTPError as error:
            raise RunsError(f"Hermes API rejected request with HTTP {error.code}; no replacement run was submitted") from error
        except (URLError, OSError, ValueError) as error:
            raise RunsError(f"Hermes API result is unavailable or unknown: {error}") from error
