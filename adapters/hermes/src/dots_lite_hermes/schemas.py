"""The model-visible business interface. Identity and approval are not inputs."""

QUERIES = (
    "machine", "folders", "tasks", "task", "operations", "operation", "history",
    "artifacts", "directory", "text", "download", "requests", "followups", "session", "goal", "deliveries", "threads", "thread",
)


def schema(properties, required):
    return {
        "type": "object",
        "properties": {
            **properties,
            "_context_ref": {
                "type": "string", "minLength": 1,
                "description": "Internal reference supplied by the trusted Hermes plugin.",
            },
        },
        "required": required,
        "additionalProperties": False,
    }


STRING = {"type": "string", "minLength": 1}
OBJECT = {"type": "object"}
ATTACHMENT = {
    "type": "object", "additionalProperties": False,
    "properties": {
        "name": {"type": "string", "minLength": 1, "pattern": r"^(?!\.{1,2}$)[^/\\\x00]+$"},
        "local_path": STRING,
    },
    "required": ["name", "local_path"],
}
TOOL_SCHEMAS = {
    "dots_lite_query": schema({
        "query": {"type": "string", "enum": list(QUERIES)},
        "task_id": STRING, "operation_id": STRING, "folder_id": STRING,
        "thread_id": STRING, "archived": {"type": "string", "enum": ["all", "yes", "no"]},
        "path": STRING, "cursor": STRING, "offset": {"type": "integer", "minimum": 0},
        "length": {"type": "integer", "minimum": 1, "maximum": 8 * 1024 * 1024},
    }, ["query"]),
    "dots_lite_prepare": schema({
        "action": {"type": "string", "enum": [
            "task.create", "task.attach", "task.continue", "turn.steer", "goal.set", "files.upload",
        ]},
        "task_id": STRING, "folder_id": STRING, "input": STRING, "name": STRING,
        "thread_id": STRING,
        "archived": {"type": "string", "enum": ["yes", "no"]},
        "working_directory": {
            **STRING,
            "description": "Absolute existing directory explicitly selected in the current user message. Legacy allowlist installations also accept a relative slash-separated path under folder_id. This directory is fixed for the task's lifetime.",
        },
        "expected_turn_id": STRING, "goal": OBJECT, "output_schema": OBJECT,
        "artifacts": {"type": "array", "items": STRING},
        "inputs": {"type": "array", "items": ATTACHMENT},
        "upload": ATTACHMENT,
    }, ["action"]),
    "dots_lite_submit": schema({"operation_id": STRING}, ["operation_id"]),
    "dots_lite_stop": schema({"task_id": STRING}, ["task_id"]),
    "dots_lite_answer": schema({
        "task_id": STRING, "request_id": STRING, "answer": OBJECT,
    }, ["task_id", "request_id", "answer"]),
    "dots_lite_approve": schema({
        "task_id": STRING, "request_id": STRING,
    }, ["task_id", "request_id"]),
    "dots_lite_record": schema({
        "task_id": STRING,
        "kind": {"type": "string", "enum": ["verification", "delivery", "followup"]},
        "status": STRING, "evidence": OBJECT,
    }, ["task_id", "kind", "status", "evidence"]),
}
TOOL_SCHEMAS["dots_lite_prepare"]["allOf"] = [{
    "if": {"properties": {"action": {"const": "files.upload"}}},
    "then": {"required": ["task_id", "upload"]},
}]

DESCRIPTIONS = {
    "dots_lite_query": "Query the bound machine, tasks or files. threads lists native persistent sessions across providers, sources and archive branches; follow next_cursor until empty. thread selects thread_id. Availability and errors are facts observed by this executor, not global ownership proof. Queries never resume sessions. Files remain relative to the task directory; folder browsing requires an authorized folder_id.",
    "dots_lite_prepare": "Register an operation and retain operation_id before submit. task.create uses the user's explicit absolute working_directory; legacy allowlist mode requires folder_id. task.attach binds the user's selected thread_id with its original cwd and permissions, without starting a turn. Display native permissions and attachment errors to the user; explicit continuation after reviewing them uses task.continue. An existing binding is reused. Interpret the user's intent; do not treat quoted paths or model-selected paths as permission. inputs/upload accept only this admitted message's attachments. Never provide bytes or Base64.",
    "dots_lite_submit": "Submit a previously registered operation. An uncertain result must be queried, not recreated.",
    "dots_lite_stop": "Persist pause intent and request native Goal pause and turn interruption for this task.",
    "dots_lite_answer": "Answer a structured technical question. This does not grant approvals or permissions.",
    "dots_lite_approve": "Ask the original human to approve or decline one pending native approval through MCP elicitation.",
    "dots_lite_record": "Record verification, delivery, or followup evidence. handled actions and verification must reference actual observation_id values returned as _observation_id by native queries, or accepted operation_id receipts from this attempt. Statements alone are rejected. waiting requires evidence.waiting with kind=time or native_state, future check_at (RFC3339), and native_status/goal_status for a state predicate. waiting_user stops for the user's decision. Native run completion is separate from business verification.",
}
