"""Opt-in live Hermes run test in a separately prepared private HERMES_HOME."""

import argparse
import asyncio
import json
import os
from pathlib import Path
import socket
import sys
import time
import uuid

from dots_duet_lite_hermes.runs import RunsClient


async def smoke(home, source):
    os.environ["HERMES_HOME"] = str(home)
    os.chdir(home)
    sys.path.insert(0, str(source))
    from gateway.config import PlatformConfig
    from gateway.platforms.api_server import APIServerAdapter

    token = (home / "api-token").read_text().strip()
    with socket.socket() as probe:
        probe.bind(("127.0.0.1", 0))
        port = probe.getsockname()[1]
    adapter = APIServerAdapter(PlatformConfig(enabled=True, extra={
        "host": "127.0.0.1", "port": port, "key": token,
    }))
    if not await adapter.connect():
        raise RuntimeError("Isolated native API did not start")
    client = RunsClient(f"http://127.0.0.1:{port}", token)

    async def request(*args, **kwargs):
        return await asyncio.to_thread(client.request, *args, **kwargs)

    try:
        capabilities = await request("GET", "/v1/capabilities")
        durable = capabilities["features"]["runs_idempotency"]["durable"]
        if not durable:
            raise RuntimeError("Native run idempotency is not durable")
        identity = uuid.uuid4().hex
        body = {"input": "Reply exactly DDL_HERMES_RUN_OK. Do not use any tools.", "session_id": "duet-test-" + identity}
        key = "duet-smoke-" + identity
        accepted = await request("POST", "/v1/runs", body, idempotency_key=key)
        repeated = await request("POST", "/v1/runs", body, idempotency_key=key)
        if accepted["run_id"] != repeated["run_id"] or not repeated.get("replayed"):
            raise RuntimeError("Native replay created a different run")
        run_id = accepted["run_id"]
        deadline = time.monotonic() + 75
        while time.monotonic() < deadline:
            status = await request("GET", "/v1/runs/" + run_id)
            if status["status"] in {"completed", "failed", "cancelled"}:
                break
            await asyncio.sleep(1)
        else:
            await request("POST", "/v1/runs/" + run_id + "/stop", {})
            raise RuntimeError("Native smoke run exceeded the bounded deadline")
        if status["status"] != "completed" or "DDL_HERMES_RUN_OK" not in str(status.get("output", "")):
            raise RuntimeError("Native run did not return the expected result: " + str(status.get("error", status["status"])))
        if status.get("session_id") != body["session_id"]:
            raise RuntimeError("Native run changed the selected API session")
        report = {"run_id": run_id, "session_id": body["session_id"], "idempotency_key": key,
                  "request": body, "status": status["status"], "durable": durable, "same_run_replayed": True}
        path = home / "native-smoke-result.json"
        path.write_text(json.dumps(report, indent=2))
        path.chmod(0o600)
    finally:
        await adapter.disconnect()
    # A new adapter opens the same native store; no clone of Hermes run state is used.
    restarted = APIServerAdapter(PlatformConfig(enabled=True, extra={"host": "127.0.0.1", "port": port, "key": token}))
    if not await restarted.connect():
        raise RuntimeError("Isolated native API could not restart")
    try:
        restored = await request("GET", "/v1/runs/" + report["run_id"])
        if restored["status"] != "completed" or restored["session_id"] != report["session_id"]:
            raise RuntimeError("Native durable result was not restored after restart")
        report["restart_result_restored"] = True
        path.write_text(json.dumps(report, indent=2))
        print(json.dumps({key: value for key, value in report.items() if key not in {"request", "idempotency_key"}}))
    finally:
        await restarted.disconnect()


if __name__ == "__main__":
    parser = argparse.ArgumentParser()
    parser.add_argument("--home", required=True, type=Path)
    parser.add_argument("--hermes-source", required=True, type=Path)
    args = parser.parse_args()
    asyncio.run(smoke(args.home.resolve(), args.hermes_source.resolve()))
