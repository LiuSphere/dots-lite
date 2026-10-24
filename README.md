# dots-duet-lite

云端 Agent 固定采用 hub-server 上的 Hermes，通过微信协调自己电脑上的 Codex 任务。参考 Dots 的本地任务控制与 Claude Duet 的任务、产物和通知组织，先做日常可用的轻量实现。

项目状态：Hub、执行端和 Hermes MCP 已实现，真实 Codex、跨机器文件传输及 Hermes 核验已通过。聊天交付与最终部署仍在验证；完整结果见[验收记录](docs/验收记录.md)和[实现进度](docs/实现进度.md)。

当前主线：各用户的 Codex 执行端运行专用控制服务，直接接入官方 Codex app-server；hub-server 上各 Hermes profile 只操作其用户绑定的执行端。当前先验证 Windows，其他用户执行端的操作系统尚未核实。重点是 Codex 会话控制与任务文件获取：支持产物自动回传，也允许 Hermes 主动查询、读取和下载授权任务文件。文件写入和通用 Shell 按具体场景后续评估。HAPI 保留为备选。

相关材料：[需求](docs/需求.md)、[详细实现方案](docs/详细实现方案.md)、[Hermes 运行说明](adapters/hermes/README.md)。需求定义范围与验收，方案解释实现方式，验收记录区分真实运行与协议替身。

## 开发与检查

```text
cmd/hub/                 Go 远端服务入口
cmd/executor/            Go 执行端入口
internal/protocol/       两端共用业务协议
internal/hub/            授权、任务、事件与业务跟进
internal/executor/       Codex 进程、会话与授权文件操作
internal/store/          SQLite 事务存储
adapters/hermes/         Python 薄适配包与 uv 环境
docs/                    需求与详细实现方案
```

Go 模块为 `dots-duet-lite`，使用 Go 1.27.3、WebSocket、SQLite 与系统进程控制。依赖及校验在 `go.mod` 和 `go.sum`。Hub 依赖 Linux Unix socket 的真实对端 UID，不在 Windows 上提供降级身份认证。

Python 适配包采用 uv，使用 Python `>=3.14,<3.15` 与 MCP 2.0.0；锁文件位于 `adapters/hermes/`。来源插件仅依赖标准库，可在 Hermes 自己的 Python 中加载。MCP worker 与 Hermes 环境分别安装，不复制模型、微信或管理员凭据。

在项目根目录执行：

```powershell
go mod download
go mod verify
go fmt ./...
go vet ./...
New-Item -ItemType Directory -Path dist -Force
go build -o dist/hub.exe ./cmd/hub
go build -o dist/executor.exe ./cmd/executor
uv sync --project adapters/hermes --locked
```

普通 `go test ./...` 不启动真实模型。显式设置 `DOTS_DUET_REAL_CODEX=1` 后，集成测试才在临时项目调用真实 Codex，验证文件及重启后继续原会话。执行端使用 `-config <私有 JSON>` 常驻运行，也可用 `-probe` 做无模型就绪检查。Hub 在 Linux 使用 `-config`、`-state`、`-socket`、`-listen` 启动；所有实际验收范围均见验收记录。

## 需求范围

首版以 [需求](docs/需求.md)的 R01—R13 为准：授权路由、机器/Codex 就绪查询、项目选择、多会话、稳定任务列表、追加与停止、审批与问题、异步通知、文件获取、结果核验、恢复、Hermes 集成与可靠的自动异常处理。

Hermes 主动跟进 Codex，先自行查证并处理目标、权限和恢复路径明确的异常。只有仍不明确、需要新授权、必须人工介入或达到恢复预算时才询问用户。关键事件必须可靠唤醒正确 profile，并记录实际处理、后续动作和核验；遗漏补查属于首版，不依赖用户另订阅定时简报。

项目卡片扩展、搜索、日志片段、Git 查询、命令模板、打包预览、待处理汇总和定时简报列为 E01—E08 后续增强，不是首版已具备或必须同时完成的功能。复用 Hermes 原有会话、后台、多 Agent、调度、记忆和 Skills，不另建云端 Agent 框架。

## 使用场景

在微信里告诉 Hermes：“在本机某个项目里完成这件事。”本机 Codex 执行任务，Hermes 回传进度、需要回答的问题和最终结果。用户可以继续追问、中途补充要求、停止当前执行，并在需要时审批操作。

## 已确认的需求

