# dots-lite

**Run and follow up on Codex work on your own computer through Hermes chat.**

[简体中文](README.zh-CN.md) · [Deployment skill](skills/dots-lite-deploy/SKILL.md) · [Hermes adapter](adapters/hermes/README.md) · [Releases](https://github.com/LiuSphere/dots-lite/releases)

Your code, Codex login and dependencies stay on your computer. Hermes receives chat requests. A Linux Hub routes them to the paired Windows executor and returns task state, approvals and files to the correct profile.

```text
Hermes chat / WeChat
        │ MCP + trusted-source plugin
        ▼
Linux Hub ── private WebSocket ◀── Windows executor
                                      │ local stdio
                                      ▼
                                Codex app-server
                                      │
                              Selected task directory
```

## Current source

The current source uses **protocol 4**, with Hermes adapter **0.4.0.dev0**. Released v0.3.0 uses protocol 3. Upgrade the Hub, executor and adapter together. Existing state requires an explicit offline migration; replacing one binary alone is insufficient.

- **Continue native sessions.** List accessible persistent sessions in the paired Codex environment, including other sources and archived sessions. Follow pagination and preserve the original thread ID, history and directory. Listing does not resume a session or start a model turn.
- **Keep failures visible.** Attachment checks the effective provider, directory, approvals and sandbox. It first shows those settings and keeps the task paused; a later explicit user request continues it. Active-writer conflicts and unverified permissions return a reason and queue a notification. The service does not force control of another active instance.
- **Choose a task directory.** New installations use `directory_mode: task`. The user supplies an existing absolute directory or selects the original native thread in the current trusted chat message. File tools stay inside that task's actual directory. An optional `allowlist` adds root restrictions; upgrades retain existing roots.
- **Follow meaningful events.** Ordinary output, waiting and native retries do not create another model diagnosis. Normal progress has no task-wide eight-follow-up or one-hour limit. A single unresolved fault still has bounded diagnostics. Native resource limits, user pauses and unknown operations remain effective.
- **Pause for maintenance.** Two Windows scripts pause and resume the local service. Pause first suspends managed tasks and Goals, then confirms owned processes have exited. Resume starts the service while leaving individual tasks paused.
- **Verify and deliver files.** Keep execution, verification, file acquisition and chat delivery separate. A completed turn or a model's success claim is not proof that the user's goal passed or a file arrived.

For example, in task mode:

> Fix the login failure in `C:\work\demo`. Check the cause first, then return the test results and changed files.

Directory selection does not grant administrator rights or bypass native approval. The service checks the Linux UID, profile, executor and trusted message source. It does not modify Hermes core or implement a second native run engine.

## Deployment

The supported deployment is **Linux Hub + Windows executor + Hermes**. Each connected profile uses its own unprivileged OS account. Executors connect outbound; server-to-Windows SSH is unnecessary. The Hub has no built-in TLS, so use a controlled private network or an authenticated encrypted tunnel.

Ask your agent to use [dots-lite-deploy](skills/dots-lite-deploy/SKILL.md):

```text
Deploy or update dots-lite using skills/dots-lite-deploy/SKILL.md.
Inspect the existing setup and preserve its credentials, IDs, task history,
pauses and directory restrictions. Ask only for missing decisions.
Prepare configuration and backups before changing services.
If new temporary sudo access is needed, explain its authority and cleanup
before creating it. Report observed checks separately from untested behavior.
```

Deployment uses a server administrator identity only for installation. Do not give maintenance keys or sudo rights to business profiles. Temporary access has a separate [provisioning and revocation procedure](skills/dots-lite-deploy/references/access.md); an existing shared administrator identity must not be deleted as temporary cleanup.

Full configuration and migration instructions are in [deployment operations](skills/dots-lite-deploy/references/deployment.md). The MCP configuration key is `dots_lite`; its seven tools use `dots_lite_*`. Runtime environment names use `DOTS_LITE_*`.

Protocol 3 migration requires confirmed shutdown of old writers and consistent backups. Use the documented explicit migration commands as the actual service users. Migration preserves task/thread/operation IDs, receipts and recovery history, retains `allowlist`, and leaves tasks paused. Once protocol 4 accepts new business or control writes, do not restore an old snapshot over the new state.

Windows maintenance scripts belong beside `bin/executor.exe` and read `config/executor.json`. Their `maintenance` configuration must match the real scheduled task, executable and exact arguments. No private configuration, keys, database or binaries are committed here.

## Validation and limits

Local checks cover Windows Go tests and vet, Linux Hub/executor/integration tests, and the Hermes adapter suite. Real Codex checks cover read-only inventory and a dedicated thread continued through a localhost fixed-response provider. A controlled clock covers 72 hours and 12 normal stages; this is not three days of live operation or a measurement of model cost.

Real model supervision, human approvals, actual WeChat attachment exchange, multiple execution computers and long-running fault recovery require their own field evidence. Old protocol executors cannot connect to a protocol 4 Hub. Reading a native session does not establish safe control of an already active desktop chat. Native run counts do not equal underlying LLM request counts; unavailable usage stays unknown.

## Development

[go.mod](go.mod) pins the Go toolchain. The adapter uses Python 3.14 and its uv lockfile. Windows users running a built executor do not need Go or the adapter's Python environment.

```sh
# Repository root. Normal tests do not call a real model.
go test ./... -count=1
go vet ./...
go build -o dist/executor.exe ./cmd/executor
uv sync --project adapters/hermes --locked
uv run --project adapters/hermes --locked python -m unittest discover -s adapters/hermes/tests -v
```

Build the Hub on Linux, or cross-compile with `GOOS=linux GOARCH=amd64`. Run Linux UID/socket tests on Linux; platform skips on Windows do not validate them. `DOTS_LITE_REAL_CODEX=1` explicitly enables tests that can call a real model and write a temporary workspace. Never set it globally as a health-check option.

```text
cmd/hub/           Linux Hub entry point
cmd/executor/      Windows executor and maintenance commands
internal/          Protocol, authorization, sessions, files and durable state
adapters/hermes/   MCP, trusted-source plugin and background adapter
skills/            Public deployment workflow and assets
```

`docs/` contains local requirements, design, progress and acceptance records. It is ignored by Git. Public deployment instructions do not depend on those files.

## Contributing and license

Use Issues for bugs and proposals and Pull Requests for changes. Include the version, OS, reproduction steps and redacted logs. Keep private keys, tokens, chat identifiers and personal deployment records out of commits. Run the relevant tests and distinguish model execution, file acquisition and delivery results.

Licensed under [BSD 3-Clause](LICENSE). Commercial use, modification and redistribution are permitted subject to its terms. Third-party components retain their own licenses.