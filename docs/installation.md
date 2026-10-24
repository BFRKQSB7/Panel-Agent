# Linux 安装、所有权与卸载

Panel-Agent 的安装器只接受已经由发布流程校验过的本地二进制和 Xray 文件，不接受任意 URL、Shell 或远程命令。支持 Debian 12+/Ubuntu 22.04+ 的 amd64/arm64 systemd 主机；安装器不会自动改 DNS、防火墙、sysctl、BBR 或 nginx。

## 构建和安装

在发布目录生成 Linux 二进制，并把已经固定版本、已核对 SHA-256 的 Xray 路径放入 `PANEL_AGENT_XRAY`：

```sh
PANEL_AGENT_XRAY=/path/to/verified/xray ./scripts/build-linux.sh dist amd64
sudo ./scripts/install.sh ./dist --role panel-agent
```

`scripts/install.sh` 先在发布目录执行 `sha256sum -c SHA256SUMS`，校验不通过就不会调用安装器。`--role control` 只安装 Panel，`--role agent` 只安装 Agent，`--role panel-agent` 安装两者；安装创建 `/opt/panel-agent`、`/var/lib/panel-agent/{control,agent}`、`/var/log/panel-agent` 和项目专用 systemd unit。默认只执行 `systemctl enable`，因为 Agent 必须先完成配对；管理员初始化 Panel 后可分别启动：

```sh
sudo -u panel-agent /opt/panel-agent/bin/control admin init --data-dir /var/lib/panel-agent/control --password-stdin
sudo systemctl start panel-agent-control
sudo -u panel-agent /opt/panel-agent/bin/control-agent pair --panel-url https://panel.example --data-dir /var/lib/panel-agent/agent --join-token-stdin
sudo systemctl start panel-agent-agent
```

也可以显式传 `--start`，但只有已经初始化并配对的服务适合这样做。已有 Xray 不需要复制时，直接调用 `panel-agent-installer install` 并传 `--core-owner external --xray-bin /绝对路径/xray`；安装器只记录 `owner=external`，使用自己的配置路径，不接管或删除该二进制、配置、服务或其他数据。

## 所有权清单

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