- 保留 hub-server 上的 Hermes 和微信入口。
- hub-server 上存在多个 Hermes profile，每个 profile 由不同的人使用，各自使用不同机器上的 Codex 执行端。这里的执行端指不同的电脑，不是同一机器上的不同进程或会话。这是用户确认的现有使用方式，不是待扩展的单用户模型。
- 任务实际在对应用户的执行端运行，使用该端的项目与 Codex 环境。当前 Windows 端作为首个验证对象，不能将其路径和凭据套用到其他执行端。
- 支持创建任务、查看状态、继续同一对话、中途补充要求、停止当前执行。
- 一个 profile 可以控制其绑定电脑上的多个已授权 Codex 会话，按项目、会话和当前回合准确选择目标；不将一台机器等同于一个会话。
- 提供稳定任务编号与列表，保存目标、项目、会话、执行、验证和交付状态；Hermes 切换 session 后仍可重新查询和选择，不只靠聊天记忆跟踪。
- 任务结束后，控制服务依据交付清单核对并回传授权产物，使 Hermes 能实际取得文件，而非只收到 Windows 路径。
- Hermes 可主动查询任务产物、列出授权目录、读取文本和下载文件，用于检查中间结果、补取遗漏文件和整理交付；各操作仍按用户/profile/机器/项目/任务校验权限。
- 支持将审批和需要用户回答的问题送回微信，并把回答送到正确请求。
- 已有资料可回答的问题由 Hermes 查证后回应；原任务内可明确处理的失败由它自动恢复和核验。必须用户决定的审批、目标与权限问题不代批或猜测。
- 支持断线后的状态核对，避免重复执行已经接收或结果不确定的任务。
- 将微信用户、Hermes profile、执行机器、项目目录和 Codex 会话明确关联。
- 本机控制权限仅授予指定用户和 profile，不随公共配置同步给其他用户。

## 第一阶段范围

先打通一个用户、其 Hermes profile、一个 Windows 执行端和一个项目的完整流程。数据与路由从开始就按用户、profile 和执行端区分，再用第二个用户及其另一台执行机器验证隔离。直接复用 Codex app-server 的会话与回合接口，实现最小 Hermes 适配、本机控制服务和必要的任务映射。

多 profile 对应不同用户与执行端属于当前范围；暂不增加公共注册、跨用户共享执行端、通用桌面操作或完整 Dots 替代品。

“远程运行本机 Codex”和“接管 Codex 桌面端已有活跃聊天”是不同能力。前者为首阶段目标；后者需要独立验证，不能因共享配置目录或能读取历史就认为已经实现。

## 当前技术方案

- hub-server：现有多个 Hermes profile；Python 编写薄工具/后台事件适配，Go 服务集中承担连接接收、授权路由、任务状态、可靠跟进、持久记录和通知。
- 每个用户的执行机器：在各自电脑上运行独立的 Go 控制服务、Codex app-server、项目和配置。当前先验证 Windows；其他系统适配不视为已经完成。
- 两端 Go 服务共用协议定义；Python 只桥接 Hermes 的可信上下文、工具与后台机制，不重复维护任务、去重和预算状态。文档与数据处理优先复用 Hermes 的 Python 工具和 Skills。
- Go 开发与构建采用官方安装和内置工具链选择，不使用 mise 或额外版本管理器。依赖使用 Go Modules，直接以 `go build`、`go test` 等原生命令构建和检查；执行端用户只需对应平台的控制服务可执行文件及 Codex/项目依赖。具体见实现方案第 3.5 节。
- 本机控制服务通过 stdio 与 app-server 进行双向 JSON-RPC 通信，不直接开放 app-server 的远程监听端口。
- 各执行端主动连接 hub-server，在受控网络上使用 WebSocket 接收操作、回传事件。当前优先沿用 Tailscale；各端网络接入条件需分别核实。
- 正式版本中，hub-server 使用 SQLite 保存可信用户、profile、执行端和聊天路由绑定；各执行端的本地 SQLite 保存其会话映射、请求结果和待回答请求。最小链路验证可先使用内存，但不承诺重启恢复。断线恢复后先核对状态，不盲目重发结果不确定的操作。
- SSH 保留作维护入口，任务不再依赖双向 SSH 命令拼接。
- Hermes 理解用户要求、选择任务、诊断异常并检查恢复结果；适配程序维护可靠唤醒、处理确认、后续动作与遗漏补查；本机服务调用 Codex 接口并返回真实状态。文件和命令由 Codex 在工作区内处理。
- 机器就绪、项目列表、任务快照和简单文件查询由程序直接返回，不为每次查询启动 Codex 模型任务。首版通过微信文本和 Hermes 工具显示任务列表，不另造控制台。
- 本机服务另外提供任务文件获取接口。产物经它传到 hub-server 后，Hermes 得到该 profile 可访问的副本或附件引用，再检查、整理或发回微信；这些接口由我们实现，不是假定 Codex 已自动上传文件。

本轮已经实现这条直接接入链路。HAPI 仍为备选，不同时维护第二套后端。恢复历史会话与接管桌面已有活跃聊天不同，后者尚未验收。

