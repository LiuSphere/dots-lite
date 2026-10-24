"""Fixed native Hermes subprocess boundary for trusted outbox deliveries."""

import json
from pathlib import Path
import subprocess


class NativeSender:
    def __init__(self, hermes_python, hermes_source, files_root, timeout=120):
        python = Path(hermes_python).absolute()
        if not python.is_file():
            raise FileNotFoundError("Configured Hermes Python does not exist")
        self.command = [
            str(python), str(Path(__file__).with_name("delivery_helper.py").resolve()),
            "--hermes-source", str(Path(hermes_source).resolve(strict=True)),
            "--files-root", str(Path(files_root).resolve()),
        ]
        self.timeout = timeout

    def send(self, delivery):
        try:
            result = subprocess.run(
                self.command, input=json.dumps(delivery, ensure_ascii=False, allow_nan=False),
                stdout=subprocess.PIPE, encoding="utf-8", timeout=self.timeout, check=False,
            )
        except subprocess.TimeoutExpired:
            return {"status": "unknown", "error": "Native sender timed out; its delivery outcome is unknown"}
        except OSError as error:
            return {"status": "failed", "definitely_not_sent": True, "error": "Native sender could not start: " + str(error)}
        if result.returncode or len(result.stdout) > 65536:
            return {"status": "unknown", "error": "Native sender exited without a bounded complete receipt"}
        try:
            report = json.loads(result.stdout)
        except ValueError:
            return {"status": "unknown", "error": "Native sender returned no valid receipt; its outcome is unknown"}
        if not isinstance(report, dict) or report.get("status") not in {"delivered", "failed", "unknown"}:
            return {"status": "unknown", "error": "Native sender returned an invalid delivery report"}
        return report
