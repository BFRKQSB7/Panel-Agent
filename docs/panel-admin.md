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

`run` 成功后每 30～35 秒同步一次，失败时指数退避并加入抖动，恢复连接后自动继续。`sync` 可手动执行单次同步。重新配对时，先在机器管理页面为目标机器签发专用凭据，再在同一 Agent 数据目录执行 `re-pair`；它保留机器 ID 并轮换认证密钥。吊销会立即拒绝该机器的后续 API 请求，但不会远程停止机器上的进程。当前没有 Core 应用实现，Agent 只报告快照已接收，不会报告 applied 成功。

## 验证

```powershell
go test ./internal/security ./internal/storage ./internal/control
pwsh -File .\bench\run-d02-smoke.ps1
```
