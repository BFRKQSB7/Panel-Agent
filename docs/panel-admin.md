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

## 验证

```powershell
go test ./internal/security ./internal/storage ./internal/control
pwsh -File .\bench\run-d02-smoke.ps1
```
