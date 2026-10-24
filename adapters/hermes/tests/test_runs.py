from contextlib import contextmanager
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import json
import threading
import unittest

from dots_lite_hermes.runs import RunsClient


class RunsTests(unittest.TestCase):
    def test_native_request_keeps_exact_session_body_and_idempotency_header(self):
        received = []

        class Handler(BaseHTTPRequestHandler):
            def do_POST(self):
                received.append((self.path, self.headers.get("Authorization"), self.headers.get("Idempotency-Key"),
                                 json.loads(self.rfile.read(int(self.headers["Content-Length"])))))
                self.send_response(202)
                self.end_headers()
                self.wfile.write(b'{"run_id":"run_test","status":"queued"}')

            def log_message(self, *args):
                pass

        with api_peer(Handler) as url:
            result = RunsClient(url, "test-only-token").request("POST", "/v1/runs", {
                "input": "check Q1", "session_id": "api-session-a",
            }, idempotency_key="followup-a")
        self.assertEqual(result["run_id"], "run_test")
        self.assertEqual(received, [("/v1/runs", "Bearer test-only-token", "followup-a", {
            "input": "check Q1", "session_id": "api-session-a",
        })])


@contextmanager
def api_peer(handler):
    server = ThreadingHTTPServer(("127.0.0.1", 0), handler)
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    try:
        yield f"http://127.0.0.1:{server.server_port}"
    finally:
        server.shutdown()
        server.server_close()
        thread.join()


if __name__ == "__main__":
    unittest.main()
