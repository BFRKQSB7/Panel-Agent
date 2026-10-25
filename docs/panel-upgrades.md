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

Panel 自身使用 `control update` 本机流程。它要求停止正在运行的 Panel 进程，先写 D16 完整备份到数据目录的 `backups/`，校验候选的 SHA-256 和固定 `control version` 输出，再用候选程序的 `control check` 在隔离的 `panel.db`/`master.key` 副本上实际跑迁移、解密和初始化检查，随后才在同一目录原子替换并运行版本健康检查。旧可执行文件只在候选检查通过后删除；Windows 或无法获取 Panel 进程锁时会拒绝切换，不会覆盖正在运行的实例。Windows 更新进程可能暂时占用已重命名的旧版本，因此健康更新成功但清理失败时会保留 `.previous` 并由下一次更新尝试清理。Agent 读取 D14 ownership manifest 后只允许替换 `owner=project` 的 Core，`owner=external` 会明确拒绝升级。

```sh
control update --data-dir /var/lib/panel-agent/control \
  --package /tmp/control \
  --target-version 0.2.0 \
  --sha256 <panel-binary-sha256> \
  --backup-password-stdin
```

更新流程不承诺未验证的 Xray 新能力自动兼容。当前配置测试、Stats/监听健康、固定扩展 renderer 和 Transit 配置是最低门槛；真实公网握手、NAT、双机链路和 128MiB 峰值仍需在相应发布矩阵中单独验收。
