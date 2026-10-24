# Hermes 薄适配

此包把 Hermes 的原生 MCP、人工确认和后台 runs 接到 Go Hub。Hub 保存授权、任务和业务事件；适配不创建第二套 Agent 循环，也不替代 Hermes 的原生 run 存储。

## 运行

在项目根目录安装锁定依赖并检查：

```powershell
uv sync --project adapters/hermes --locked
uv run --project adapters/hermes python -m unittest discover -s adapters/hermes/tests -v
```

发行包使用 Python `>=3.14,<3.15` 与 `mcp==2.0.0`。Windows 不支持本包要求的 UID Unix socket；其 socket 集成测试明确跳过，须在 Linux 运行。

为每个隔离 Linux UID 配置独立的 `DOTS_DUET_SOCKET`。Hermes 原生 MCP 配置的服务器名必须为 `dots_duet`，命令为该包环境的 `python -m dots_duet_lite_hermes`。标准输出只传 MCP 消息。不要把 socket 路径、profile 身份或调用引用交给模型选择。

把 `src/dots_duet_lite_hermes/` 作为 Hermes 官方目录插件加载。`plugin.yaml` 声明 `pre_gateway_dispatch`、`post_gateway_admission` 与 `pre_tool_call`，`__init__.py` 是注册入口。插件仅使用标准库，不要求把 MCP SDK 安装进 Hermes 的 Python 环境。插件必须运行在原生消息任务内；缺少当前绑定或可信来源时明确阻止调用。

后台跟进使用独立进程：

```text
python -m dots_duet_lite_hermes worker --socket /run/duet/profile.sock --api-url http://127.0.0.1:8642 --api-token-file /private/profile/api-token --hermes-python /opt/hermes/venv/bin/python --hermes-source /opt/hermes/source
```

加 `--once` 只处理一次，失败退出非零。常驻模式每五秒查询，错误写标准错误。API URL 固定来自运维配置，不由模型传入；不跟随重定向。token 文件在 Linux 必须不向组或其他用户开放。原生 API 必须声明持久幂等支持，否则适配拒绝启动模型请求。

## 工具与可信来源

提供七个工具：`duet_query`、`duet_prepare`、`duet_submit`、`duet_stop`、`duet_answer`、`duet_approve`、`duet_record`。精确参数以 `schemas.py` 为准，不接受额外字段。

`duet_query(query="task")` 返回业务任务；`query="session"` 返回当前原生会话、任务与待处理请求；`query="goal"` 返回真实原生 Goal。核验线程是否静止以及是否仍有 active Goal 时，读取后两项，不从业务快照推测。

输入附件使用 `duet_prepare(action="task.create",inputs=[{name,local_path}],...)`，或 `duet_prepare(action="files.upload",task_id=...,upload={name,local_path})`，取得 operation ID 后再 `duet_submit`。模型只传当前消息附件的路径引用。名称只允许 basename，支持空文件，总量最大 8 MiB。执行端将附件写入任务的 `.dots-duet/attachments/<task-id>/<name>`，拒绝覆盖；prepare 本身不写文件。Hub 与执行端继续校验内容哈希、实际大小和任务归属。

pre-dispatch 只收集该消息的原生媒体路径，不能授予读取权。post-admission 经 Hermes 授权后，按平台、发送者、聊天、线程、profile、scope、父聊天及 message ID 精确配对；从官方媒体缓存逐级用不跟随符号链接的句柄读取稳定快照。原始路径与官方内容寻址保留路径均只绑定该条消息。工具调用还须匹配当前原生 session key 和消息。重启后登记丢失则拒绝；不会扩大到整个 HERMES_HOME。字节只进入私有 Invocation.attachment_files HTTP body，不进入 hook 修改参数、模型输入或公开 operation 结果。

文件必须归当前操作系统用户所有，并且只有一个硬链接。候选与已授权登记均在收集后 300 秒过期；过期、缓存淘汰或换消息后须重新走原生入站流程。登记缓存有固定上限，淘汰后明确拒绝读取，不复用旧来源。

插件读取原生 `bound_session_key()`，再取当前平台、发送者、聊天、线程、会话、消息及原生任务/调用 ID。插件用 `POST /v1/invocations` 注册原始工具与参数，覆盖模型伪造的 `_context_ref`。MCP 只把登记引用和同一组参数送至 `POST /v1/tools/call`，不接收模型自报的用户、profile 或来源。

API 后台来源的 `chat_id` 为预先绑定的 API session 根，`session_id` 可能是原生压缩后的实际会话。Hub 按预绑定根校验，不用请求返回的 session 反向创建归属。

`duet_approve` 从 Hub 获取原生待审批实例，然后请求 MCP elicitation。只有真实 `accept` 或 `decline` 才提交同一实例。取消、客户端无人工确认能力或结构化权限请求均保持 pending。模型参数中的 `decision`、`approved` 无效。写出确认也不证明原生操作已完成。

