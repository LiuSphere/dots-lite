"""UID-authenticated local HTTP transport. No TCP fallback is provided."""

import http.client
import json
import os
import socket


class HubError(RuntimeError):
    pass


class _UnixConnection(http.client.HTTPConnection):
    def __init__(self, path, timeout):
        super().__init__("localhost", timeout=timeout)
        self.path = path

    def connect(self):
        if not hasattr(socket, "AF_UNIX"):
            raise HubError("Hub Unix socket transport requires Linux")
        self.sock = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        self.sock.settimeout(self.timeout)
        self.sock.connect(self.path)


class HubClient:
    def __init__(self, socket_path, timeout=15):
        if not socket_path:
            raise HubError("DOTS_LITE_SOCKET must name the profile's authorized hub socket")
        self.socket_path = socket_path
        self.timeout = timeout

    @classmethod
    def from_environment(cls):
        return cls(os.environ.get("DOTS_LITE_SOCKET", ""))

    def request(self, method, path, payload=None):
        body = None if payload is None else json.dumps(
            payload, ensure_ascii=False, allow_nan=False, separators=(",", ":"),
        ).encode("utf-8")
        connection = _UnixConnection(self.socket_path, self.timeout)
        try:
            connection.request(method, path, body=body, headers={"Content-Type": "application/json"})
            response = connection.getresponse()
            raw = response.read(16 * 1024 * 1024 + 1)
            if len(raw) > 16 * 1024 * 1024:
                raise HubError("Hub response exceeds the transfer limit; no partial result was accepted")
            try:
                data = json.loads(raw)
            except (ValueError, UnicodeError) as error:
                raise HubError(f"Hub returned invalid JSON (HTTP {response.status})") from error
            if response.status >= 400:
                detail = data.get("error", {}) if isinstance(data, dict) else {}
                raise HubError(f"Hub HTTP {response.status}: {detail.get('code', 'error')}: {detail.get('message', 'request rejected')}")
            return data
        except (OSError, http.client.HTTPException) as error:
            raise HubError(f"Hub request failed; its execution result may be unknown: {error}") from error
        finally:
            connection.close()
