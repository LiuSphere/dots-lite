# Hermes 薄适配

此包把 Hermes 的原生 MCP、人工确认和后台 runs 接到 Go Hub。Hub 保存授权、任务和业务事件；适配不创建第二套 Agent 循环，也不替代 Hermes 的原生 run 存储。

首次接入请先看[部署与运行指南](../../docs/运行指南.md)，微信中的日常操作见[使用指南](../../docs/使用指南.md)。本文件说明适配协议和测试边界，不代表生产用户、电脑和项目已绑定。

## 运行

在项目根目录安装锁定依赖并检查：

```powershell
uv sync --project adapters/hermes --locked
uv run --project adapters/hermes python -m unittest discover -s adapters/hermes/tests -v
```

发行包使用 Python `>=3.14,<3.15` 与 `mcp==2.0.0`。Windows 不支持本包要求的 UID Unix socket；其 socket 集成测试明确跳过，须在 Linux 运行。

为每个隔离 Linux UID 配置独立的 `DOTS_LITE_SOCKET`。Hermes 原生 MCP 配置的服务器名必须为 `dots_lite`，命令为该包环境的 `python -m dots_lite_hermes`。标准输出只传 MCP 消息。不要把 socket 路径、profile 身份或调用引用交给模型选择。

v0.3.0 只接受当前名称：MCP 配置键 `dots_lite`、工具前缀 `dots_lite_*`、插件名 `dots-lite`、环境变量 `DOTS_LITE_SOCKET` 和 `DOTS_LITE_FILES_DIR`。默认文件目录为 `$HERMES_HOME/dots-lite/files`。配置和工具只使用 folders/folder_ids/folder_id。更新前离线转换现有配置与状态，随后同步切换两端服务；不提供旧名称回退。

来源插件所在的 Hermes 网关父进程也必须配置 `DOTS_LITE_SOCKET`，不能只设置 MCP 子进程的 env。网关、MCP 和 worker 使用同一 profile UID、HOME、HERMES_HOME 与 DOTS_LITE_FILES_DIR；socket 可由同一个 Hub 提供，再按真实对端 UID 授权。

把 `src/dots_lite_hermes/` 作为 Hermes 官方目录插件加载。`plugin.yaml` 声明 `pre_gateway_dispatch`、`post_gateway_admission` 与 `pre_tool_call`，`__init__.py` 是注册入口。插件仅使用标准库，不要求把 MCP SDK 安装进 Hermes 的 Python 环境。插件必须运行在原生消息任务内；缺少当前绑定或可信来源时明确阻止调用。

后台跟进使用独立进程：

```text
python -m dots_lite_hermes worker --socket /run/duet/profile.sock --api-url http://127.0.0.1:8642 --api-token-file /private/profile/api-token --hermes-python /opt/hermes/venv/bin/python --hermes-source /opt/hermes/source
```

加 `--once` 只处理一次，失败退出非零；它仍可能调用模型或发送原聊天队列，不能用作只读健康检查。常驻模式每五秒查询，错误写标准错误。API URL 固定来自运维配置，使用根地址、不加 `/v1`，不由模型传入；不跟随重定向。token 文件只放裸 token，在 Linux 必须不向组或其他用户开放。原生 API 必须声明持久幂等支持，否则适配拒绝启动模型请求。

## 工具与可信来源

提供七个工具：`dots_lite_query`、`dots_lite_prepare`、`dots_lite_submit`、`dots_lite_stop`、`dots_lite_answer`、`dots_lite_approve`、`dots_lite_record`。精确参数以 `schemas.py` 为准，不接受额外字段。

`dots_lite_query(query="task")` 返回业务任务；`query="session"` 返回当前原生会话、任务与待处理请求；`query="goal"` 返回真实原生 Goal。核验线程是否静止以及是否仍有 active Goal 时，读取后两项，不从业务快照推测。

