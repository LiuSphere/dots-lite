# 临时部署访问

此流程用于 Windows Agent 向 Linux 部署。SSH 只用于安装和维护，日常执行端连接不需要服务器 sudo 密钥。默认创建本轮专用、短期的部署身份，保留用户原有管理员入口。

## 先说明风险并取得认可

向用户说明实际目标、临时账号、有效期和清理范围，然后取得明确回答：

> 我会在你指定的服务器上创建临时部署账号。它持有的 SSH 公钥对应一个本机无口令私钥，账号可通过免密 sudo 执行任意 root 命令。任何取得该私钥的人都可能控制服务器、读取凭据、修改或删除数据。SSH 转发限制不能约束已经取得 root 的人。私钥只在本机生成，不上传服务器，不加入 ssh-agent。公钥最长 24 小时后失效，但已建立的连接和 sudo 规则不会因此自动消失。部署验收后，我会撤销公钥和 sudo 规则、停用临时账号，再删除本机专用私钥及本轮副本。保留你原有的管理员账号与密钥用于恢复。是否理解并同意为本次部署创建这项临时访问？

未得到认可时，继续不需提权的构建和配置准备；不创建 root 等价访问。用户不接受时可由用户管理员按具体安装方案执行提权步骤，不修改目标服务权限来绕过决定。

无口令 SSH 私钥和免密 sudo 是两项设置；密钥本身不携带 sudo 权限。仅删除本机文件无法撤销可能已经复制的私钥。清理必须同时处理远程授权。

## Agent 完成准备

确认远程已安装 Python 3、OpenSSH、sudo/visudo、shadow 账号工具、util-linux 的 runuser 和 OpenSSL。检查 `/etc/sudoers.d` 会被 sudo 读取，以及 sshd 使用标准的用户 `authorized_keys`、支持 `restrict` 和 `expiry-time`。自定义 AllowUsers/AllowGroups、AuthorizedKeysFile、MFA 或集中身份策略需要按实际策略准备；辅助脚本不改全局 SSH 配置来绕过它们。

先核对用户信任的主机指纹并记录到本机 known_hosts。可通过已有可信连接或用户的服务器控制台取得；`ssh-keyscan` 只取得候选值，不能独立证明身份。准备命令使用 `StrictHostKeyChecking=yes`，不会跳过验证。

在风险已认可后，Agent 在本机运行以下准备脚本，填入实际参数；这些参数及输出目录不得提交 Git。`--admin-identity` 是用户原有的普通登录身份，可省略以使用已配置的 SSH 身份。先确认所选 OpenSSH 工具可在 Agent 环境运行；遇到包管理器 shim 或被过滤的环境导致启动失败时，用 `--ssh` 和 `--keygen` 指定真实可执行文件，保留原始错误。

```powershell
python skills/dots-lite-deploy/scripts/prepare_access.py prepare `
  --host server.example.net --port 22 --admin admin `
  --admin-identity 'C:\private\ordinary-admin-key' `
  --output 'C:\private\dots-lite-access-UNIQUE' `
  --risk-acknowledged
```

脚本只做本机准备：生成 Ed25519 专用密钥、收紧私有目录权限，生成 `access.json` 和已填好目标的 `approve.ps1`。输出目录必须在仓库外且尚不存在。脚本不会连接服务器或自动授予 sudo。检查生成文件与目标后，向用户交付这一条实际命令：

```powershell
& 'C:\private\dots-lite-access-UNIQUE\approve.ps1'
```

用户在自己的终端执行。命令通过普通管理员身份分配 TTY，运行 sudo 提示密码，并将准备好的公钥装入服务器上的临时账号；私钥保留在本机。密码只输入终端，不进入聊天、命令参数、环境变量或文件。普通 SSH 已免交互时通常只提示 sudo 密码；SSH 密码登录、密钥口令或 MFA 可能另有提示，提前说明，不能承诺固定一次。

Agent 应完成参数、载荷和命令的准备，不让用户替换占位符或手工编辑 sudoers。如果普通 SSH 登录本身需要人工认证，优先使用上述内嵌载荷命令，避免先要求用户手动上传文件。生成的 `approve.ps1` 内含可检查的 Base64 Python 源码，其原文为 [remote_access.py](../scripts/remote_access.py)；Base64 不是加密。

