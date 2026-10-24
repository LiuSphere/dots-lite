"""Opt-in real Hub -> remote executor -> Codex -> private-file acceptance.

Run under the bound Linux test UID. The source is an explicit test fixture;
native Hermes source injection and Weixin delivery are separate checks.
"""

import argparse
import hashlib
import json
import os
from pathlib import Path
import time
import uuid

from dots_duet_lite_hermes.client import HubClient
from dots_duet_lite_hermes.downloads import materialize_download
from dots_duet_lite_hermes.worker import FollowupWorker


def smoke(options):
    hub = HubClient(options.socket)
    identity = uuid.uuid4().hex
    source = {
        "kind": "gateway", "platform": "fixture", "sender_id": "sender-a",
        "chat_id": "chat-a", "session_id": "cross-machine-" + identity,
        "native_task_id": "explicit-test-fixture", "tool_call_id": "",
    }

    def call(tool, arguments):
        source["tool_call_id"] = uuid.uuid4().hex
        registered = hub.request("POST", "/v1/invocations", {
            "tool": tool, "arguments": arguments, "source": source,
        })
        return hub.request("POST", "/v1/tools/call", {
            "tool": tool, "arguments": arguments, "context_ref": registered["context_ref"],
        })

    def wait(check, seconds=120):
        deadline = time.monotonic() + seconds
        while time.monotonic() < deadline:
            result = check()
            if result:
                return result
            time.sleep(0.5)
        raise TimeoutError("Bounded cross-machine acceptance check did not settle")

    machine = call("duet_query", {"query": "machine"})
    if not machine["connected"] or not machine["snapshot"]["initialized"]:
        raise RuntimeError("Actual remote executor is not initialized")
    text_name, binary_name, extra_name = ["smoke-" + identity + suffix for suffix in (".txt", ".bin", "-extra.txt")]
    text = b"dots-duet-cross-machine-ok\n"
    binary = bytes([0, 1, 2, 127, 128, 255])
    extra = b"dots-duet-extra-ok\n"
    prepared = call("duet_prepare", {
        "action": "task.create", "project_id": "acceptance", "name": "isolated cross-machine acceptance",
        "input": f"Work only in the current registered workspace. Create exactly three files: {text_name} with UTF-8 bytes dots-duet-cross-machine-ok followed by one LF; {binary_name} with exactly six bytes 00 01 02 7f 80 ff; {extra_name} with UTF-8 bytes dots-duet-extra-ok followed by one LF. Use one local write command and read all three to verify. Do not inspect other directories, use network, install software, or change other files. Report briefly and stop.",
        "artifacts": [text_name, binary_name],
    })
    if prepared["stage"] != "prepared":
        raise RuntimeError("Operation was not registered before submission")
    report = {"task_id": prepared["task_id"], "operation_id": prepared["operation_id"],
              "source_is_explicit_test_fixture": True, "uid": os.getuid()}
    result_path = Path(options.report)
    result_path.write_text(json.dumps(report, indent=2))
    result_path.chmod(0o600)
    submitted = False
    try:
        submitted = True
        call("duet_submit", {"operation_id": prepared["operation_id"]})

        def completed():
            task = call("duet_query", {"query": "task", "task_id": prepared["task_id"]})
            if task.get("waiting") or task["execution"] in {"failed", "unknown", "interrupted"}:
                raise RuntimeError("Actual task did not complete without user intervention: " + json.dumps(task))
            return task if task["execution"] == "completed" else None

        task = wait(completed)
        if task["verification"] != "pending" or task["delivery"] != "pending":
            raise RuntimeError("Native completion incorrectly claimed verification or delivery")
        call("duet_submit", {"operation_id": prepared["operation_id"]})

        def artifacts_ready():
            listing = call("duet_query", {"query": "artifacts", "task_id": prepared["task_id"]})
            return listing if len([a for a in listing["items"] if a["status"] == "available"]) == 2 else None

        wait(artifacts_ready, 30)

        class NoNativeRuns:
            def request(self, *args, **kwargs):
                raise RuntimeError("Artifact acquisition unexpectedly invoked a native model run")

        worker = FollowupWorker(hub, NoNativeRuns())
        for _ in range(2):
            if not worker.step():
                raise RuntimeError("Automatic artifact transfer did not run")
        listing = call("duet_query", {"query": "artifacts", "task_id": prepared["task_id"]})
        expected = {hashlib.sha256(text).hexdigest(): text, hashlib.sha256(binary).hexdigest(): binary}
        for item in listing["items"]:
            if item["status"] != "acquired":
                raise RuntimeError("Artifact has no verified private copy: " + json.dumps(item))
            copy = Path(item["local_path"])
            content = copy.read_bytes()
            if expected.pop(hashlib.sha256(content).hexdigest(), None) != content:
                raise RuntimeError("Automatic artifact bytes did not match")
        if expected:
            raise RuntimeError("One automatic artifact was missing")
        downloaded = call("duet_query", {
            "query": "download", "task_id": prepared["task_id"], "path": extra_name,
        })
        acquired = materialize_download(downloaded)
        if acquired["acquired"] != "complete" or Path(acquired["local_path"]).read_bytes() != extra:
            raise RuntimeError("Active acquisition did not obtain the omitted artifact")
        final = call("duet_query", {"query": "task", "task_id": prepared["task_id"]})
        if final["verification"] != "pending" or final["delivery"] == "delivered":
            raise RuntimeError("File acquisition incorrectly claimed goal validation or Weixin delivery")
        report.update(real_codex_completed=True, automatic_text_and_binary_acquired=True,
                      omitted_artifact_actively_acquired=True, same_operation_replayed=True,
                      verification=final["verification"], delivery=final["delivery"])
    finally:
        if submitted:
            stop = call("duet_stop", {"task_id": prepared["task_id"]})
            report["stop"] = stop
        result_path.write_text(json.dumps(report, indent=2))
        result_path.chmod(0o600)
        if submitted and not report["stop"].get("stop_confirmed"):
            raise RuntimeError("Native stop was not actually confirmed: " + json.dumps(report["stop"]))
    print(json.dumps(report))


if __name__ == "__main__":
    parser = argparse.ArgumentParser()
    parser.add_argument("--socket", required=True)
    parser.add_argument("--report", required=True)
    smoke(parser.parse_args())
