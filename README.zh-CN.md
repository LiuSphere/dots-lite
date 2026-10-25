# dots-lite

**通过 Hermes，在聊天里运行和跟进自己电脑上的 Codex。**

[English](README.md) · [部署 skill](skills/dots-lite-deploy/SKILL.md) · [Hermes 适配说明](adapters/hermes/README.md) · [Releases](https://github.com/LiuSphere/dots-lite/releases)

代码、Codex 登录和依赖保留在自己的电脑。Hermes 接收聊天要求，Linux Hub 将任务路由到配对的 Windows 执行端，再把状态、审批和文件送回对应 profile。

```text
Hermes 聊天 / 微信
        │ MCP + 可信来源插件
        ▼
Linux Hub ── 私网 WebSocket ◀── Windows 执行端
                                      │ 本地 stdio
                                      ▼
                                Codex app-server
                                      │
                                  任务工作目录
```

## 当前源码

当前源码采用**协议 4**，Hermes 适配包版本为 **0.4.0.dev0**。已发布 v0.3.0 使用协议 3。升级须协调 Hub、执行端和适配包，并显式离线迁移旧状态，不能只替换一个二进制。

- **继续原生会话**：列出配对电脑同一 Codex 环境内可访问的持久会话，包含不同来源和归档记录；完整处理分页，继续时保留原编号、历史和目录。列举不恢复执行，也不启动模型回合。
- **明确接入失败原因**：核对实际 provider、目录、审批和沙箱。首次接入展示实际策略并保持任务暂停，后续明确请求才继续。活动写入冲突或权限无法核实时显示原因并登记通知，不强制接管其他活动实例。
- **按任务选择目录**：新安装默认 directory_mode=task，用户在当前可信消息中提供已有绝对目录或原生会话编号。文件操作限定真实任务目录；allowlist 是可选附加限制，旧安装升级保留原授权根。
- **按关键事件跟进**：普通输出、等待和原生自行重试不重复发起模型诊断。正常推进不受整项任务累计八次或一小时限制，同次未解决故障仍有限诊断；原生资源限制、用户暂停和未知操作保持有效。
- **暂停和恢复服务**：两份 Windows 脚本用于维护。默认先暂停受管理任务及 Goal，确认 owned 进程退出后停止服务；恢复服务不自动解除任务暂停。
- **核验和交付文件**：执行、核验、取得文件和聊天送达分别记录。回合结束或模型自报成功不证明用户目标通过，也不证明文件送达。

例如，task 模式下可以说：

> 在 `C:\work\demo` 修复登录失败的问题。先检查原因，完成后给我测试结果和修改的文件。

选择目录不授予管理员权限，也不绕过原生审批。系统核对 Linux UID、profile、执行端和可信消息来源，不修改 Hermes 核心，不另建原生运行引擎。

## 部署

当前支持 **Linux Hub + Windows 执行端 + Hermes**。每个接入 profile 使用独立无特权系统账号。执行端主动连接 Hub，不需要服务器 SSH 登录 Windows。Hub 没有内置 TLS，须使用受控私网或经过认证的加密隧道。

可以直接交给 Agent：

```text
请按 skills/dots-lite-deploy/SKILL.md 部署或更新 dots-lite。
先检查现有环境，保留凭据、编号、任务历史、暂停及目录限制。
只询问缺少的决定。修改服务前先准备配置和一致备份。
如需新建临时 sudo 访问，先说明权限和清理范围。
最终分别报告实际验证和未验证的行为。
```

服务器管理员身份只用于部署，业务 profile 不取得维护密钥或 sudo。新建临时访问使用独立的[准备与撤销流程](skills/dots-lite-deploy/references/access.md)；已有共享管理员身份不能当作临时清理对象删除。

完整配置和迁移步骤见[部署操作](skills/dots-lite-deploy/references/deployment.md)。MCP 配置项为 dots_lite，七工具采用 dots_lite_* 名称，环境变量采用 DOTS_LITE_*。

协议 3 迁移前须核实旧写入方已停止并取得一致备份，再以实际服务账号运行明确的离线命令。迁移保留任务、原生会话、操作编号、收据和恢复历史，保留 allowlist，任务保持暂停。协议 4 一旦接受新的业务或控制写入，不能用旧快照覆盖最新状态。

两份维护脚本与 bin/executor.exe 同目录，读取 config/executor.json。maintenance 元数据须匹配真实计划任务、程序和完整参数。仓库不包含私人配置、密钥、数据库或二进制。

## 验证与边界

本地检查覆盖 Windows Go 测试和 vet、Linux Hub/执行端/集成测试及 Hermes 适配测试。实际 Codex 检查覆盖只读清单，以及专用线程通过 localhost 固定响应提供方的继续。受控时钟覆盖 72 小时和 12 次正常阶段，不等于实际运行三天或测得模型成本。

真实模型监督、真人审批、微信附件收发、多执行电脑和长期故障恢复需要各自的现场证据。旧协议执行端无法连接协议 4 Hub。读取原生历史不代表可安全接管桌面已有活动会话。原生 run 数不等于底层 LLM 请求数；用量不可观察时保持未知。

## 开发

Go 工具链由 [go.mod](go.mod) 固定。适配包使用 Python 3.14 和 uv 锁文件；运行已构建的 Windows 执行端不需要 Go 或适配包 Python 环境。

```sh
# 在仓库根目录执行；普通测试不调用真实模型。
go test ./... -count=1
go vet ./...
go build -o dist/executor.exe ./cmd/executor
uv sync --project adapters/hermes --locked
uv run --project adapters/hermes --locked python -m unittest discover -s adapters/hermes/tests -v
```

Hub 在 Linux 构建，或用 GOOS=linux GOARCH=amd64 交叉构建。UID/socket 测试在 Linux 执行；Windows 跳过不能代替。显式设置 DOTS_LITE_REAL_CODEX=1 才启用可能调用真实模型并写临时目录的测试，不把它设为全局健康检查开关。

```text
cmd/hub/           Linux Hub 入口
cmd/executor/      Windows 执行端和维护命令
internal/          协议、授权、会话、文件和持久状态
adapters/hermes/   MCP、可信来源插件和后台适配
skills/            公开部署流程与资源
```

根目录 docs/ 是本地维护的需求、设计、进度及验收资料，由 Git 忽略。公开部署说明不依赖这些文件。

## 参与项目与许可证

通过 Issue 报告问题或建议，通过 Pull Request 提交改动。请附版本、系统、复现步骤和脱敏日志，分别描述模型执行、文件取得和消息送达；不要提交密钥、token、聊天标识或私人部署记录。代码改动运行相关测试。

本项目采用 [BSD 3-Clause](LICENSE) 许可证，按其条款允许商业使用、修改和分发。第三方组件遵循各自许可证。