## 文件与跟进结果

`duet_query(query="download")` 对收到的 Base64、返回字节数和 SHA-256 进行校验，再写入本 profile 私有目录。默认目录为 `$HERMES_HOME/dots-duet/files`，可用 `DOTS_DUET_FILES_DIR` 配置。Linux 目录须为 700，文件为 600。写入后重新读取核对哈希。

返回结果包含实际 `local_path`，不含 `data_base64`。单文件上限为 8 MiB。只有 offset 为 0、字节数等于全文件大小且 `truncated=false` 才报告 `acquired=complete`；部分范围报告 `partial`。文件获取与微信附件交付是两个状态，私有副本不证明已送到微信。

worker 使用 Hub 已保存的完整 request、session 和 idempotency key。响应丢失后重试仍用同一请求；已有 run ID 则只查询该 run。它核对原生 session 后登记结果。原生 completed 仅说明原生运行结束，业务完成仍需实际动作与核验证据。

每次 worker 先查询 `GET /v1/artifacts/next`。待传产物必须是完整文件；通过上述私有落地校验后，worker 用 `POST /v1/artifacts/{id}/acquired` 回报真实路径、哈希、大小和完整获取状态。失败则记录 `acquired=false` 和具体错误，并让错误可见。文件传输不需要启动模型，也不重跑原任务。模型跟进请求从创建时就不含 Base64；自动传输不改变已保存的 request 或幂等键。

完成待获取文件后，worker 查询 `GET /v1/deliveries/next`，再处理后台模型跟进。Hub 在返回发送单元前持久化 dispatching 与 attempt ID。每个单元只含文本或一个已获取文件，目标来自原任务的平台、聊天和线程。worker 不让模型选择发送命令、Hermes 源码或目标。`duet_query(query="deliveries",task_id=...)` 只查看本任务回执。

发送使用固定 Hermes Python 子进程和官方 `tools.send_message_tool.send_message_tool`。stdin 只传可信 outbox JSON；不另写微信协议或消息循环。helper 精确核对原目标，拒绝回退到 home channel。文本不得含 MEDIA 指令。文件发送前重新检查私有目录、所有者、权限、大小和 SHA-256。随后把已验证句柄读出的字节写入同 profile 私有随机目录，保留文件名并设为只读；原生发送只读取这份快照。快照在整个调用期间保留，发送前后核对，并在结束后清理。原产物路径被替换不会改变实际发送的字节。原生解析必须得到一个文件与空正文。

原生回执原样保留。成功须有 `success=true` 和非空 message ID，且无 partial、dropped 或 warnings。已核对的官方 Weixin 单文件契约另生成绑定 artifact ID、哈希、大小与目标的 verification，不伪造原生 `media_delivered`。原生若明确否认媒体送达，或其他平台的回执不足以证明媒体送达，则保持 unknown。超时、异常和不完整回执不重发；只有发送前明确失败才报告 definitely_not_sent，让 Hub 在预算内重试。

## hub-server 实测范围

以下目录是本轮测试的历史证据位置。临时测试运行时、账户、socket 与副本将在验收后清理，不作为永久可访问接口。最终留存证据由项目验收记录说明。

测试开始时，安装版 Hermes 为 `f97608f178d1ffeca59860195ab7da295f7c8e5f`，其 Python 为 3.13.16。该版缺少 `bound_session_key()`，本插件拒绝放行。此前未修改其核心或现有网关配置；这条历史结果不描述后续正式升级状态。

独立测试目录固定了官方源码 `5894e750c3dba980d43b25aa29e3958149ba2c61`。官方 PluginManager 已验证插件加载、原生消息绑定注册、无绑定拒绝及陈旧环境不复用。该契约测试使用外部 socket 接收端，并非真实 Go Hub。

另一个私有测试 home 已运行真实原生 API/model 请求，取得 `DDL_HERMES_RUN_OK`；同键请求复用同一 run，重建 API adapter 后结果可恢复。证据位于 hub-server 的 `/home/owner-a/.cache/dots-duet-test-20261024/native-api-home/native-smoke-result.json`。它没有发送微信，也没有替换现有 Hermes 安装。

适配测试须分别记录 Windows 与 Linux 结果。hub-server 原生 Python 3.13.16 按源码运行验证源码和 MCP 2.0.0，不表示 Python 3.14 发行包已安装到现网。实际 Go Hub 和微信交付须单独记录，不用上述外部接收端和原生 API 测试替代。

`tests/hub_mcp_smoke.py` 是显式运行的真实 Hub 检查，须由绑定测试 UID 执行，并传入测试 socket 与预登记的平台、发送者和聊天。它验证真实 MCP 查询、参数引用不可更换和未绑定来源拒绝。它使用明确的测试来源，不声称该来源来自真实微信消息。