远程载荷创建随机命名的 `dl-deploy-<id>` 系统账号和独立 `/etc/sudoers.d` 文件，用 visudo 校验，再用该 UID 检查 `sudo -n true`。账号使用未保留的随机密码哈希，不使用空密码。管理员持有的 home 和公钥文件只含本轮身份；公钥关闭转发、PTY 等功能，并设置 UTC 到期时间。账号名、规则和路径冲突时停止，不覆盖已有身份。安装过程失败会撤回本轮已创建的文件与账号；撤回失败会明确报错。

收到成功结果后，Agent 从私有 `access.json` 读取账号与私钥路径，以 `BatchMode=yes`、`IdentitiesOnly=yes`、禁用连接复用和 agent 的新连接验证身份及 `sudo -n`，再执行原部署流程。不要在首次连接失败后重新执行安装命令；先查已创建的身份和原始错误。

## 验收后的撤销与清理

保存配置位置、程序摘要、配对和恢复记录，确认普通管理员入口仍可用于维护。关闭本轮其他部署 SSH/ControlMaster 会话，停止自己启动的临时提权进程，再运行：

```powershell
python skills/dots-lite-deploy/scripts/prepare_access.py revoke `
  --manifest 'C:\private\dots-lite-access-UNIQUE\access.json'
```

此命令先匹配本轮 marker、账号、完整公钥和 sudo 规则，再撤销远程公钥及规则，锁定并使临时账号到期，用 nologin 禁止登录。它保留停用账号和 root 私有的非秘密 marker 作为审计记录。业务服务使用自己的无特权账号，不依赖该临时身份。

脚本确认新 SSH 连接得到认证拒绝后，才删除本机 `identity` 和 `identity.pub`；`access.json` 写入撤销结果。辅助脚本从未加载 ssh-agent；若 Agent 在本轮另行加载过它，先删除该 agent 身份。核对本轮传输、备份和临时文件没有私钥副本，删除确属本轮的副本，并确认原路径不存在。文件删除不等于 SSD 上的安全擦除，远程撤销才使复制的密钥失去访问权。

撤销公钥不会终止已有会话，也不能消除曾经取得 root 后留下的程序。因此先关闭本轮其他会话与进程，再撤销；怀疑泄露时按服务器入侵处理，不能只删除密钥。保留普通管理员恢复入口，不删除共享密钥、用户其他 authorized_keys 或全局 sudoers。

过期、断网、检查失败或部署中断时，清理仍是未完成事项。明确列出剩余账号、规则、私钥、到期时间和错误，使用普通管理员执行私有清单中的 `revoke_command`（将 `sudo -n` 改为交互 `sudo`），验证撤销后再删除专用私钥。自助撤销会话若在移除规则后中断，后续也从普通管理员入口继续，不能靠失败重试猜测已完成。禁止将网络超时当作认证拒绝。

远程撤销允许本轮规则或公钥已经被删除，管理员可继续完成剩余步骤。辅助脚本在收到撤销成功后先保存 `remote_revoked`，本机检查或删除失败时可重新运行清理命令；不要为恢复清理而重新授予 sudo。

## 辅助脚本验证

在有 sudo、OpenSSH 服务端、shadow、util-linux 和 OpenSSL 的 Linux 测试环境中运行：

```sh
sudo python3 skills/dots-lite-deploy/scripts/tests/linux_access_smoke.py --require-ssh
```

测试自动建立独立 mount、PID 和网络命名空间，以临时副本替换 `/etc`，用 tmpfs 隔离 `/var/lib`、`/var/log` 和 `/run`。它实测账号与 sudo 授权、SSH 登录、撤销、认证拒绝、本机密钥删除、半完成撤销恢复及失败回滚，结束后移除临时副本。不支持这些隔离能力时直接失败；不在宿主系统直接创建测试账号。

## 依据

- [OpenSSH ssh-keygen 手册](https://man.openbsd.org/ssh-keygen.1)：私钥、公钥和口令。
- [OpenSSH sshd 手册](https://man.openbsd.org/sshd.8)：authorized_keys、restrict、expiry-time。
- [sudoers 手册](https://www.sudo.ws/docs/man/sudoers.man/) 与 [visudo 手册](https://www.sudo.ws/docs/man/visudo.man/)：NOPASSWD、配置校验。
