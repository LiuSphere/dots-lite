"""The model-visible business interface. Identity and approval are not inputs."""

QUERIES = (
    "machine", "folders", "projects", "tasks", "task", "operations", "operation", "history",
    "artifacts", "directory", "text", "download", "requests", "followups", "session", "goal", "deliveries",
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
    "duet_query": schema({
        "query": {"type": "string", "enum": list(QUERIES)},
        "task_id": STRING, "operation_id": STRING, "folder_id": STRING, "project_id": STRING,
        "path": STRING, "cursor": STRING, "offset": {"type": "integer", "minimum": 0},
        "length": {"type": "integer", "minimum": 1, "maximum": 8 * 1024 * 1024},
    }, ["query"]),
    "duet_prepare": schema({
        "action": {"type": "string", "enum": [
            "task.create", "task.continue", "turn.steer", "goal.set", "files.upload",
        ]},
        "task_id": STRING, "folder_id": STRING, "project_id": STRING, "input": STRING, "name": STRING,
        "working_directory": {
            **STRING,
            "description": "Relative to the authorized folder root; defaults to '.'. Use forward slashes, without '..', absolute paths, backslashes or colons. A task keeps this directory for its lifetime.",
        },
        "expected_turn_id": STRING, "goal": OBJECT, "output_schema": OBJECT,
        "artifacts": {"type": "array", "items": STRING},
        "inputs": {"type": "array", "items": ATTACHMENT},
        "upload": ATTACHMENT,
    }, ["action"]),
    "duet_submit": schema({"operation_id": STRING}, ["operation_id"]),
    "duet_stop": schema({"task_id": STRING}, ["task_id"]),
    "duet_answer": schema({
        "task_id": STRING, "request_id": STRING, "answer": OBJECT,
    }, ["task_id", "request_id", "answer"]),
    "duet_approve": schema({
        "task_id": STRING, "request_id": STRING,
    }, ["task_id", "request_id"]),
    "duet_record": schema({
        "task_id": STRING,
        "kind": {"type": "string", "enum": ["verification", "delivery", "followup"]},
        "status": STRING, "evidence": OBJECT,
    }, ["task_id", "kind", "status", "evidence"]),
}
TOOL_SCHEMAS["duet_prepare"]["allOf"] = [{
    "if": {"properties": {"action": {"const": "files.upload"}}},
    "then": {"required": ["task_id", "upload"]},
}]

DESCRIPTIONS = {
    "duet_query": "Query your bound machine, authorized folders, tasks, operations, requests, or files. Use folders to select a folder_id. directory without task_id requires explicit folder_id and lists relative to that folder; text/download and task directory queries are relative to the task's fixed working directory. projects/project_id are compatibility aliases for folders/folder_id. session returns native thread/runtime and pending requests; goal returns the actual native Goal.",
    "duet_prepare": "Register a business operation without executing it. task.create selects folder_id and an optional relative working_directory (default '.'); later operations cannot change that directory. project_id is a compatibility alias for folder_id. Keep operation_id before submitting. task.create inputs and files.upload upload accept name/local_path references to attachments in this exact inbound message. Never supply file bytes, Base64 or a different message's files. submit performs the write.",
    "duet_submit": "Submit a previously registered operation. An uncertain result must be queried, not recreated.",
    "duet_stop": "Persist pause intent and request native Goal pause and turn interruption for this task.",
    "duet_answer": "Answer a structured technical question. This does not grant approvals or permissions.",
    "duet_approve": "Ask the original human to approve or decline one pending native approval through MCP elicitation.",
    "duet_record": "Record verification, delivery, or followup evidence without treating model claims as execution proof.",
}
