# 部署操作与验收

以选定源码为准：读取 `cmd/hub`、`cmd/executor`、`internal/protocol`、`adapters/hermes` 和锁文件；仓库有部署文档时，查阅 `docs/运行指南.md`、`docs/使用指南.md`、`docs/部署记录模板.md`。历史路径与版本不是通用安装参数。以下保留不易从单个配置看出的约束。

## 绑定与工作目录

- Hub 新配置用 `bindings[].folder_ids`，执行端用 `folders=[{id,root,...}]`，ID 对应一致。root 是本机授权根的绝对路径。旧 projects/project_ids 仅作兼容输入，不能同时配置两套列表。
- 当前一个 Linux UID 只有一条 binding，绑定一台 executor，可授权多个根。增加第二台电脑前核对实现是否支持，不能复制同 UID 的 binding 绕过限制。
- scope 的 user_id/profile_id/executor_id、token、可信来源共同完成配对。执行端主动连接 Hub，不依赖服务器 SSH 登录电脑。重启沿用 token、scope 和 state_path，自动复连。
- 工具使用 folder_id；task.create 的 working_directory 是根内已有相对目录，以 `/` 分隔。省略为根 `.`，拒绝绝对路径、盘符、`..`、缺失目录和越界链接。创建后固定；附件、文件读取及产物相对该任务目录。
- 多个授权根生成独立稳定 ID，检查实际路径与重叠。启动脚本的校验须使用本次完整清单，不能复制只接受某个历史单项目的脚本。

## 版本、构建与回滚

安装用户选定或确认的版本，记录精确提交与二进制 SHA-256。未给版本时检查原部署和项目默认来源后提出具体版本，不以同名第三方包代替源码。依赖按锁文件安装。Hub 与执行端需支持相同协议及工作目录语义，不能接入会忽略 working_directory 的旧执行端。

先构建对应架构程序和锁定的适配环境，再修改现网。检查安装路径、配置和启动入口的语法。缺少产品功能时报告边界，不在部署中默写新协议或恢复框架。

更新前记录代码、程序、配置、服务定义、权限/所有者、scope、token、来源、数据库与私有文件。核对任务及原生活动后停相关写入方。SQLite 用一致性备份接口，或停止写入后保存完整状态；运行中只复制主文件会漏 WAL。

回滚方案说明哪些双端程序与配置需一起恢复、数据格式是否兼容。升级后若已有业务写入，不能直接覆盖成旧数据库；先核对新增事实，再决定回退或向前修复。

## Linux Hub

SSH 管理身份只用于部署。Hub 使用独立无特权账号，profile 不取得 sudo 或维护私钥。已有合适账号则核对复用。

公共程序和依赖由管理员持有，业务账号只读。Hub 配置、token、状态私有，配置满足程序要求的 mode 0600。profile 通过专用组访问 socket：父目录 750、socket 660；核对实际进程补充组，不用 777 修权限。

TCP 用选定的可达私网地址，仅提供 executor WebSocket；工具 HTTP 走 Unix socket 与实际 peer UID。当前 Hub 不带 TLS，没有受控连接方案时先解决传输方式。

Hub 使用 systemd。核对启用状态、真实 UID、监听、进程及启动结果，不能只看 enable 命令返回。

## Windows 执行端

配置、token、SQLite、日志、发布记录和启动入口放入选定服务目录。ACL 限定当前用户、SYSTEM 和 Administrators。工作目录可在别处，授权不表示移动项目。

用当前普通用户自己的 Codex 和 CODEX_HOME。默认登录触发、Limited、Interactive、隐藏、单实例；失败重启按已确认维护方案设置。常驻任务不应到默认时长后被杀。

计划任务调用 PowerShell 绝对路径，使用 `-NoProfile -NonInteractive -WindowStyle Hidden -File`。启动脚本对子进程使用无可见窗口方式，stdout/stderr 写日志，并保持等待进程，不能启动后立即退出。停止时核对该服务 owned executor/Codex 子进程，保留无关会话。

从用户/系统设置解析 USERPROFILE、APPDATA、LOCALAPPDATA、TEMP、PATH 等；Codex 工具进程的环境可能被过滤。只修该服务子进程环境，保留全局环境与原认证。已有代理按需沿用，并让 Hub 私网地址走正确路径。

