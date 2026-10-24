# dots-lite

**通过 Hermes，在聊天里运行和跟进自己电脑上的 Codex。**

[English](README.md) · [使用指南](docs/使用指南.md) · [部署指南](docs/运行指南.md) · [Releases](https://github.com/LiuSphere/dots-lite/releases)

你的代码和 Codex 在自己的电脑上运行。Hermes 接收聊天请求，Linux Hub 将任务送到已配对的电脑，并把状态、审批和文件送回对应的 profile。你可以继续同一会话、补充要求、停止任务，也可以让 Hermes 检查实际文件后再交付结果。

```text
聊天 / 微信
    │
    ▼
Hermes profile ── MCP + 可信来源插件 ── Linux Hub
                                         ▲
                               私网 WebSocket，执行端主动连接
                                         │
                                  Windows 执行端
                                         │ stdio
                                  Codex app-server
                                         │
                                    授权文件夹
```

## 可以做什么

- **在自己的环境执行**：复用电脑上的 Codex 登录、依赖和工作目录。
- **按文件夹授权**：授权一个或多个根目录，任务可选择其中已有的子目录，无需逐个登记项目。
- **持续跟进任务**：保存任务编号、会话、操作和事件；聊天切换或服务重连后仍可查询。
- **处理问题和审批**：将请求关联到具体任务与回合，再把回答送回 Codex。
- **传递和核验文件**：接收输入附件，读取或下载任务文件，获取产物并记录核验和交付结果。
- **隔离不同用户**：按 Linux UID、profile、执行端和可信聊天来源校验访问；业务服务使用无特权账号。

例如，授权 `C:\work` 后，可以在聊天中说：

> 在 `demo` 文件夹里修复登录失败的问题。先检查原因，完成后给我测试结果和修改的文件。

之后可查询任务、补充要求或停止当前执行。工作目录须已存在且位于授权根内；模型得到的文字指令不能扩大文件夹权限。

## 开始使用

当前部署组合为 **Linux Hub + Windows 执行端 + Hermes**。Hub 通过 Unix socket 校验真实调用 UID，执行端通过私网主动连接 Hub。Hub 当前不提供内置 TLS，请使用受控私网或经过认证的加密隧道。

准备以下环境：

| 位置 | 要求 |
| --- | --- |
| 执行电脑 | Windows、已登录且可用的 Codex app-server、任务所需依赖 |
| 服务器 | Linux、systemd、可用的 Hermes profile；每个接入 profile 使用独立系统账号 |
| 网络 | 执行电脑可达 Hub 的受控网络；不需要服务器 SSH 登录执行电脑 |
| 部署工具 | 可读写本机与服务器的 Agent；Windows PowerShell 7、Python 3.10+ 和 OpenSSH 用于临时访问准备 |

1. 从 [Releases](https://github.com/LiuSphere/dots-lite/releases) 选择版本。Hub 与执行端必须使用相同协议；当前 v0.3.0 使用协议 3。
2. 让 Agent 阅读仓库根目录的 [dots-lite-deploy skill](skills/dots-lite-deploy/SKILL.md)。告知服务器、目标 profile、两端安装目录和允许访问的文件夹。
3. Agent 准备配置、备份和服务，核对当前版本及权限，再完成配对与验收。Windows 默认在用户登录后隐藏启动；重启服务沿用凭据并自动重连。
4. 验收后，Agent 撤销临时部署访问并删除本机专用私钥。日常任务不需要保留服务器管理员密钥。

可以直接把下面的话交给 Agent：

```text
请按 skills/dots-lite-deploy/SKILL.md 部署 dots-lite。
先检查现有环境，再询问缺少的服务器、profile、安装目录和授权文件夹信息。
如需临时免密 sudo，先解释风险并取得我的确认，再准备所有材料。
只把必须由我输入密码的那一条命令交给我运行。
验收后撤销临时访问，删除本机专用部署私钥，并报告已验证和未验证的项目。
```

**临时部署密钥具有服务器 root 级别的能力。** 取得该私钥的人可在部署期间控制服务器。部署 skill 包含[风险说明、单命令准备和撤销流程](skills/dots-lite-deploy/references/access.md)；私钥在本机生成，只把公钥装到服务器。已有普通 SSH 登录可免交互时，用户通常只需输入 sudo 密码；否则 SSH 登录可能另外要求密码或多因素验证。

## 状态与边界

项目处于早期阶段。Hub、执行端、MCP、来源绑定、后台跟进和文件传输均已实现。已有真实 Codex 执行与跨机器取文件的验收记录；当前命名更新也已通过部署复核。完整范围见[验收记录](docs/验收记录.md)。

以下能力仍需现场验证：两名真实用户在不同电脑上交错使用、真人微信审批、实际微信附件收发、长期故障恢复和系统重启后的启动。读取或恢复 Codex 会话历史，不代表可以接管桌面端已有的活跃聊天。

MCP 配置项为 `dots_lite`，七个工具统一使用 `dots_lite_*` 名称。配置使用 `folders`、`folder_ids` 和 `folder_id`；当前版本不接受旧名称。升级已有部署时，应先停写、备份并离线转换状态，详见[运行指南](docs/运行指南.md)。

## 开发

Go 版本见 [go.mod](go.mod)；当前为 Go 1.27.3。Hermes 适配包使用 Python 3.14 与 uv 锁文件。运行发布的 Windows 执行端不需要 Go 或适配包 Python 环境。

```sh
# 在仓库根目录执行；普通测试不调用真实模型。
go mod download
go test ./...
go vet ./...
uv sync --project adapters/hermes --locked
uv run --project adapters/hermes --locked python -m unittest discover -s adapters/hermes/tests -v
```

Linux 上运行完整 UID/socket 测试；Windows 会跳过这些平台专用用例。真实 Codex 集成测试需显式设置 `DOTS_LITE_REAL_CODEX=1`，会调用模型并写临时工作目录。构建与服务配置见[部署指南](docs/运行指南.md)。构建产物通过 Releases 分发，仓库不保存二进制。

```text
cmd/hub/           Linux Hub 入口
cmd/executor/      执行端入口
internal/          协议、授权、会话、文件和 SQLite 状态
adapters/hermes/   MCP、可信来源插件和后台 worker
skills/            可复用部署流程与辅助脚本
docs/              使用、运维、设计与验收说明
```

## 参与项目

欢迎通过 Issue 提交问题或建议，通过 Pull Request 提交改动。报告问题时请附版本、系统、复现步骤和脱敏日志，并区分模型执行、文件获取与消息送达的结果。请勿上传私钥、token、真实聊天标识或个人部署记录。

代码改动请运行相关测试；Linux 权限和 socket 行为应在 Linux 验证。设计背景可查阅[需求](docs/需求.md)和[实现方案](docs/详细实现方案.md)。

## 许可证

本项目采用 [BSD 3-Clause](LICENSE) 许可证，允许商业使用、修改和分发。分发时须保留版权、许可条件和免责声明；未经许可，不得使用作者或贡献者的名义为衍生产品背书。第三方组件遵循各自的许可证。