hub-server 的独立公开运行环境位于 `/private/test-environment`，为 Python 3.14.8、MCP 2.0.0。其第三方 wheel 按本包锁文件下载，安装前后均校验 SHA-256；清单位于同级 `mcp-wheels/manifest.json`。附件切片完成后，实际绑定 UID 1002 下完整 29 项适配测试通过，包括硬链接拒绝与登记过期。Windows 同一套 29 项中 19 项通过、10 项明确跳过 Linux 接口。真实 Hub/MCP 查询也已通过。此环境不含模型或微信凭据。

`tests/native_hub_smoke.py` 验证独立私有 home 中的真实官方 Hermes 模型、MCP、来源插件、Go Hub 与产物获取。它从原生会话接口读取实际工具调用，要求模型查询 session、Goal 和指定文本文件，并把实际标记和哈希记录为核验证据。脚本自身还读取私有副本核对字节。它不会把模型宣称完成当作测试通过，也不发送微信。

直接嵌入 `APIServerAdapter` 的测试脚本须在同一进程调用官方 `discover_mcp_tools`，完整网关入口原本承担此启动步骤。另一个进程完成工具发现，不会填充当前模型进程的工具注册表。此测试 home 用官方 `tools.tool_search.enabled: off` 配置直接展示七个工具，便于逐条核对原生会话；默认 Tool Search 桥接模式须单独验证。

原任务 `task_example` 的真实测试先暴露了上述启动遗漏。首个原生 run 明确报告工具不可用，未做任何工具调用。修复测试入口后，Hub 在预算内创建新的诊断 attempt；旧 run、session、key 和 request 保持不变。实际模型通过官方 MCP 和来源插件调用 session、goal、text、verification 与 followup 记录，原生 run 结束且跟进为 handled。没有重新提交 Codex 创建或继续操作。

模型读取的原生会话状态为 `notLoaded`，Goal 接口仍实际返回 `{"goal":null}`。它读取授权文件，并把实际标记与 SHA-256 写进核验证据；测试另核对私有副本精确为标记加一个 LF，共 33 字节。结果与完整原生会话证据分别保存在上述私有测试 home 的 `native-hub-result.json`、`native-hub-transcript.json`，权限为 600。首次失败会话另存 `native-hub-first-attempt-transcript.json`。

该次实测也暴露了同任务 `query=followups` 被 API 授权误拒的问题。Hub 修复后，`tests/hub_api_followups_smoke.py` 用保留的两个 API 会话、显式测试来源，经真实 MCP stdio 与 Hub 成功读取原任务两条记录；全局和其他任务查询仍被拒绝。复验没有调用模型，不改写原模型“无法独立读取旧收据”的实际记录。测试 API 已断开，私有测试进程已退出。没有微信交付证明。

`tests/native_delivery_smoke.py` 在隔离 home 使用官方发送函数，仅通过公开 platform registry 替换外部网络 sender。fixture 实际收到同一 19 字节二进制文件和空正文；还覆盖错误路径、目标、MEDIA 注入、损坏哈希，以及不足以证明送达的原生回执。该结果不是实际微信送达。固定 Python/helper subprocess 的 stdin 边界也已运行验证。

`tests/hub_native_delivery_smoke.py` 已在 `5894e750c3dba980d43b25aa29e3958149ba2c61` 运行真实三 hook、原生 retained media、私有附件注册、Go Hub、Windows Codex 二进制复制、worker 获取和官方发送 helper。只有入站渠道与出站网络使用 fixture；模型确实执行了文件复制。结果产物为 19 字节，SHA-256 为 `0160ee28442ea402e2861d477ea99cd321c991d144e3b254b5afe6543e903380`。文本和媒体各有一次发送 attempt，Hub 保留原始 fixture receipt 并标记 delivered。脚本末尾曾误读 stop 返回字段，清理后的状态已独立回读确认 paused；恢复证据为 `hub-native-delivery-recovered.json`。没有为修正该测试脚本重跑模型。Linux 临时副本随测试清理，结果不证明微信网络送达。

官方 stable `v0.22.5`、commit `bf0203a794a07aac3ff764972ab9087baa4e4f4e` 的 Python 3.14.8 隔离运行时另通过两个无模型契约复验：`tests/native_ingress_smoke.py` 验证真实 PluginManager 三 hook、ContextVar、retained media、缺少绑定拒绝及仅私有 HTTP 注册含字节；`tests/native_delivery_smoke.py` 验证官方单文件发送和回执规则。前者的 HTTP 接收端是明确的外部测试 fixture，后者只替换外部网络 sender，不把这些契约复验称作 stable 上重跑完整模型链路。证据为 `stable-ingress-contract-result.json` 与 `stable-delivery-contract-result.json`。
