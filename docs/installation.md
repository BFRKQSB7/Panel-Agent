# Linux 安装、所有权与卸载

Panel-Agent 的安装器只接受已经由发布流程校验过的本地二进制和 Xray 文件，不接受任意 URL、Shell 或远程命令。支持 Debian 12+/Ubuntu 22.04+ 的 amd64/arm64 systemd 主机；安装器不会自动改 DNS、防火墙、sysctl、BBR 或 nginx。

## 低配主机准备

128MiB 整机须先确保操作系统本身能够启动。Debian 13 arm64 的默认大 initramfs 在测试 VM 上耗尽过引导内存；验收使用按实际虚拟硬件生成的精简 initramfs，保留完整 Debian 用户空间。系统包维护应与持续代理负载分开安排，避免 APT 后台维护争用低配机器内存；这不改变程序的功能或配额配置。

systemd 重载要求 `/run/systemd` 至少保留 16MiB 空闲。128MiB 主机的默认 `/run` 上限可能不足，安装或更新前检查 `df -h /run`；空间不足时由管理员执行 `sudo mount -o remount,size=24m /run`。这是 tmpfs 的容量上限，不会预先占用 24MiB RAM；重启后的持久设置由主机管理员管理。

## 构建和安装

在发布目录生成 Linux 二进制，并把已经固定版本、已核对 SHA-256 的 Xray 路径放入 `PANEL_AGENT_XRAY`：

```sh
PANEL_AGENT_XRAY=/path/to/verified/xray ./scripts/build-linux.sh dist amd64
sudo ./scripts/install.sh ./dist --role panel-agent
```

Windows 上可用 `scripts/package-linux.ps1` 生成包含官方 Xray、安装入口、许可证、来源清单和 SHA256SUMS 的双架构本地候选包，命令见根 README。该脚本需要 PowerShell 7、Go 1.27.1 与 WSL Debian；输出目录必须不存在。`.tar.gz` 内程序与 shell 入口使用 Linux 可执行权限，解压后执行 `sudo sh ./install.sh . --role panel-agent`。`manifest.json` 的候选状态不等于发布验收通过，正式状态以任务索引为准。

`scripts/install.sh` 先在发布目录执行 `sha256sum -c SHA256SUMS`，校验不通过就不会调用安装器。`--role control` 只安装 Panel，`--role agent` 只安装 Agent，`--role panel-agent` 安装两者；安装创建 `/opt/panel-agent`、`/var/lib/panel-agent/{control,agent}`、`/var/log/panel-agent` 和项目专用 systemd unit。项目所有的 Xray 会安装到 `/var/lib/panel-agent/agent/runtime/xray`，`/opt/panel-agent/bin` 只保存受管程序入口；systemd unit 和 Agent CLI 都应使用数据目录中的 runtime Core。默认只执行 `systemctl enable`，因为 Agent 必须先完成配对；管理员初始化 Panel 后可分别启动：

```sh
sudo -u panel-agent /opt/panel-agent/bin/control admin init --data-dir /var/lib/panel-agent/control --password-stdin
sudo systemctl start panel-agent-control
sudo -u panel-agent /opt/panel-agent/bin/control-agent pair --panel-url https://panel.example --data-dir /var/lib/panel-agent/agent --join-token-stdin
sudo systemctl start panel-agent-agent
```

也可以显式传 `--start`，但只有已经初始化并配对的服务适合这样做。已有 Xray 不需要复制时，直接调用 `panel-agent-installer install` 并传 `--core-owner external --xray-bin /绝对路径/xray`；安装器只记录 `owner=external`，使用自己的配置路径，不接管或删除该二进制、配置、服务或其他数据。

## 所有权清单

hy2 可选安装见 [官方 Hysteria 2](hysteria2.md)。安装器接受 `--hysteria-bin` 并校验固定官方资产；只有该内核时生成 hy2-only 服务。打包脚本增加可选 `-HysteriaAmd64Binary`、`-HysteriaArm64Binary` 和 `-HysteriaLicense`，必须一起提供；包内包含二进制、许可证及可导入的 `protocol-extensions/official-hysteria2.zip`。

每个角色在自己的数据根保存 `ownership.json`，记录规范化路径、角色、来源、类型和普通卸载/`--purge` 行为。control 和 agent 使用分离的数据根，因此共机卸载 Panel 不会删除 Agent 的未 ACK 账本；外部 Core 永远不进入可删除路径清单。安装重跑只替换清单已声明且仍为普通文件的项目文件，发现同名外部文件、符号链接、路径逃逸或清单被篡改会拒绝继续。

安装、卸载和清理使用项目数据根下的进程锁串行。数据目录和秘密文件使用受限权限与原子替换；systemd unit、程序入口和日志路径按精确文件处理，不递归删除 `/opt`、`/etc` 或 `/var` 父目录。卸载前会验证 unit 仍带有 Panel-Agent 所有权标记；入口符号链接只解除链接，不会删除其指向的外部目标。

## 普通卸载与 purge

普通卸载删除项目程序、对应 systemd unit 和服务注册，保留数据库、master key、Panel 配置、Agent identity、Core journal、Current/LKG 配置、账本和日志。只有显式 `--purge` 才删除该角色自己的数据根和日志；如果路径不是清单声明的私有根、被替换为符号链接或包含不能安全确认的资源，操作会拒绝。`--purge` 不触碰外部 Core、外部配置、其他 systemd unit 或共机另一角色的数据。

```sh
sudo ./scripts/uninstall.sh ./dist --role control
sudo ./scripts/uninstall.sh ./dist --role agent --purge
```

低盘清理只处理 `tmp/`、`downloads/` 中带 `.tmp`/`.part`/`.download` 后缀的可再生残留，以及过期的 `debug-*` 文件。`control.log`、`agent.log` 和 Agent 的 `core/core.log` 超过 8 MiB 时做 copy-truncate 轮转，当前 Core 日志描述符仍可继续写入；不会删除数据库、密钥、未 ACK 账本、Current、LKG 或 Core journal。清理命令可由管理员或项目专用 timer 调用：

```sh
sudo ./scripts/cleanup.sh ./dist --role panel-agent
```

systemd unit 的 `ProtectSystem=strict`、`ReadWritePaths`、`NoNewPrivileges`、项目级日志限额和固定服务用户共同限制运行时写入范围；不会改变主机全局 journal 保留策略。

精简 Debian 13 的 128MiB VM 可能将 `/run` tmpfs 上限设为不足 16MiB，导致 systemd 以安全缓冲不足拒绝 `daemon-reload`。遇到这个具体错误时先用 `df -h /run` 核对，由管理员将该 tmpfs 上限配置为至少 24MiB；调整容量上限不会预分配这些内存，安装器不会自动修改系统挂载。小内存主机的候选程序应暂存到磁盘目录（例如 `/var/tmp`），避免把大二进制写入以 tmpfs 挂载的 `/tmp`。整机验收应记录这些挂载设置、物理内存、swap 和 OOM 计数。
