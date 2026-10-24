# dots-lite

**Run and follow up on Codex tasks on your own computer, through Hermes chat.**

[简体中文](README.zh-CN.md) · [User guide](docs/使用指南.md) · [Deployment guide](docs/运行指南.md) · [Releases](https://github.com/LiuSphere/dots-lite/releases)

Your code and Codex run on your computer. Hermes receives chat requests, a Linux Hub routes tasks to the paired computer, and results return to the correct profile. Continue a session, add instructions, stop work, handle approvals, and let Hermes inspect actual files before delivering the result.

```text
Chat / WeChat
    │
    ▼
Hermes profile ── MCP + trusted-source plugin ── Linux Hub
                                                  ▲
                                  Private WebSocket, outbound from executor
                                                  │
                                           Windows executor
                                                  │ stdio
                                           Codex app-server
                                                  │
                                           Authorized folders
```

## Features

- **Use your own environment.** Keep your Codex login, dependencies and working directories on your computer.
- **Authorize folders.** Allow one or more roots, then choose an existing subdirectory for each task without registering every project.
- **Track work across sessions.** Persist task IDs, sessions, operations and events so reconnects and chat changes do not lose task identity.
- **Route questions and approvals.** Send answers to the correct task, turn and pending request.
- **Transfer and verify files.** Accept attachments, read or download task files, acquire artifacts, and record verification and delivery separately.
- **Isolate users.** Check the Linux UID, profile, executor and trusted chat source. Run business services under unprivileged accounts.

For example, after authorizing `C:\work`, ask in chat:

> Fix the login failure in the `demo` folder. Check the cause first, then return the test results and changed files.

You can then query the task, add instructions or stop the current execution. The working directory must already exist inside an authorized root. Model instructions cannot expand folder access.

## Get started

The current deployment targets **Linux Hub + Windows executor + Hermes**. The Hub authenticates local callers through Unix socket peer credentials. Executors connect outbound over a controlled network. The Hub has no built-in TLS; use a private network or an authenticated encrypted tunnel.

| Location | Requirements |
| --- | --- |
| Execution computer | Windows, a working authenticated Codex app-server, and task dependencies |
| Server | Linux, systemd, a working Hermes profile; a separate OS account for each connected profile |
| Network | Executor can reach the Hub over a controlled network; no server-to-computer SSH access is needed |
| Deployment tools | An agent with local and server access; PowerShell 7, Python 3.10+ and OpenSSH on Windows for temporary-access preparation |

1. Select a version from [Releases](https://github.com/LiuSphere/dots-lite/releases). Hub and executor must use the same protocol; v0.3.0 uses protocol 3.
2. Ask your agent to read the repository's [dots-lite-deploy skill](skills/dots-lite-deploy/SKILL.md). Provide the server, target profile, installation directories and allowed folders.
3. The agent prepares configuration, backups and services, checks versions and permissions, and verifies pairing. Windows defaults to hidden startup at user login. Service restarts reuse credentials and reconnect automatically.
4. After acceptance checks, the agent revokes temporary deployment access and deletes the dedicated local private key. Routine tasks do not need a server administrator key.

A prompt you can give your agent:

```text
Deploy dots-lite using skills/dots-lite-deploy/SKILL.md.
Inspect the existing setup, then ask for any missing server, profile,
installation-directory and authorized-folder choices.
If temporary passwordless sudo is needed, explain the risks and obtain
my acknowledgement before preparing access. Prepare everything for me;
give me only the command that requires my password.
After acceptance checks, revoke temporary access, delete the dedicated
local deployment private key, and report what was and was not verified.
```

**A temporary deployment key grants root-equivalent server access.** Anyone holding it can control the server during deployment. The skill includes [risk disclosure, one-command provisioning and revocation](skills/dots-lite-deploy/references/access.md). Generate the private key locally and install only its public key on the server. With an existing noninteractive ordinary SSH login, the user normally enters only the sudo password. Otherwise SSH may require an additional password or multi-factor authentication.

## Status and limits

This is an early-stage project. The Hub, executor, MCP adapter, source binding, background follow-up and file transfers are implemented. Acceptance records include real Codex execution and cross-machine file retrieval. The current naming update has also passed deployment checks. See the [acceptance record](docs/验收记录.md) for evidence and limits.

Two real users on different computers, human WeChat approval, actual WeChat attachment exchange, sustained fault recovery and startup after a system reboot still need field validation. Reading or restoring Codex history does not establish control over an already active desktop chat.

The MCP configuration key is `dots_lite`; all seven tools use the `dots_lite_*` prefix. Configuration uses `folders`, `folder_ids` and `folder_id`. The current version rejects old names. Existing deployments require coordinated shutdown, backup and offline state conversion before upgrading; see the [operations guide](docs/运行指南.md).

## Development

The Go version is declared in [go.mod](go.mod), currently Go 1.27.3. The Hermes adapter uses Python 3.14 and a uv lockfile. Running a released Windows executor does not require Go or the adapter's Python environment.

```sh
# From the repository root. Normal tests do not call a real model.
go mod download
go test ./...
go vet ./...
uv sync --project adapters/hermes --locked
uv run --project adapters/hermes --locked python -m unittest discover -s adapters/hermes/tests -v
```

Run the complete UID/socket suite on Linux. Windows skips those platform-specific cases. Real Codex integration tests require explicit `DOTS_LITE_REAL_CODEX=1`; they call a model and write to a temporary workspace. Build and service instructions are in the [deployment guide](docs/运行指南.md). Binaries are distributed through Releases rather than committed to the repository.

```text
cmd/hub/           Linux Hub entry point
cmd/executor/      Executor entry point
internal/          Protocol, authorization, sessions, files and SQLite state
adapters/hermes/   MCP, trusted-source plugin and background worker
skills/            Reusable deployment workflow and helpers
docs/              Usage, operations, design and acceptance records
```

## Contributing

Use Issues for bugs and proposals, and Pull Requests for changes. Include the version, OS, reproduction steps and redacted logs. Distinguish model execution, file acquisition and message delivery. Keep private keys, tokens, real chat identifiers and personal deployment records out of reports and commits.

Run relevant tests for code changes. Validate Linux permission and socket behavior on Linux. The [requirements](docs/需求.md) and [design](docs/详细实现方案.md) explain the architecture; detailed guides are currently in Chinese.

## License

Licensed under [BSD 3-Clause](LICENSE). Commercial use, modification and redistribution are permitted. Retain the copyright notice, license conditions and disclaimer when redistributing. Do not use the copyright holder's or contributors' names to endorse derived products without permission. Third-party components retain their own licenses.
