# Panel-Agent

[**English**](README.md) | **简体中文**

## 发布状态

当前版本 **0.1.0** 为预发布：两小时固定长稳与最终发行接纳尚未完成，D13-HY2/D17 保持 PARTIAL。最新长测记录3750秒、16962次请求后由用户停止，尾部HTTP EOF与停机的先后顺序尚未确定，原始证据保留在 `bench/results/`。

## 功能

Go 编写的管理面板与主动拉取配置的 Agent，提供节点、用户权限、三维配额、持久流量账本和隐藏的一跳 Exit。VLESS+Reality/Trojan 使用每机一个 Xray，hy2 使用独立官方 Hysteria 2；按需选择 hy2-only 或双内核。任务执行与验收状态只在 [任务索引](tasks/README.md) 维护。

## 运行范围

安装器面向 Debian 12+/Ubuntu 22.04+、systemd、amd64/arm64。Agent 的低配目标为整机 128MiB/1GB，Panel 与本机节点的目标为 1C/512MiB；实际承载能力取决于节点、用户和连接数量，必须通过对应整机验收。Alpine 功能测试、交叉编译和单独 cgroup 压测分别保留证据，不能代替正式平台矩阵。

内置 Core 最低版本为 Xray `26.10.21`。128MiB 升级测试使用 `GOMEMLIMIT=32MiB GOGC=50`，并记录主机现有服务、swap 和 OOM 计数；这些环境变量需要由运维显式配置，默认 systemd unit 不会自动设置整机资源限制。扩展仅支持声明式受限协议包，具体能力通过管理界面与 API 检查，不支持任意脚本运行时。

hy2 当前固定官方 Hysteria `2.13.0`，提供 Direct、URI/Mihomo、独立实例与统一配额账本；见 [安装和 NAT UDP 指引](docs/hysteria2.md)。hy2 及双内核资源指标另行验收，当前不宣称通过 128MiB 门槛。

## 构建与本地候选包

Go 模块要求 Go 1.26+；本地打包入口固定使用 Go 1.27.1。准备官方 Xray 26.10.21 的 amd64/arm64 ZIP 后，在 Windows PowerShell 7 与 WSL Debian 环境执行：

```powershell
./scripts/package-linux.ps1 -OutputDirectory ./artifacts/local-candidate `
  -XrayAmd64Package /path/to/Xray-linux-64.zip `
  -XrayArm64Package /path/to/Xray-linux-arm64-v8a.zip
```

输出目录必须不存在。脚本核对固定官方包 SHA-256，生成 Linux 程序、Xray、安装入口、使用文档、18 个实际链接模块的许可证、Go/Xray 许可、来源清单和两层 SHA256SUMS，再归档为 `.tar.gz`。清单中的 `unaccepted_local_candidate` 表示本地产物尚未通过完整发布验收；生成包本身不表示可正式发布。项目代码尚未声明许可证，第三方许可独立保留。

在对应 Linux 主机解压正确架构的包并核对 SHA256SUMS 后，可以运行：

```sh
sha256sum -c SHA256SUMS
sudo sh ./install.sh . --role panel-agent
```

安装器不会自动修改 DNS、防火墙、NAT、nginx 或系统网络参数。管理员初始化、HTTPS、Agent 配对、节点配置与服务启动按 [安装说明](docs/installation.md) 和 [管理后台说明](docs/panel-admin.md) 执行。

## 更新、恢复与验证

本机更新和回滚见 [更新说明](docs/panel-upgrades.md)，完整备份/恢复见 [备份说明](docs/panel-backup.md)，持久计量与补报见 [账本说明](docs/agent-ledger.md)。普通卸载保留数据；只有显式 `--purge` 才删除对应角色拥有的数据。

基础检查为 `go test ./...` 和 `go vet ./...`。真实 Core、双机链路、历史版本升级、整机资源与候选包生命周期测试位于 `bench/`，需要明确指定测试文件或已授权机器。发布前还须完成任务索引要求的三机组合、正式低配环境、跨周期清理及固定长稳测试。

## 文件结构与维护

`cmd/` 为程序入口，`internal/` 为实现与测试，`scripts/` 为打包入口，`docs/` 为运维说明，`bench/` 为真实集成测试及证据。发布维护见 [维护文档](维护文档.md)。

## 0.1.0 变更

首次预发布，包含双内核、有限流量保留与持久身份凭据、不清零的核心健康检查、更新兼容检查和外部租约测试守护。

## 许可证与致谢

项目尚未声明软件许可证，公开可见不代表授予项目软件许可。第三方许可证随包独立提供；感谢 Xray-core、Hysteria、Go 及 `go.mod` 所列依赖。

## 使用范围

仅用于获授权管理的机器与网络，当前验证范围及未完成项以任务索引为准。