核对登录触发、用户、权限、隐藏参数与单实例，再确认进程属主、路径、数量、窗口句柄及 Hub 连接。LastTaskResult 是最近请求结果，IgnoreNew 拒绝重复启动也会改变它，需结合 Running 与实际连接判断。没做系统重启/登录测试时，明确只验证配置和本次启动。

## Hermes、MCP 与 worker

逐项核对目标 profile 的实际账号、HOME、HERMES_HOME、unit；优先复用已有隔离账号。共享账号迁移另按明确范围处理。default 和其他 profile 不自动迁移或接入 Hub。

命名 profile 独立运行网关时，核对目标版本的官方 standalone 与多 profile 启动保护；需要时配置 `gateway.standalone=true`、`GATEWAY_MULTIPLEX_PROFILES=false`。实际确认该进程只服务目标 profile，避免重复轮询其他账号。

适配依赖按锁文件安装，业务 UID 能读、不能改。uv 使用 `--link-mode copy`，避免 root 缓存硬链接保留过严权限；仍需以实际 UID 导入确认。

完整适配包目录注册为来源插件，启用时保留原插件。官方 hook 为 pre_gateway_dispatch、post_gateway_admission、pre_tool_call。MCP 名为 dots_duet，七工具为 duet_query、duet_prepare、duet_submit、duet_stop、duet_answer、duet_approve、duet_record。

网关父进程、MCP、worker 使用同一 profile UID、HOME、HERMES_HOME、DOTS_DUET_SOCKET、DOTS_DUET_FILES_DIR。仅给 MCP 子进程写环境变量不够，来源插件在网关父进程中运行。传输副本目录 700、文件 600，各 profile 分开。

合并配置时保留微信/其他平台、已有 MCP、模型、provider 和 Tool Search。使用运行时已有 YAML 解析器，不假定安装了 PyYAML。遇版本不兼容，核对官方接口后报告或准备升级，不修改 Hermes 核心。

原生 API 仅回环监听，每个 profile 独立端口与随机 token。带认证 capabilities 必须报告 runs_idempotency supported=true、durable=true。worker 的 token 文件存同一裸 token，权限 600；URL 使用 API 根地址，指定实际 Hermes Python 与源码。

以实际 UID 做 MCP stdio initialize/list_tools，检查 folders、folder_id、working_directory 字段及官方 discovery/Tool Search 可见性。工具列表成功不证明来源绑定或模型实际调用。

真实来源从用户选定聊天的原生允许来源、会话或接入流程核对；多个候选时请用户确认。昵称、账号和 profile 名不是稳定来源 ID，生产绑定不能用 fixture。

需重启时先核对 active work、run、lease 和待恢复消息。版本支持时用官方 quiet drain 与 suppress_notification；临时改通知偏好前记录原值，启动后恢复原语义，核对 drain/通知标记清除。仅停目标 profile；替换共享适配代码前核对全部使用者都在已授权范围。确认旧进程退出后再替换，避免新旧模块混用。

worker 每个 profile 一份，用对应 UID 和 socket 组常驻。启动或 `--once` 都可能调用模型、发送消息，不能作为只读探针。队列非空时按已确认授权处理；授权不足则保持停止并明确未完成。队列空且部署授权包含后台服务时可启动。

## 验收顺序

1. **进程和权限**：实际账号、服务状态、单实例、日志、自启正确；未授权 profile 不可连接 socket，其他用户不能读取 Hub 私密数据。日志检查不输出凭据或个人来源。
2. **配对**：正式 token 建立连接、scope 一致；connected、initialized、错误分别核对。probe 失败保留原错误，不改 provider 掩盖失败。
3. **目录**：实际 UID 查询的授权根与用户清单完全一致且可用；无任务浏览指定 folder_id 成功，父目录和未授权根拒绝。缺少真实消息上下文时，可明确登记管理员只读 audit，但不能称为微信入站验收。
4. **Hermes**：三 hook、七工具、新字段与官方发现正常，API 持久幂等可用。worker 获准启动后检查运行和错误，不为制造健康结果投递任务。
5. **可选真实流程**：按明确授权在指定目录创建最小任务，核对 cwd、实际文件、继续/停止和所需人工审批。私有副本与原聊天回执分别核对，不以模型文字自报证明完成。

记录已通过、失败、未验证。账号路由超时、未测模型和真人审批等保持各自状态。公开仓库仅保留通用示例、精确代码版本和验证范围。真实路径、账号、地址、UID、来源和服务映射仅保存在仓库外私有记录或已确认忽略的 deployments.local/，其中再交付启停方法、回滚条件与清理结果。
