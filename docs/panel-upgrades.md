# 手动更新与回滚

机器管理页的“手动 Xray 升级”只创建绑定当前 `authority_epoch`、Machine、目标版本、官方 release 来源、ZIP 大小和 SHA-256 的 Upgrade Intent。Panel 不接受任意下载 URL、命令或服务名；Agent 主动拉取后按自身 Linux 架构生成受控官方 Xray release 地址。旧 authority 下的 Intent 不会在恢复或新握手后重放，管理员需要重新创建。

Agent 先检查可用磁盘，再下载到 `agent-data/core/upgrades/<intent-id>/`，限制下载和解压大小，只提取根目录 `xray`，验证包和候选文件校验值及目标版本。候选会用当前完整配置执行 `xray run -test`，然后在同一 Core 锁内采样并停止旧进程；旧二进制保存在私有 rollback 路径，候选通过 Stats、监听和进程身份健康检查后才提交。升级不会重建 `ledger_epoch`，Core generation 会独立递增，边界观察仍由既有账本消费。

下载、版本、配置或健康失败会回报明确结果；在已经切换候选的情况下，Agent 恢复旧二进制和旧配置并再次健康检查。回滚自身失败时保留升级 journal 和现场，状态不会伪装为成功，下一次 Agent 启动会按 journal 选择安全恢复路径。重复拉取同一 Intent 会继续 `running` 状态，不重复创建另一项任务；重复终态回报幂等处理。

Agent 本机可用明确的 CLI 包执行同一受控切换：

```sh
control-agent upgrade --data-dir /var/lib/panel-agent \
  --xray-path /opt/panel-agent/bin/xray \
  --package /tmp/Xray-linux-64.zip \
  --target-version 26.10.22 \
  --sha256 <official-zip-sha256>
```

Panel 自身使用 `control update` 本机流程。它要求停止正在运行的 Panel 进程，先写入并重新解密验证 D16 完整备份，再用候选程序的 `control check` 在隔离的数据库、主密钥与扩展代上实际跑迁移和初始化检查。检查通过后一起提交候选程序和已迁移的数据代；迁移失败或切换后的健康失败会恢复完整旧代。`.panel-self-update.json` 与 D16 journal 共同记录提交决定，中断发生在提交前时，下次启动恢复匹配的旧程序与旧数据，并要求重新启动；提交后中断则保留新代并补完清理。回滚缺少匹配原程序或校验失败时保留现场并报错。Windows 更新进程可能暂时占用 `.previous`，健康更新后保留它并由后续启动重试清理。Agent 读取 D14 ownership manifest 后只允许替换 `owner=project` 的 Core，`owner=external` 会明确拒绝升级。

```sh
control update --data-dir /var/lib/panel-agent/control \
  --package /tmp/control \
  --target-version 0.2.0 \
  --sha256 <panel-binary-sha256> \
  --backup-password-stdin
```

更新流程不承诺未验证的 Xray 新能力自动兼容。当前配置测试、Stats/监听健康、固定扩展 renderer 和 Transit 配置是最低门槛；真实公网握手、NAT、双机链路和 128MiB 峰值仍需在相应发布矩阵中单独验收。

Agent 程序自身用 `control-agent update --data-dir /var/lib/panel-agent/agent --package /tmp/control-agent --target-version 0.1.0 --sha256 <agent-binary-sha256>` 更新；与更新 Xray 的 `upgrade` 是两个命令。先停止 Agent 服务，再执行本机命令；运行中的 Agent 持有 `agent.lock` 时会拒绝更新。安装清单存在时只更新该安装所拥有的 Agent 二进制。候选必须通过精确的 `version` 和只读的 `check --data-dir`，V1 仅支持不迁移身份/账本格式的兼容更新。程序以同目录原子替换，`agent.program-update.json` 保留中断恢复决定；检查失败恢复原程序，Core、身份文件、账本 epoch 与未 ACK 段不被更新器重建。恢复回滚后重新启动 Agent；新进程按运行程序版本上报版本号。

升级包暂存使用流式解压和文件 SHA-256，磁盘预检包含 ZIP、最大候选大小及回滚余量。128MiB 机器的验收使用 `GOMEMLIMIT=32MiB GOGC=50`，须同时记录整机内存、swap、OOM 前后计数与磁盘最小余量；Go 的软目标本身不构成资源通过证据。

历史升级测试使用未修改的官方 `26.9.30` → `26.10.21` 二进制及已核对的包 SHA-256，只在测试进程暂时允许历史版本。正式程序最低版本仍为 `26.10.21`，没有降低门槛的 CLI 或环境变量。编译 `go test -c ./internal/agent` 后，可用 `PANEL_AGENT_TEST_XRAY_OLD`、`PANEL_AGENT_TEST_XRAY`、`PANEL_AGENT_TEST_XRAY_PACKAGE` 显式指定两个 ELF 和当前官方 ZIP，运行 `-test.run TestRealHistoricalCoreUpgrade -test.v`。该测试覆盖真实代理、Stats/未 ACK 账本、重复升级、启动失败回滚及三个持久阶段的进程退出恢复。

`bench/d15-resource.sh AGENT_TEST OLD_XRAY NEW_XRAY OFFICIAL_ZIP` 在 systemd Linux 的 128MiB/no-swap cgroup 中执行同一测试；`bench/d15-lowdisk.sh AGENT_TEST OLD_XRAY NEW_XRAY` 在本机隔离的 48MiB tmpfs 验证低盘拒绝。二者需要 root。`bench/d15-host-smoke.sh RUN_DIR HOSTNAME PRIVATE_IP` 仅用于已授权的真实 128MiB 整机容器，要求私有目录内已有校验过的 `agent.test`、`old-xray`、`new-xray`、`current.zip`，记录原有服务在内的系统峰值并清理测试进程。cgroup 结果与整机结果分别记录。