### 多 profile 与执行端隔离

路由关系为：消息渠道中的可信用户与聊天 → 实际处理消息的 Hermes profile → 已授权执行端 → 该端项目与 Codex 会话。profile 名称、模型生成的参数和消息中自称的身份，都不能代替服务端维护的授权绑定。

- 各执行端分别认证；凭据只允许注册到对应用户和 profile，不能通过自报执行端 ID 覆盖其他用户的连接。
- 每次查询、创建、继续、追加、停止、审批和文件收发都校验完整归属。会话列表也只从已授权执行端查询，不汇总给其他 profile。
- 任务、会话、回合、审批、事件、去重记录和通知均带用户/profile/执行端范围，不能只用一个裸会话 ID 或审批编号定位操作。
- 事件按原任务归属返回原 profile 和微信聊天；某端离线时只报告该端不可用，不自动转给其他用户的执行端。重连也必须恢复原绑定。
- 执行端凭据、Codex 登录、`CODEX_HOME`、项目路径和本地数据库分别保存。公共工具代码可以复用，授权配置和用户数据不能随公共配置同步。

应用授权已实现并经过隔离账号测试。正式环境仍须用独立 Linux UID 和私有目录部署，不能只靠 profile 名称提供操作系统隔离。

同一 profile 绑定的电脑内可以存在多个 Codex 会话。控制服务按实际 `threadId` 和 `turnId` 调度，聊天当前选择不由后台事件覆盖。第一版计划一台电脑使用一个受控 app-server 管理多个会话；独立进程及原生桌面活跃会话的接入另行验证。

### Codex 控制接口

| 操作 | 官方接口或机制 |
| --- | --- |
| 查找会话、读取历史 | `thread/list`、`thread/read`、`thread/turns/list`、`thread/items/list` |
| 创建、恢复会话 | `thread/start`、`thread/resume` |
| 发送要求并启动回合 | `turn/start` |
| 执行中追加要求 | `turn/steer` |
| 停止当前回合 | `turn/interrupt`，随后核对实际终止状态 |
| 接收进度、回答问题和审批 | 事件通知、服务端请求与对应响应 |

接口按实际采用的 Codex 版本核对。只能控制所连接实例中的会话；恢复历史与接入其他进程正在运行的会话是不同能力。若要求微信与原生桌面端同时操作同一活跃聊天，必须先验证桌面运行实例的连接方式，不通过另起一个 app-server 模拟接管。

## 开源实现与参考

2026-10-22 的资料检查结论如下。项目文档和部分源码已查阅，尚未在本机完成安装或端到端验证；开始开发前需重新确认版本和接口。

