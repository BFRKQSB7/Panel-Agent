# Panel 一致性备份与恢复

管理员可在设置页下载备份，或调用 `POST /api/admin/backup/export`（JSON `{"password":"..."}`，需要登录会话和 `X-CSRF-Token`）。备份包外层是有界 JSON envelope，内部为固定条目的 ZIP：SQLite 由 `VACUUM INTO` 生成一致性快照，另含 `master.key`、版本/校验元数据和数据库内协议扩展的校验副本。设置密码时使用 Argon2id 派生 AES-256-GCM 密钥；错误密码、篡改、未知条目、路径逃逸、压缩/展开超限都会在替换现有数据前拒绝。

恢复使用设置页上传，或 `POST /api/admin/backup/restore` 的 multipart 字段 `backup`、`password`，需要 `X-CSRF-Token`。安装身份必须与当前 Panel 相同；恢复先在 0700 暂存目录打开数据库、运行迁移并解密管理员设置、用户凭据、节点密钥、机器快照、Exit/Transit 凭据和扩展，全部通过后才切换 `panel.db` 与 `master.key`。切换通过 `.panel-restore-journal.json` 记录，authority 完成新 epoch、旧 Join Token/操作回执清理、Agent 交通状态置待对账并成功重开后才提交；进程中断时启动前自动回到完整旧代或保留完整新代。

恢复成功会清空内存登录会话，所有 Agent 通过 D03 的 nonce/full-snapshot 握手接收恢复快照；不会凭 revision 整数拒绝合法的新 authority。恢复不会自动补造备份之后已经由 Agent 清理且 Panel 也没有明细的流量。若 Agent 的关闭凭据能证明某个连续范围已经关闭，管理员还必须确认该范围不在 Agent `held_final_ranges` 且 Panel 没有任何明细，才能在机器页登记 `recovery gap`；登记只推进关闭前缀并写证据，不增加字节。仍有明细、缺少 master key 或凭据不匹配时保持待对账。

备份元数据中的监听地址和 TLS 路径只描述当前运行配置，不会在热恢复中改动已经绑定的监听器。若恢复到不同部署，应在停机切换后按元数据重新配置环境变量和证书路径，并重新验证 HTTPS、Agent 重连与流量对账。
