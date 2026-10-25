"""External followup transport. Hermes alone owns run admission and execution."""

from urllib.parse import quote
from datetime import datetime, timezone

from .downloads import materialize_download


class FollowupWorker:
    def __init__(self, hub, runs, sender=None):
        self.hub = hub
        self.runs = runs
        self.sender = sender

    def step(self):
        artifact = self.hub.request("GET", "/v1/artifacts/next")
        if artifact:
            route = "/v1/artifacts/" + quote(artifact["id"], safe="") + "/acquired"
            try:
                file = artifact["file"]
                if file.get("offset") != 0 or file.get("returned_bytes") != file.get("size") or file.get("truncated") is not False:
                    raise ValueError("Automatic artifact transfer is incomplete; no full file was acquired")
                saved = materialize_download(file)
            except (ValueError, OSError, KeyError) as error:
                self.hub.request("POST", route, {"acquired": False, "error": str(error)})
                raise
            self.hub.request("POST", route, {
                "local_path": saved["local_path"], "sha256": saved["sha256"],
                "size": saved["size"], "returned_bytes": saved["returned_bytes"],
                "offset": saved["offset"], "acquired": saved["acquired"] == "complete",
            })
            return True
        if self.sender is not None:
            delivery = self.hub.request("GET", "/v1/deliveries/next")
            if delivery:
                try:
                    result = self.sender.send(delivery)
                except Exception as error:
                    result = {"status": "unknown", "error": "Sender raised " + type(error).__name__ + "; outcome is unknown"}
                route = "/v1/deliveries/" + quote(delivery["id"], safe="") + "/report"
                self.hub.request("POST", route, {**result, "attempt_id": delivery["attempt_id"]})
                return True
        followup = self.hub.request("GET", "/v1/followups/next")
        if not followup or followup.get("idle"):
            return False
        run_id = followup.get("run_id")
        if not run_id:
            features = self.runs.request("GET", "/v1/capabilities").get("features", {})
            capability = features.get("runs_idempotency", {})
            if not capability.get("supported") or not capability.get("durable"):
                raise RuntimeError("Hermes native runs are not durably idempotent; no followup was submitted")
            retention = capability.get("retention_seconds")
            offered = followup.get("offered_at")
            if not isinstance(retention, (int, float)) or retention <= 0 or not offered:
                raise RuntimeError("Native idempotency retention or first submission time is unverified; no run was submitted")
            first = datetime.fromisoformat(offered.replace("Z", "+00:00"))
            if first.tzinfo is None:
                raise RuntimeError("Followup first submission time requires a timezone")
            if (datetime.now(timezone.utc) - first).total_seconds() >= retention:
                self.hub.request("POST", "/v1/followups/" + quote(followup["id"], safe="") + "/submission", {
                    "error": "Idempotency retention expired with no confirmed run_id; automatic POST replay is disabled",
                })
                return True
            request = followup["request"]
            if request.get("session_id") != followup["session_id"]:
                raise RuntimeError("Followup request does not match its pre-registered API session")
            self.hub.request("POST", "/v1/followups/" + quote(followup["id"], safe="") + "/submission", {"status": "attempting"})
            accepted = self.runs.request(
                "POST", "/v1/runs", request, idempotency_key=followup["idempotency_key"],
            )
            run_id = accepted["run_id"]
        native = self.runs.request("GET", "/v1/runs/" + quote(run_id, safe=""))
        if native.get("run_id") != run_id or native.get("session_id") != followup["session_id"]:
            raise RuntimeError("Hermes native run/session binding changed or is unverified; no task was rebound")
        route = "/v1/followups/" + quote(followup["id"], safe="")
        self.hub.request("POST", route + "/run", {
            "run_id": run_id, "session_id": followup["session_id"], "status": native["status"],
        })
        if native["status"] in {"completed", "failed", "cancelled"}:
            self.hub.request("POST", route + "/report", {
                "run_id": run_id, "status": native["status"],
                "evidence": {
                    "native_status": native["status"], "native_run_id": run_id,
                    "native_session_id": native["session_id"],
                    "output": str(native.get("output", ""))[:2048],
                    "error": str(native.get("error", ""))[:1024],
                    "requires_business_verification": True,
                },
            })
        return True
