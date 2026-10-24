import argparse
import json
import sys
import time
import os
from pathlib import Path

import anyio

from .server import serve
from .client import HubClient
from .runs import RunsClient
from .worker import FollowupWorker
from .delivery import NativeSender


def main():
    parser = argparse.ArgumentParser(description="Profile-local dots-duet MCP and native Hermes followup adapter")
    commands = parser.add_subparsers(dest="command")
    worker = commands.add_parser("worker", help="bridge hub followups to this profile's native Hermes runs")
    worker.add_argument("--socket", required=True)
    worker.add_argument("--api-url", required=True)
    worker.add_argument("--api-token-file", required=True)
    worker.add_argument("--hermes-python", required=True, help="fixed absolute native Hermes Python path for sends")
    worker.add_argument("--hermes-source", required=True, help="fixed official Hermes source directory for sends")
    worker.add_argument("--once", action="store_true")
    options = parser.parse_args()
    if options.command != "worker":
        anyio.run(serve)
        return
    home = Path(os.environ.get("HERMES_HOME", Path.home() / ".hermes"))
    files = Path(os.environ.get("DOTS_DUET_FILES_DIR", home / "dots-duet" / "files"))
    sender = NativeSender(options.hermes_python, options.hermes_source, files)
    bridge = FollowupWorker(HubClient(options.socket), RunsClient.from_token_file(options.api_url, options.api_token_file), sender)
    while True:
        try:
            bridge.step()
        except (RuntimeError, OSError, ValueError, KeyError) as error:
            if options.once:
                raise
            print(json.dumps({"event": "followup_error", "error": str(error)}), file=sys.stderr, flush=True)
        if options.once:
            return
        time.sleep(5)


if __name__ == "__main__":
    main()
