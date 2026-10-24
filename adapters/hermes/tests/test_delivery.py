import unittest
import subprocess
import sys
import tempfile
from unittest.mock import patch

from dots_duet_lite_hermes.worker import FollowupWorker


class DeliveryWorkerTests(unittest.TestCase):
    def test_lost_report_does_not_cause_the_worker_to_repeat_a_dispatch(self):
        class Hub:
            pending = True

            def request(self, method, path, payload=None):
                if path == "/v1/artifacts/next" or path == "/v1/followups/next":
                    return None
                if path == "/v1/deliveries/next":
                    if not self.pending:
                        return None
                    self.pending = False  # Real Hub persists dispatching before returning this unit.
                    return {"id": "D1", "attempt_id": "A1"}
                raise OSError("report response lost after possible commit")

        class Sender:
            def __init__(self):
                self.sent = []

            def send(self, delivery):
                self.sent.append(delivery["id"])
                return {"status": "delivered", "receipt": {"success": True, "message_id": "M1"}}

        sender = Sender()
        worker = FollowupWorker(Hub(), None, sender)
        with self.assertRaises(OSError):
            worker.step()
        self.assertFalse(worker.step())
        self.assertEqual(sender.sent, ["D1"])

    def test_sender_exception_records_unknown_instead_of_retryable_failure(self):
        class Hub:
            def __init__(self):
                self.report = None

            def request(self, method, path, payload=None):
                if path == "/v1/artifacts/next":
                    return None
                if path == "/v1/deliveries/next":
                    return {"id": "D1", "attempt_id": "A1"}
                self.report = payload
                return {"ok": True}

        class Sender:
            def send(self, delivery):
                raise OSError("sender connection lost after possible dispatch")

        hub = Hub()
        self.assertTrue(FollowupWorker(hub, None, Sender()).step())
        self.assertEqual(hub.report["status"], "unknown")
        self.assertNotIn("definitely_not_sent", hub.report)

    def test_native_sender_timeout_is_unknown_even_if_partial_output_claims_success(self):
        from dots_duet_lite_hermes.delivery import NativeSender
        with tempfile.TemporaryDirectory() as directory:
            sender = NativeSender(sys.executable, directory, directory)
            with patch("subprocess.run", side_effect=subprocess.TimeoutExpired(["fixed"], 1, output='{"success":true}')):
                result = sender.send({"id": "D1", "attempt_id": "A1"})
        self.assertEqual(result["status"], "unknown")
        self.assertNotIn("definitely_not_sent", result)

    def test_trusted_delivery_is_sent_and_receipt_is_reported_without_starting_a_model(self):
        delivery = {
            "id": "D1", "attempt_id": "A1", "task_id": "Q1", "kind": "text",
            "target": {"platform": "weixin", "chat_id": "trusted@im.wechat"}, "text": "Task needs your answer",
        }

        class Hub:
            def __init__(self):
                self.reports = []

            def request(self, method, path, payload=None):
                if path == "/v1/artifacts/next":
                    return None
                if path == "/v1/deliveries/next":
                    return delivery
                if path != "/v1/deliveries/D1/report":
                    raise AssertionError("outbound delivery must not start a model")
                self.reports.append(payload)
                return {"ok": True}

        class Native:
            def request(self, *args, **kwargs):
                raise AssertionError("outbound delivery must not start a model")

        class Sender:
            def send(self, actual):
                if actual != delivery:
                    raise AssertionError("the trusted Hub target or content changed")
                return {"status": "delivered", "receipt": {"success": True, "message_id": "native-a"}}

        hub = Hub()
        self.assertTrue(FollowupWorker(hub, Native(), Sender()).step())
        self.assertEqual(hub.reports, [{
            "attempt_id": "A1", "status": "delivered", "receipt": {"success": True, "message_id": "native-a"},
        }])


if __name__ == "__main__":
    unittest.main()