输入附件使用 `dots_lite_prepare(action="task.create",inputs=[{name,local_path}],...)`，或 `dots_lite_prepare(action="files.upload",task_id=...,upload={name,local_path})`，取得 operation ID 后再 `dots_lite_submit`。模型只传当前消息附件的路径引用。名称只允许 basename，支持空文件，总量最大 8 MiB。执行端将附件写入任务的 `.dots-lite/attachments/<task-id>/<name>`，拒绝覆盖；prepare 本身不写文件。Hub 与执行端继续校验内容哈希、实际大小和任务归属。

pre-dispatch 只收集该消息的原生媒体路径，不能授予读取权。post-admission 经 Hermes 授权后，按平台、发送者、聊天、线程、profile、scope、父聊天及 message ID 精确配对；从官方媒体缓存逐级用不跟随符号链接的句柄读取稳定快照。原始路径与官方内容寻址保留路径均只绑定该条消息。工具调用还须匹配当前原生 session key 和消息。重启后登记丢失则拒绝；不会扩大到整个 HERMES_HOME。字节只进入私有 Invocation.attachment_files HTTP body，不进入 hook 修改参数、模型输入或公开 operation 结果。

文件必须归当前操作系统用户所有，并且只有一个硬链接。候选与已授权登记均在收集后 300 秒过期；过期、缓存淘汰或换消息后须重新走原生入站流程。登记缓存有固定上限，淘汰后明确拒绝读取，不复用旧来源。

插件读取原生 `bound_session_key()`，再取当前平台、发送者、聊天、线程、会话、消息及原生任务/调用 ID。插件用 `POST /v1/invocations` 注册原始工具与参数，覆盖模型伪造的 `_context_ref`。MCP 只把登记引用和同一组参数送至 `POST /v1/tools/call`，不接收模型自报的用户、profile 或来源。

API 后台来源的 `chat_id` 为预先绑定的 API session 根，`session_id` 可能是原生压缩后的实际会话。Hub 按预绑定根校验，不用请求返回的 session 反向创建归属。

`dots_lite_approve` 从 Hub 获取原生待审批实例，然后请求 MCP elicitation。收到原生确认流程的 `accept` 或 `decline` 后提交同一实例。取消、MCP 确认调用不可用或结构化权限请求保持 pending。Hermes 渠道缺少通知回调或通知失败时可能返回 `decline`，因此没有显示确认界面不等于仍在 pending，应查询实际请求状态。模型参数中的 `decision`、`approved` 无效。写出确认也不证明原生操作已完成。

## 文件与跟进结果

`dots_lite_query(query="download")` 对收到的 Base64、返回字节数和 SHA-256 进行校验，再写入本 profile 私有目录。默认目录为 `$HERMES_HOME/dots-lite/files`，可用 `DOTS_LITE_FILES_DIR` 配置。Linux 目录须为 700，文件为 600。写入后重新读取核对哈希。

返回结果包含实际 `local_path`，不含 `data_base64`。单文件上限为 8 MiB。只有 offset 为 0、字节数等于全文件大小且 `truncated=false` 才报告 `acquired=complete`；部分范围报告 `partial`。文件获取与微信附件交付是两个状态，私有副本不证明已送到微信。

worker 使用 Hub 已保存的完整 request、session 和 idempotency key。响应丢失后重试仍用同一请求；已有 run ID 则只查询该 run。它核对原生 session 后登记结果。原生 completed 仅说明原生运行结束，业务完成仍需实际动作与核验证据。

每次 worker 先查询 `GET /v1/artifacts/next`。待传产物必须是完整文件；通过上述私有落地校验后，worker 用 `POST /v1/artifacts/{id}/acquired` 回报真实路径、哈希、大小和完整获取状态。失败则记录 `acquired=false` 和具体错误，并让错误可见。文件传输不需要启动模型，也不重跑原任务。模型跟进请求从创建时就不含 Base64；自动传输不改变已保存的 request 或幂等键。