| 实现 | 可复用部分 | 限制与定位 |
| --- | --- | --- |
| [OpenAI Codex app-server](https://learn.chatgpt.com/docs/app-server) | 对话生命周期、运行事件、中途引导、停止、审批 | 当前主线。通过本机 stdio 接入，自行承担远程连接、运行管理和恢复。 |
| [tiann/hapi](https://github.com/tiann/hapi) | Hub、Runner、共享会话与远程客户端 | 备选。共享终端会话不等于接管原生桌面聊天；其文档注明原生 Windows 执行仍需验证。 |
| [uluckyXH/Chat-Codex](https://github.com/uluckyXH/Chat-Codex) | app-server 适配、聊天与会话绑定、审批和文件回传 | 实现参考，尚未安装或验证。不因参考它而替换现有 Hermes 微信入口。 |
| [Dimillian/CodexMonitor](https://github.com/Dimillian/CodexMonitor) | 远端后台、多项目、会话和审批处理 | 完整桌面客户端，可参考其实现，不作为首阶段另造界面的理由。 |
| [lucianlamp/codex-monitor](https://github.com/lucianlamp/codex-monitor) | 外部事件投递、app-server 连接、Windows 接收方式 | 与上一项目不同。其文档对 Windows 原生 App 采用 hook 等方式，不能视为通用桌面接管已经可用。 |
| [slopus/happy](https://github.com/slopus/happy) | 手机与终端继续工作、同步服务 | 备选。需要区分原版 CLI 与新的 Happy Harness。 |

重点接口资料：

- [Codex app-server API](https://learn.chatgpt.com/docs/app-server#api-overview)
- [Dots 的本地任务与持续跟进能力](https://learn.chatgpt.com/docs/dots/tasks-and-memory)
- [Chat-Codex 实现说明](https://github.com/uluckyXH/Chat-Codex)
- [HAPI 架构](https://github.com/tiann/hapi/blob/main/docs/guide/how-it-works.md)
- [Codex 共享会话及生命周期](https://github.com/tiann/hapi/blob/main/docs/guide/codex-shared-sessions.md)
- [HAPI REST 接口](https://github.com/tiann/hapi/blob/main/docs/api/client-contract/rest.md)
- [HAPI SSE 与断线恢复](https://github.com/tiann/hapi/blob/main/docs/api/client-contract/sse.md)

## 需要自己实现的部分

1. Hermes 工具：查询机器与项目、列出稳定任务、创建、继续、补充和停止任务；分别展示执行、等待、验证和交付状态，不能把回合结束等同于目标已完成。
2. 持久化映射：hub-server 保存用户/profile/执行端和聊天路由授权，各执行端保存本地任务与会话记录；恢复后在原归属范围内按实际结果更新标识。
3. 事件与跟进：持久记录关键事件、Hermes 投递/处理句柄与决定、后续动作及核验结果。常驻补查发现遗漏或未完成处理；投递不等于处理完成。可自行解决的失败先自动恢复，需要用户决定、持续阻塞或最终结果再通知；不能假定 app-server 提供 HAPI 的 SSE 游标回放。
4. 问题与审批：可查证的问题由 Hermes 自行回答；必须用户决定的请求绑定用户、profile、执行端、会话和有效请求再转交。模型不能冒充用户批准或将模糊同意扩成全局授权。
5. 执行端运行管理：保证远程任务不依赖手动打开的终端；区分网络断开、进程退出和任务失败。
6. 附件与结果：将微信附件送入正确工作区；按交付清单自动回传产物；提供授权目录查询、文本读取与文件下载工具，使 Hermes 可主动检查和补取文件，再通过微信交付。

优先使用 app-server 已有能力；控制服务补跨机器连接、身份与会话映射、持久记录、通知和任务文件获取，不重复实现 Codex 的执行引擎。首版不以取文件为由增加任意文件写入或通用 Shell 接口。

## 第一阶段验收

- 从微信提交任务，本机在指定项目内产生可检查的实际结果。
- 后续消息进入同一会话，并能正确使用前文。
- Hermes 换 session 后能从任务列表找到原工作，正确继续对应 Codex 会话；机器在线但 Codex 未就绪时如实报告。
- 执行中追加要求和停止均实际生效；停止后仍可继续对话。
- 触发一次真实审批，并分别验证用户批准与拒绝的实际效果。
- 断开连接后恢复，状态可以核对，任务不被盲目重发，最终结果不因重复通知而混乱。
- 用两个用户、两个 profile 和两台不同机器上的 Codex 执行端分别验证正确路由；用户 A 无法查询、继续、停止或审批用户 B 的任务，也无法获取其文件、事件和通知。
- 验证伪造目标标识、重复请求和重连不能越过绑定；一个执行端离线时，不会改派到另一用户的执行端。
- 需要交付的文件可从微信取得，而非仅得到本机路径。
- Hermes 实际取得自动回传的产物，并可主动查询、读取及下载遗漏文件；其他用户文件和授权范围外路径访问被拒绝。
- 分别记录 Codex 执行结果、Hermes 文件获取结果和微信交付结果；文件传输失败时只重试对应传输，不重新执行原任务。
- 回合正常结束但检查失败或证据不足时，回报实际失败或待核验，不报告目标已完成。完整场景与对应编号见需求文档，原型演示不等于首版通过。
- 已知可恢复异常无需用户逐次介入：Hermes 完成诊断、恢复和真实核验；不明确或必须人工处理的情况才带事实与具体问题升级。
- 验证事件未投递、Hermes 未处理/中途退出、动作已发生但确认丢失时仍能补查恢复，无重复副作用。重复无进展、预算耗尽或用户暂停/取消后不自动继续。

桌面已有会话接管单独记录验证结论，不作为上述能力已完成的推论。

## 下一步与待决事项

1. 按运行指南登记实际消息来源、用户身份、项目和执行端，不从 profile 目录名称推断授权。
2. 在两名真实用户的两台执行电脑上验证路由、会话、项目、恢复和交叉拒绝；目前一台 Windows 实机的结果不能替代此项。
3. 通过实际微信消息验证输入附件、真人批准和拒绝、原聊天通知及输出附件，并逐项核对真实回执。
4. 桌面已有活跃会话接管单独验证；当前独立 app-server 会话控制不证明该能力。

Python 薄适配、Go Hub 与执行端、本机 stdio、跨机器 WebSocket 和 SQLite 均已有实现。真实 Codex、hub-server 跨机器取文件与 Hermes 核验已有运行证据；原聊天发送复用官方函数，实际微信收发和第二台用户电脑仍需现场验收。配置与常驻方法见[运行指南](docs/运行指南.md)，各项证据及边界见[验收记录](docs/验收记录.md)。
