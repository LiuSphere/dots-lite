import os
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

from dots_lite_hermes.worker import FollowupWorker


class WorkerTests(unittest.TestCase):
    def test_automatic_artifact_is_acquired_before_any_model_followup(self):
        class Hub:
            def __init__(self):
                self.receipts = []

            def request(self, method, path, payload=None):
                if path == "/v1/artifacts/next":
                    return {"id": "artifact-a", "task_id": "Q1", "file": {
                        "path": "out/result.bin", "data_base64": "YQ==",
                        "sha256": "ca978112ca1bbdcafac231b39a23dc4da786eff8147c4e72b9807785afee48bb",
                        "size": 1, "returned_bytes": 1, "offset": 0, "truncated": False,
                    }}
                if path != "/v1/artifacts/artifact-a/acquired":
                    raise AssertionError("pending artifact must be acquired before polling model followups")
                self.receipts.append(payload)
                return {"ok": True}

        class Native:
            def request(self, *args, **kwargs):
                raise AssertionError("automatic file transfer does not require a native model run")

        hub = Hub()
        with tempfile.TemporaryDirectory() as files:
            with patch.dict(os.environ, {"DOTS_LITE_FILES_DIR": files}):
                self.assertTrue(FollowupWorker(hub, Native()).step())
            receipt = hub.receipts[0]
            self.assertTrue(receipt["acquired"])
            self.assertNotIn("data_base64", receipt)
            self.assertEqual(Path(receipt["local_path"]).read_bytes(), b"a")
            self.assertEqual(receipt["sha256"], "ca978112ca1bbdcafac231b39a23dc4da786eff8147c4e72b9807785afee48bb")

    def test_corrupt_automatic_artifact_records_transfer_failure_without_model_run(self):
        class Hub:
            def __init__(self):
                self.failures = []

            def request(self, method, path, payload=None):
                if path == "/v1/artifacts/next":
                    return {"id": "artifact-a", "file": {
                        "path": "result.bin", "data_base64": "YQ==", "sha256": "0" * 64,
                        "size": 1, "returned_bytes": 1, "offset": 0, "truncated": False,
                    }}
                self.failures.append(payload)
                return {"ok": True}

        class Native:
            def request(self, *args, **kwargs):
                raise AssertionError("a transfer failure must not repeat the original task")

        hub = Hub()
        with tempfile.TemporaryDirectory() as files:
            with patch.dict(os.environ, {"DOTS_LITE_FILES_DIR": files}):
                with self.assertRaisesRegex(ValueError, "hash"):
                    FollowupWorker(hub, Native()).step()
            self.assertEqual(list(Path(files).iterdir()), [])
        self.assertEqual(len(hub.failures), 1)
        self.assertFalse(hub.failures[0]["acquired"])
        self.assertIn("hash", hub.failures[0]["error"])

    def test_non_durable_native_runs_are_not_submitted(self):
        class Hub:
            def request(self, method, path, payload=None):
                if path == "/v1/artifacts/next":
                    return None
                return {
                    "id": "F1", "session_id": "api-session-a", "idempotency_key": "followup-a",
                    "request": {"input": "Check task Q1", "session_id": "api-session-a"},
                }

        class Native:
            def request(self, method, path, payload=None, idempotency_key=None):
                if method != "GET" or path != "/v1/capabilities":
                    raise AssertionError("non-durable native API must not receive a model run")
                return {"features": {"runs_idempotency": {"supported": True, "durable": False}}}

        with self.assertRaises(RuntimeError):
            FollowupWorker(Hub(), Native()).step()

    def test_known_native_run_is_polled_without_starting_a_second_run(self):
        followup = {
            "id": "F1", "session_id": "api-session-a", "idempotency_key": "followup-a",
            "request": {"input": "Check task Q1", "session_id": "api-session-a"},
        }

        class Hub:
            def __init__(self):
                self.reports = []

            def request(self, method, path, payload=None):
                if path == "/v1/artifacts/next":
                    return None
                if method == "GET":
                    return dict(followup)
                if path.endswith("/run"):
                    followup["run_id"] = payload["run_id"]
                else:
                    self.reports.append(payload)
                return {"ok": True}

        class Native:
            def __init__(self):
                self.submissions = []

            def request(self, method, path, payload=None, idempotency_key=None):
                if path == "/v1/capabilities":
                    return {"features": {"runs_idempotency": {"supported": True, "durable": True}}}
                if method == "POST":
                    self.submissions.append((payload, idempotency_key))
                    return {"run_id": "run_a", "status": "queued"}
                return {"run_id": "run_a", "session_id": "api-session-a", "status": "completed", "output": "checked"}

        hub, native = Hub(), Native()
        worker = FollowupWorker(hub, native)
        self.assertTrue(worker.step())
        self.assertTrue(worker.step())
        self.assertEqual(native.submissions, [({"input": "Check task Q1", "session_id": "api-session-a"}, "followup-a")])
        self.assertEqual(len(hub.reports), 2)
        self.assertEqual(hub.reports[0]["status"], "completed")
        self.assertNotIn("handled", hub.reports[0])

    def test_lost_native_post_result_reuses_same_key_and_body_without_another_run(self):
        followup = {
            "id": "F1", "session_id": "api-session-a", "idempotency_key": "followup-a",
            "request": {"input": "Check task Q1", "session_id": "api-session-a"},
        }

        class Hub:
            def request(self, method, path, payload=None):
                if path == "/v1/artifacts/next":
                    return None
                if method == "GET":
                    return dict(followup)
                if path.endswith("/run"):
                    followup["run_id"] = payload["run_id"]
                return {"ok": True}

        class Native:
            def __init__(self):
                self.runs = {}
                self.attempts = []

            def request(self, method, path, payload=None, idempotency_key=None):
                if path == "/v1/capabilities":
                    return {"features": {"runs_idempotency": {"supported": True, "durable": True}}}
                if method == "POST":
                    self.attempts.append((dict(payload), idempotency_key))
                    if idempotency_key not in self.runs:
                        self.runs[idempotency_key] = dict(payload)
                        raise OSError("response lost after native admission")
                    if self.runs[idempotency_key] != payload:
                        raise AssertionError("idempotency key was reused for different input")
                    return {"run_id": "run_a", "status": "queued"}
                return {"run_id": "run_a", "session_id": "api-session-a", "status": "completed"}

        native = Native()
        worker = FollowupWorker(Hub(), native)
        with self.assertRaises(OSError):
            worker.step()
        self.assertTrue(worker.step())
        self.assertEqual(len(native.runs), 1)
        self.assertEqual(native.attempts, [
            ({"input": "Check task Q1", "session_id": "api-session-a"}, "followup-a"),
            ({"input": "Check task Q1", "session_id": "api-session-a"}, "followup-a"),
        ])

    def test_native_run_in_another_session_is_never_bound_or_reported(self):
        class Hub:
            def request(self, method, path, payload=None):
                if path == "/v1/artifacts/next":
                    return None
                if method != "GET":
                    raise AssertionError("unverified native session must not change Hub state")
                return {"id": "F1", "run_id": "run_a", "session_id": "api-session-a"}

        class Native:
            def request(self, method, path, payload=None, idempotency_key=None):
                return {"run_id": "run_a", "session_id": "unrelated-session", "status": "completed"}

        with self.assertRaisesRegex(RuntimeError, "binding changed"):
            FollowupWorker(Hub(), Native()).step()


if __name__ == "__main__":
    unittest.main()