完成待获取文件后，worker 查询 `GET /v1/deliveries/next`，再处理后台模型跟进。Hub 在返回发送单元前持久化 dispatching 与 attempt ID。每个单元只含文本或一个已获取文件，目标来自原任务的平台、聊天和线程。worker 不让模型选择发送命令、Hermes 源码或目标。`dots_lite_query(query="deliveries",task_id=...)` 只查看本任务回执。

发送使用固定 Hermes Python 子进程和官方 `tools.send_message_tool.send_message_tool`。stdin 只传可信 outbox JSON；不另写微信协议或消息循环。helper 精确核对原目标，拒绝回退到 home channel。文本不得含 MEDIA 指令。文件发送前重新检查私有目录、所有者、权限、大小和 SHA-256。随后把已验证句柄读出的字节写入同 profile 私有随机目录，保留文件名并设为只读；原生发送只读取这份快照。快照在整个调用期间保留，发送前后核对，并在结束后清理。原产物路径被替换不会改变实际发送的字节。原生解析必须得到一个文件与空正文。

原生回执原样保留。成功须有 `success=true` 和非空 message ID，且无 partial、dropped 或 warnings。已核对的官方 Weixin 单文件契约另生成绑定 artifact ID、哈希、大小与目标的 verification，不伪造原生 `media_delivered`。原生若明确否认媒体送达，或其他平台的回执不足以证明媒体送达，则保持 unknown。超时、异常和不完整回执不重发；只有发送前明确失败才报告 definitely_not_sent，让 Hub 在预算内重试。

## 测试范围

普通单元测试不调用模型。Linux 执行 Unix UID、socket 和权限检查；Windows 明确跳过不支持的接口，不能用跳过结果代替 Linux 验收。测试环境使用独立账号或临时 HOME，来源与外部发送器使用明确 fixture，不能注册为生产绑定。

以下 smoke 脚本需显式提供独立测试环境：

- `tests/hub_mcp_smoke.py`：真实 Go Hub、MCP stdio、可信引用、参数及来源拒绝。
- `tests/native_ingress_smoke.py`：官方插件三 hook、原生媒体上下文和私有字节传输；不调用模型。
- `tests/native_delivery_smoke.py`：官方发送函数配合外部网络 fixture，不证明真实微信送达。
- `tests/native_api_smoke.py`、`tests/native_hub_smoke.py`：真实原生 API/模型与工具核验，需要明确模型使用授权。
- `tests/hub_native_delivery_smoke.py`：输入附件、真实 Codex 执行、worker 获取和官方发送 helper 的组合流程；渠道 fixture 与真实微信分别记录。

嵌入 `APIServerAdapter` 的测试入口需在同一进程完成官方 MCP discovery，另一进程的发现不会填充当前注册表。直接展示工具与 Tool Search 需分别检查，不为通过测试擅自更改现有配置。

公开验证摘要见[验收记录](../../docs/验收记录.md)。真实会话、任务、来源、凭据与主机目录保存在私有记录中，仓库只保留可重跑脚本和证明范围。


## 协议 4 适配（0.4.0.dev0）

Hub、执行端和本包须协调升级。当前消息的可信完整绝对目录或 thread ID 由插件私下登记，API 跟进不能新增目录/原生线程。query=threads/thread 提供原生清单与元信息；prepare action=task.attach 接入原线程并保留暂停，后续明确用户消息才能继续。

跟进提交前登记 /followups/<id>/submission，保留固定 session/key/body。持久幂等期已过而结果未知时停止 POST，已知 run 继续查询。handled 与核验使用实际 observation_id/operation_id，waiting 使用可观察条件和未来 check_at。正常阶段不受整项任务累计次数限制；运行数、提交次数和底层模型请求数分别记录。

维护中的 Hub 不派发新跟进。恢复服务也不会解除任务暂停。源码本地测试通过不代表生产服务、真人审批或微信送达已经验收。
