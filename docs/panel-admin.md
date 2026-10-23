# Panel 管理入口

Panel 使用 Go 单体、SQLite 和内嵌页面。默认数据目录为 `./data`，本机开发监听 `127.0.0.1:8080`；未初始化管理员时，`serve` 会报错退出，不会打开管理端口，也没有可远程抢注的初始化 API。

## 本机开发

在仓库根目录先运行初始化命令。终端会隐藏输入并要求确认管理员口令，口令至少 12 字节、最多 72 字节；口令不会放进命令参数或环境变量。

```powershell
go run ./cmd/control admin init --data-dir .\data
go run ./cmd/control serve --data-dir .\data --listen 127.0.0.1:8080
```

浏览器打开 `http://127.0.0.1:8080`。登录后可以修改面板名称、注销并重新登录。管理员口令使用 bcrypt 保存，会话在进程重启后失效，登录失败达到五次后会限流 15 分钟。自动化初始化可用 `--password-stdin`，标准输入必须恰好包含两行口令；不要把口令写入脚本参数、环境变量或日志。

## HTTPS 运行

直接对外监听时必须配置证书和私钥，服务最低使用 TLS 1.2。证书路径通过环境变量传入：

```powershell
$env:PANEL_TLS_CERT = 'C:\panel\tls\fullchain.pem'
$env:PANEL_TLS_KEY = 'C:\panel\tls\private.key'
go run ./cmd/control serve --data-dir C:\panel\data --listen 0.0.0.0:8443
```

也可以让已有反向代理终止 TLS。此时 Panel 只绑定 loopback，代理必须覆盖 `X-Forwarded-Proto` 为 `https` 并保留浏览器请求的 Host；服务仅在连接来自本机 loopback 代理时才信任该协议头：

```powershell
$env:PANEL_TRUST_PROXY = 'true'
go run ./cmd/control serve --data-dir C:\panel\data --listen 127.0.0.1:8080
```

服务会使用 HttpOnly、SameSite Strict Cookie、CSRF token、同源 Origin 检查、限流和安全响应头。数据目录包含 `panel.db` 与 `master.key`，应一起纳入受限权限的备份；如果数据库仍在而密钥缺失，服务会拒绝启动且不会生成替代密钥。当前实现默认单个 Panel 写进程，并使用进程锁阻止并发启动。

## Agent 配对与控制通道

管理员登录后打开“机器管理”，签发一次性 Join Token，并在 30 分钟内将它安全地传到目标机器。Agent 只主动连接 Panel 的 HTTPS 地址；若使用自签证书或私有 CA，把签发证书保存到 Agent 主机并用 `--ca-cert` 指定，公有可信证书不需要该参数。不要把 Join Token 放进命令行参数、环境变量或日志，下面的示例通过标准输入传递：

```powershell
$panelUrl = 'https://panel.example.com'
$joinToken = Read-Host '粘贴一次性 Join Token'
$joinToken | .\control-agent.exe pair --panel-url $panelUrl --data-dir C:\panel-agent\data --name '边缘机器 1' --join-token-stdin
```

配对会在本地持久保存机器身份，然后立即拉取并校验完整 desired-state 快照。若注册已提交但连接中断，使用相同数据目录再次执行 `pair` 会凭本地候选密钥安全恢复结果；不要删除或复制 `agent.identity.json`，它包含机器凭据，必须限制为 Agent 服务账户可读。确认配对成功后，可以前台方式持续同步，Ctrl+C 安全退出：

```powershell
.\control-agent.exe run --data-dir C:\panel-agent\data
```

`run` 成功后每 30～35 秒同步一次，失败时指数退避并加入抖动，恢复连接后自动继续。`sync` 可手动执行单次同步。重新配对时，先在机器管理页面为目标机器签发专用凭据，再在同一 Agent 数据目录执行 `re-pair`；它保留机器 ID 并轮换认证密钥。吊销机器凭据会立即拒绝该机器的后续 API 请求，但不会远程停止机器上的进程。未传 `--xray-path` 时只同步控制通道，不报告 Core 已应用。

## Reality 节点与 Linux Core

进入机器管理中的“Reality 节点”，填写公网主机、公网端口、监听 IP、监听端口、Reality SNI 和目标。公网值仅是管理员录入，不代表已验证 NAT 映射；监听端口 10085 保留给回环 Stats API。节点自动生成独立 X25519 密钥和 Short ID，私钥与机器快照在 Panel 数据库中加密；管理员页面只展示公钥。普通节点默认没有开发凭证，正式用户授权由用户管理功能提供；隔离试验可显式勾选开发凭证，同一开发 UUID 在不同 Node 使用不同统计 email。

Linux Agent 使用已验证的 Xray **26.10.21** 可执行文件。以下命令中的路径由管理员提供；此阶段不会下载或安装 Core：

```sh
./control-agent run --data-dir /var/lib/panel-agent --xray-path /opt/panel-agent/xray
# 自签证书时追加 --ca-cert /path/to/panel-ca.pem
```

所有 Node 合并为一个 Core，由同一个数据锁和串行应用边界管理。Agent 先持久保存候选，执行配置检查及端口预检查，再采样旧 Core、停止旧进程、启动候选并检查全部监听和 Stats 健康；通过后才报告 applied。Core generation 与 Core instance ID 独立持久化，不是账本 epoch。仅名称、公网地址等元数据变化不会重启；authority 改变也会检查候选并保留回滚配置。配置失败保留 Current，启动或健康失败恢复 Current/LKG；Agent 中途退出后，重启会先按本地 journal 恢复，Panel 失联仍使用已确认配置。退出 Agent 不停止 Core。

**节点或凭证撤销通过完整 Core 重启执行，同机其他节点的现有连接也会中断并需重连。**已接收 revision 与已应用 revision 分开显示；应用失败不代表旧 Core 已停止服务，具体原因见节点页的应用错误。Agent 正常每 30～35 秒取配置，撤销生效需等待同步和应用完成。

`agent-data/core/` 保存权限 0600 的配置及 journal，目录必须为 0700；不得手工编辑、跨机器复制或删除。只保留 Current、上一成功 LKG 和正在应用的候选配置。重启前保留旧统计映射与不清零的累计读数，候选尚未确认时的可观测流量也保留；`boundaries/` 是等待后续账本消费的累计观察，不能直接相加。固定 Stats API 不能原子完成采样和停止，边界明确记录不可观测尾部；丢失 Core 时记录 gap，不伪造零用量。目前达到 126 份待消费边界时拒绝新应用，额外两份保留给候选故障恢复，不能删除观察来绕过上限。持久账本任务将接入消费。

Core 不写 access/error 日志，Agent 每次连续失联只输出一次有长度限制的错误，恢复时输出一次；常驻运行时应由服务管理器限制 stderr 保存量。Windows 可运行 Panel 和控制通道，但本阶段受管 Core 的进程恢复实现仅支持 Linux。

### 隔离回环测试

固定 Xray 对服务端代理默认阻止私网和回环目标，参见 [Freedom 的默认策略](https://xtls.github.io/en/config/outbounds/freedom.html)。仅在隔离数据目录和全部凭证明确标识为开发凭证的 Node 上，可追加 `--development-loopback-test`；它只允许这些 Node 访问 TCP `127.0.0.1:18080`、UDP `127.0.0.1:18081`，普通 Node 不加入例外路由。该开关默认关闭，模式写入 journal，后续恢复必须使用相同模式；不要对生产目录启用。

## 验证

```powershell
go test ./internal/security ./internal/storage ./internal/control
pwsh -File .\bench\run-d02-smoke.ps1
```
