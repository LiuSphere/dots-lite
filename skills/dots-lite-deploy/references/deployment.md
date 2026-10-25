# 部署操作与验收

以选定源码为准：读取 `cmd/hub`、`cmd/executor`、`internal/protocol`、`adapters/hermes` 和锁文件；仓库有部署文档时，查阅 `docs/运行指南.md`、`docs/使用指南.md`、`docs/部署记录模板.md`。历史路径与版本不是通用安装参数。以下保留不易从单个配置看出的约束。

## 绑定与工作目录

先按 [SKILL.md 第 0 节](../SKILL.md#0-目录授权与默认权限)核对版本及目录模式，再收集对应参数。协议 4 的配置与迁移见本文末节。

- 协议 4 新装 task 模式不要求 folders/folder_ids。新任务使用当前可信消息明确选择的已有绝对目录，已有会话使用原 cwd；部署目录不自动获得任务授权。
- allowlist 模式在 Hub 设置 `bindings[].folder_ids`，执行端设置 `folders=[{id,root,...}]`，ID 对应一致。root 是本机授权根的绝对路径；旧安装保留原根。
- 当前一个 Linux UID 只有一条 binding，绑定一台 executor，可授权多个根。增加第二台电脑前核对实现是否支持，不能复制同 UID 的 binding 绕过限制。
- scope 的 user_id/profile_id/executor_id、token、可信来源共同完成配对。执行端主动连接 Hub，不依赖服务器 SSH 登录电脑。重启沿用 token、scope 和 state_path，自动复连。
- 协议 4 的 task.create 使用绝对 working_directory；allowlist 模式仍核对其位于指定 folder_id 的真实根内。旧协议 3 只接受根内相对目录，使用 `/` 分隔，省略为 `.`。两种协议都拒绝缺失目录、越界和对象替换；创建后固定任务目录，附件和文件操作相对它进行。
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

Windows Terminal 为默认终端时，仅对 PowerShell 使用 `-WindowStyle Hidden` 仍可能留下可见窗口。将[WindowsServiceLauncher.cs](../assets/WindowsServiceLauncher.cs)编译到服务 bin 目录，计划任务调用这个 GUI 子系统程序，参数为 PowerShell 与启动脚本的两个绝对路径，各自加引号。在已确定源码和安装目录后执行：

```powershell
$compiler = Join-Path $env:WINDIR 'Microsoft.NET/Framework64/v4.0.30319/csc.exe'
$launcherSource = Join-Path $sourceRoot 'skills/dots-lite-deploy/assets/WindowsServiceLauncher.cs'
$launcherOutput = Join-Path $installRoot 'bin/WindowsServiceLauncher.exe'
& $compiler /nologo /target:winexe "/out:$launcherOutput" $launcherSource
if ($LASTEXITCODE -ne 0) { throw 'Launcher build failed' }
```

`$sourceRoot` 和 `$installRoot` 必须来自本次已核对参数；编译器缺失时报告依赖，不退回已经失败的直接 PowerShell 入口。已有安装核对原 launcher 后可复用。

启动入口使用 `UseShellExecute=false`、`CreateNoWindow=true`，等待 PowerShell 并传回原退出码；自身启动失败写入 `..\logs\launcher-host.log` 并返回 1。脚本再以无窗口方式启动执行端、保存 stdout/stderr，并等待执行端。常驻脚本等待是正常生命周期，不能让它提前退出或丢弃失败状态来关闭窗口。停止时核对 owned 进程，保留无关会话；不修改用户全局终端选择。

从用户/系统设置解析 USERPROFILE、APPDATA、LOCALAPPDATA、TEMP、PATH 等；Codex 工具进程的环境可能被过滤。只修该服务子进程环境，保留全局环境与原认证。已有代理按需沿用，并让 Hub 私网地址走正确路径。

核对登录触发、用户、权限、GUI 入口与单实例，再确认进程属主、路径、数量、窗口句柄及 Hub 连接。不能只检查 pwsh 的 MainWindowHandle：还要枚举可见 Windows Terminal/控制台窗口，并核对所属进程，避免遗漏独立托管窗口。以临时任务检查无可见窗口和非零退出码传播，测试后清理；不要关闭无关终端。LastTaskResult 是最近请求结果，IgnoreNew 拒绝重复启动也会改变它，需结合 Running 与实际连接判断。没做系统重启/登录测试时，明确只验证配置和本次启动。

## Hermes、MCP 与 worker

逐项核对目标 profile 的实际账号、HOME、HERMES_HOME、unit；优先复用已有隔离账号。共享账号迁移另按明确范围处理。default 和其他 profile 不自动迁移或接入 Hub。

命名 profile 独立运行网关时，核对目标版本的官方 standalone 与多 profile 启动保护；需要时配置 `gateway.standalone=true`、`GATEWAY_MULTIPLEX_PROFILES=false`。实际确认该进程只服务目标 profile，避免重复轮询其他账号。

适配依赖按锁文件安装，业务 UID 能读、不能改。uv 使用 `--link-mode copy`，避免 root 缓存硬链接保留过严权限；仍需以实际 UID 导入确认。

完整适配包目录注册为来源插件，启用时保留原插件。官方 hook 为 pre_gateway_dispatch、post_gateway_admission、pre_tool_call。MCP 名为 dots_lite，七工具为 dots_lite_query、dots_lite_prepare、dots_lite_submit、dots_lite_stop、dots_lite_answer、dots_lite_approve、dots_lite_record。

v0.3.0 使用协议 3；当前 0.4.0.dev0 源码使用协议 4，部署包必须匹配。统一 MCP 项 dots_lite、插件 dots-lite、mcp__dots_lite__dots_lite_* 工具、DOTS_LITE_* 环境变量、dots-lite 目录与服务名。旧配置不进入运行时回退；已有部署先在停止写入后备份并离线转换状态，再同步切换双端。核对官方发现与 Tool Search 仅呈现当前 MCP 项及七个工具。

网关父进程、MCP、worker 使用同一 profile UID、HOME、HERMES_HOME、DOTS_LITE_SOCKET、DOTS_LITE_FILES_DIR。仅给 MCP 子进程写环境变量不够，来源插件在网关父进程中运行。传输副本目录 700、文件 600，各 profile 分开。

合并配置时保留微信/其他平台、已有 MCP、模型、provider 和 Tool Search。使用运行时已有 YAML 解析器，不假定安装了 PyYAML。遇版本不兼容，核对官方接口后报告或准备升级，不修改 Hermes 核心。

原生 API 仅回环监听，每个 profile 独立端口与随机 token。带认证 capabilities 必须报告 runs_idempotency supported=true、durable=true，且 retention_seconds 为正。worker 的 token 文件存同一裸 token，权限 600；URL 使用 API 根地址，指定实际 Hermes Python 与源码。

以实际 UID 及服务声明的补充组做 MCP stdio initialize/list_tools，核对当前版本的目录字段、threads/thread 查询、task.attach 和官方 discovery/Tool Search 可见性。来源插件的私有选择不能变成模型可伪造的参数。工具列表成功不证明来源绑定或模型实际调用。

真实来源从用户选定聊天的原生允许来源、会话或接入流程核对；多个候选时请用户确认。昵称、账号和 profile 名不是稳定来源 ID，生产绑定不能用 fixture。

需重启时先核对 active work、run、lease 和待恢复消息。版本支持时用官方 quiet drain 与 suppress_notification；临时改通知偏好前记录原值，启动后恢复原语义，核对 drain/通知标记清除。仅停目标 profile；替换共享适配代码前核对全部使用者都在已授权范围。确认旧进程退出后再替换，避免新旧模块混用。

worker 每个 profile 一份，用对应 UID 和 socket 组常驻。启动或 `--once` 都可能调用模型、发送消息，不能作为只读探针。队列非空时按已确认授权处理；授权不足则保持停止并明确未完成。队列空且部署授权包含后台服务时可启动。

## 验收顺序

1. **进程和权限**：实际账号、服务状态、单实例、日志、自启正确；未授权 profile 不可连接 socket，其他用户不能读取 Hub 私密数据。日志检查不输出凭据或个人来源。
2. **配对**：正式 token 建立连接、scope 一致；connected、initialized、错误分别核对。probe 失败保留原错误，不改 provider 掩盖失败。
3. **目录**：task 模式核对当前可信消息选择的任务目录、实际对象与文件范围；allowlist 模式另核对根清单，无任务浏览指定 folder_id，父目录和未授权根拒绝。缺少真实消息上下文时，可明确登记管理员只读 audit，但不能称为微信入站验收。
4. **Hermes**：三 hook、七工具、新字段与官方发现正常，API 持久幂等可用。worker 获准启动后检查运行和错误，不为制造健康结果投递任务。
5. **可选真实流程**：按明确授权在指定目录创建最小任务，核对 cwd、实际文件、继续/停止和所需人工审批。私有副本与原聊天回执分别核对，不以模型文字自报证明完成。

记录已通过、失败、未验证。账号路由超时、未测模型和真人审批等保持各自状态。公开仓库仅保留通用示例、精确代码版本和验证范围。真实路径、账号、地址、UID、来源和服务映射仅保存在仓库外私有记录或已确认忽略的 deployments.local/，其中再交付启停方法、回滚条件与清理结果。


## 协议 4 配置与维护

新装在 Hub binding 和执行端配置中都明确设置 `"directory_mode":"task"`，可省略 folders/folder_ids。用户在当前消息提供绝对任务目录；文件操作仍固定到实际打开的目录对象。选择严格白名单时两端设 `allowlist` 并保留 folders/folder_ids；不能让两端模式不同。已有安装缺省且有根清单时仍为 allowlist。

Hub 的 followup_budget 现在限制同次故障的已受理诊断，默认 3；窗口 30 分钟，未知提交最多 24 次补查/传输轮询。正常阶段无任务级次数或一小时上限。原生幂等过期的未知提交不重发。模型调用数无法观察时保持未知，不能用 run 数替代。已有 followup_budget=8 不会自动改成 3，切换时明确该配置的新语义。

执行端启动时使用安装版 Codex 的 generate-json-schema 取得全来源枚举；失败时原生清单返回 source_schema_unverified，不退回部分来源。首次接入保持暂停并展示实际策略，继续须来自后续明确用户消息。对活动写入、原生直接输入受限、提供方和权限差异，保留错误并通知原聊天。

Windows 配置增加 maintenance 对象：

```json
{
  "maintenance": {
    "task_name": "\\DotsLite-Executor-EXAMPLE",
    "task_executable": "C:\\apps\\dots-lite\\bin\\WindowsServiceLauncher.exe",
    "task_arguments": "REPLACE_WITH_EXACT_REGISTERED_ACTION_ARGUMENTS"
  }
}
```

从 Get-ScheduledTask 读取真实任务和唯一 action，核对其 SID、路径、完整 Arguments、配置和 scope。参数不能照抄示例。复制 assets/暂停服务.cmd 与 assets/恢复服务.cmd 到 bin；它们使用 ../config/executor.json，不在部署 skill 之外另造后台控制程序。也可在该目录执行：

```powershell
.\executor.exe maintenance status -config ..\config\executor.json
.\executor.exe maintenance pause -config ..\config\executor.json
.\executor.exe maintenance resume -config ..\config\executor.json
```

暂停禁用原计划任务，写入持久维护屏障，先暂停任务与 Goal、结算旧写，再关闭 native supervisor、owned app-server/子进程和主进程。控制端持有进程句柄核对退出，确认清理收据及离线排他后才登记 stopped。停止失败保持屏障并报告，不以强杀、重新绑定或删除状态替代。恢复前原实例须退出；恢复后每项任务仍暂停。

协议 3 升级须先获准停止旧 Hub、worker、执行端及受管理原生工作，禁用旧启动入口并核对退出。新二进制拒绝直接打开旧业务状态。以原账号执行离线命令，保留原配置、scope、token、目录、ID、摘要、收据和未知操作：

```sh
hub -config /ABS/hub.json -state /ABS/hub.sqlite -migrate-v3 -offline-confirmed
```

```powershell
.\executor.exe maintenance migrate -config ..\config\executor.json -offline-confirmed
```

两端先创建独立一致备份再事务转换；旧配置必须保留 allowlist，执行端停在 stopped。缺失历史权限的任务保持暂停/待核实，重新接入并展示实际权限后才继续。离线确认是管理员对旧版进程已经停止的确认，新锁不能替代这项核对。旧版不识别新状态，不能在 v4 库上试跑旧二进制。

直接恢复旧快照仅限未受理新业务和控制变化的切换窗口；一旦 v4 接受新写、暂停或授权变更，就保留最新库、维护停写并向前修复，不能用旧备份抹掉新收据与暂停。生产迁移、计划任务实际控制、真实模型/微信及三天运行仍按现场授权和证据验收；本地 fixture 通过不代表这些部署结果。
