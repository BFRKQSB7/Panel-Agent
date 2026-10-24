# Agent 流量采样与恢复

Linux Agent 在启用受管 Core 时自动打开本机账本。常驻 `run` 每 5 秒采样，也会提前唤醒采集已知周期／策略边界；采样独立于控制通道的网络请求，Panel 失联仍可继续。一次性 `sync` 只采集本次运行及配置切换，退出后不会持续采样。真实统计源是固定 Xray 26.10.21 的非清零累计 Stats：正式入口按用户×入口 Node 的稳定 email 标签生成 `user_entry`，隐藏 Exit 的 Transit 凭证生成 `machine_only_transit`；后者只绑定机器周期，不计入用户全局、用户×入口或某个出口 Node，开发凭证不计正式用户账。

## 本地文件与事务

账本位于 Agent 数据目录中的 `agent.ledger.sqlite`，使用现有 modernc SQLite 依赖、WAL、`synchronous=FULL` 和一个连接；打开时检查 WAL/FULL 实际生效。缓存目标约 1MiB，128 页自动 checkpoint，重用日志文件上限设为 1MiB；这不代表未确认业务数据的大小上限，也不能限制长期读事务占用的 WAL。SQLite 参数依据见 [WAL 文档](https://www.sqlite.org/wal.html) 和 [synchronous](https://www.sqlite.org/pragma.html#pragma_synchronous)。账本需要本机文件系统，不能把数据目录放在网络共享上。

`ledger.identity.json` 先固定机器及随机 ledger epoch；初始化完成后数据库丢失、身份不匹配、损坏或权限不符合要求都会报错，禁止静默新建账本重扣历史。普通 Agent 重启、配置更改及 Core 重启保持 ledger epoch。Core generation／instance ID 另行保存，并结合 Linux boot ID、PID、启动 ticks、配置路径和实际进程参数证明实例连续性；新实例计数即使高于旧水位，也按新实例处理。不能证明连续的旧实例只建立基线并记录断档，不把整个未知历史当作新增流量。

每次样本把源实例、原始采样水位、累计段、策略身份和计量时钟下限放进同一事务，**提交成功后才可发送**。非清零计数重读不重复累计；同实例计数回退直接拒绝。真实 SQLite 写入失败或提交失败都不会推进水位，也不会删除未确认数据。数据目录要求 0700，数据库、身份文件和私有状态为 0600；不要单独删除数据库、WAL、身份文件或关闭凭据。

## 周期、策略与 Core 切换

Panel 为用户全局、用户×入口及机器分别持久创建默认月策略，scope 的策略身份可在多台机器间复用；它们随机器快照下发。旧版本已有配置缺少周期策略时，Panel 启动会刷新该快照，保留节点与凭证；正常重启不会反复增加版本。尚未配置节点的初始注册仍保留空快照。默认 UTC／每月 1 日；D10 提供全局、用户×节点和机器预算的编辑、继承、重置与状态界面。共同算法及固定时区版本见 [周期与流量协议](period-and-traffic.md)。已发布 ID 不允许换内容，同 scope 同生效点也不能换成另一身份。

一个流同时绑定三个周期的不可变元组；Transit 只绑定机器周期。周期边界取并集，旧段冻结后分配 Machine×ledger_epoch 下唯一递增 close_seq，并在同一事务保存最终累计；冻结段不再增长。离线跨过多个边界时，不创建伪造的中间分摊：含糊增量记在采样完成时可确定的最新已知周期，并保存整个含糊区间。旧策略期间的数据继续归属旧策略。同步新策略时先最终采样并冻结，再切新段；机器应用点保存在 Core journal 的 `policy_applied_at`，不会改变 Panel 固定的全局 period_id。

切换 Core／删除标签前保留带原映射的最终累计边界，账本提交后才能继续切换。崩溃后重放同一边界不会重计；即使边界文件先落盘、journal 引用尚未写入，也会恢复当前实例遗留样本。早于账本最新水位的历史样本不回退水位。账本已接收且停止状态落盘后，才耐久删除边界文件；旧源水位可清理，未确认段仍保留。旧版本没有周期策略的边界、纯开发流量的边界保留为不可直接扣账的原始证据，原队列 126／128 条保护仍有效。

**不保证绝对零漏计或固定误差上界。** 上下行查询、采样和停进程并非一个原子操作；最后采样至停止之间、物理崩溃前尚未读取的 Core 内存计数可能丢失。跨边界、长时间停机／休眠、时钟跳变及策略应用间隔会留下 `uncertainty` 的保守时间包络，`no_error_bound=true` 不表示已测得误差值。时钟回拨使用持久 high-water，不重新打开旧周期。

## 报告与确认

发送端访问配对的 HTTPS Panel 原点，使用已有机器 Bearer；不接受重定向。每次发送先请求 `/api/v1/agent/traffic/recover`，提交已分配序号、仍持有的 final 范围和最高关闭凭据。响应绑定安装、机器、ledger epoch、authority 及 request；Agent 重启或 authority 变化后，在恢复握手完成前不能消费 ACK 或清理。Panel 关闭前缀低于本地证明时暂停清理，保持原序号并等待恢复对账，不能自行清零。

`/api/v1/agent/traffic` 每批最多 256 段，优先按 close_seq 补发 final，再发当前累计。报告 K、归属及金额来自已持久化状态；重试可用新 request_id，K 保持不变。成功／失败逐项处理；未知归属项目保留并退避至少一分钟，不影响其他可处理项目。普通累计 ACK 不能超过本机已持久金额、不能回退；final 确认精确最终累计；retired 不虚构零流量，必须由连续关闭前缀覆盖本机实际已分配且已持久标记发送的序号。

只有关闭前缀与完整凭据在**同一个清理事务**中保存成功，才删除被覆盖的本地 final 明细；普通或单独 final ACK 不足以删除。序号有洞就不能跨越清理。Agent 检查凭据作用域和格式，通过已认证通道信任 Panel；它不持有 master key。Panel 用 master key 派生的 HMAC-SHA256 验证恢复证明，证明只能指示曾提交范围，不能重建缺失字节。

D09 已接入正式 Panel 恢复与入账端点：Panel 以 `(machine_id, ledger_epoch, stream_id, segment_id)` 做幂等键，保存累计水位、原始逐行报告和 period 用量，逐行隔离坏行，并只对连续 final 前缀签发关闭回执；数据库事务失败不会返回成功 ACK。D10 在此基础上接入三维额度执行、限制原因/应用回报、待对账围栏和真实用量页面；页面不把最终一致余额误报为严格零超用保证。

## 诊断与验证

`control-agent ledger --data-dir /path/to/agent` 可在常驻 Agent 运行时读取一致诊断，不启动 Core、不重建数据库：包括 epoch、已分配关闭序号、最高回执、pending/final 数量、可用磁盘、最近 128 条诊断事件及最多 32 个待确认段预览。预览不代表全部历史。每分钟检查磁盘，低于 16MiB 时告警；采样失败只在状态改变时记录日志。未 ACK 数据不会为了腾空间而删除，SQLite 已释放页可供后续累计复用。

本地针对性验证：`go test ./internal/quantity ./internal/period ./internal/protocol ./internal/security ./internal/agent/ledger ./internal/agent ./internal/core/xray ./internal/storage ./internal/control ./cmd/agent`。子进程在样本和 ACK 提交前后直接退出，核对恢复、重复报告、部分失败、序号缺口及证明保存；另用 SQLite 拒写与提交故障注入检查水位不推进。这些是进程中断／写入故障测试，不是物理断电或真实磁盘耗尽实验。

`bench/run-d08-smoke.ps1` 在获授权的玩具冰岛隔离目录执行真实 Xray 及 Linux 账本故障套件，`bench/run-d05-smoke.ps1` 回归实际 Panel／Agent 的授权、凭证、撤销和已有连接行为；两者均验证原服务、监听、OOM 计数并清理本次临时目录。实测结果放在 `bench/results/`，不把测试接收器通过当成 D09 正式 Panel 入账验收。
