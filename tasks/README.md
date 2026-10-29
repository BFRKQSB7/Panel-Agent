# Panel-Agent 连续开发任务索引

本文件是唯一执行状态源。当前方案按 DeepSeek V4.1 Flash 连续开发重新拆分，共 17 个任务、5 个里程碑；旧 8 个任务包已替换。任务正文是静态工作包，完整产品基线见 [PROJECT_PLAN.md](../PROJECT_PLAN.md)，日常无需全文重读。

CURRENT MILESTONE: M5 — 运维与发布
RECOMMENDED NEXT TASK: [D13-HY2 — 官方 Hysteria 2 内核适配](D13-hysteria2-official-core.md)（继续组合与并发回归、重建最终候选；D17 保留正式资源与长稳验收）
DEFAULT MODEL: DeepSeek V4.1 Flash / `deepseek-flash` / thinking enabled  
DEFAULT THINKING: `high`；D08、D09、D15、D16 使用 `max`

| Task | Milestone | Status | Thinking | Depends on | 验证摘要 / blocker |
|---|---|---|---|---|---|
| [D13-HY2 — 官方 Hysteria 2 内核适配](D13-hysteria2-official-core.md) | M4 | IN_PROGRESS | max | D08, D10, D13, D15 | 双内核、Client、扩展/订阅、统一账本、安装与独立升级已贯通；HTTP/UDP、503 后补账、撤销、2.12.3→2.13.0/失败回滚/日志恢复、Panel 备份恢复通过。Phoenix 原生 arm64 生命周期通过；新增 fullchain 支持、机器级状态序号、双内核→hy2-only→双内核回归及省略内核安全门控，实际旧 Xray 停止/结算/清除授权恢复配置通过。Edge 扩展导入/启用/节点创建及 UDP 引导通过。用户批准懒猫测试冰岛 45003/UDP→18443；凤凰城跨公网 NAT 的 HTTP/UDP DNS、逐账户累计统计和撤销释放端口通过（TX 3715/RX 9190），两端临时目录已清理、原服务状态未变。全仓 test/vet 通过；继续并发/组合回归与候选重建。四机组合、正式资源与固定长稳未验收；所有测试有超时/清理边界。 |
| [D01 — 验证真实 Core、统计粒度与 128MB 原型](D01-core-feasibility.md) | M1 | DONE | high | — | 指定且获用户接受的玩具冰岛 128MiB 整机容器 smoke 通过：16 次 Reality 请求/分入口累计 Stats/重启清零；系统采样峰值 64.1MiB，OOM 零，原服务保留；WSL 回归通过。 |
| [D02 — 启动 Panel、管理员认证与持久设置](D02-panel-admin-foundation.md) | M1 | DONE | high | — | Windows HTTP/HTTPS smoke、初始化/登录/CSRF/注销/重启持久化/缺失密钥拒绝通过；`go test ./...`、Linux amd64 编译通过。 |
| [D03 — 机器注册、身份隔离与主动同步](D03-machine-control-channel.md) | M1 | DONE | high | D02 | 全量 Go 测试、Linux amd64 编译与 Windows HTTPS 进程 smoke 通过；注册恢复/authority/重配/吊销闭环通过。 |
| [D04 — Reality 节点管理与单 Core 安全应用](D04-reality-apply-rollback.md) | M1 | DONE | high | D01, D03 | Node 表单/隔离快照/加密密钥、串行应用与持久恢复完成；玩具冰岛三入口单 Core、故障回滚、离线恢复、已有 TCP/UDP 撤销通过；整机采样峰值 98.6MiB、OOM 零，原服务保留；D03 回归通过。 |
| [D05 — 用户、组与实际代理权限](D05-users-groups-credentials.md) | M2 | DONE | high | D04 | 用户/组/加密正式凭证与批量操作完成；冰岛真实代理隔离、加组授权、轮换、常驻 Agent 离线到期/恢复、缺 key 拒绝通过；当前源码真实 Core 验证 LKG 不恢复已撤销权限及挂起请求到期；D03/D04 回归通过。 |
| [D06 — 双 Token、用户页与三类订阅](D06-dashboard-subscriptions.md) | M2 | DONE | high | D05 | 双 Token/hash/一次性交付与部分重试、私有用户页、HTTP 安全边界和 Chromium 验收通过；冰岛产品 URI/Mihomo 真实代理、LAN/CN 分流及凭证轮换通过，整机采样峰值 87.2MiB、OOM 零、清理通过。按用户确认禁用 sing-box Reality，保留原互通失败证据。 |
| [D07 — 独立周期规则与可执行边界样例](D07-period-policy-contract.md) | M3 | DONE | high | D05 | 固定时区三维周期、不可变策略、精确字节及累计/final/关闭凭据契约完成；Windows/冰岛样例完全一致且清理通过，Chromium 大整数/零/不限往返通过，相关 Go 与权限凭证回归通过。 |
| [D08 — Agent 持久采样、累计账本与重启恢复](D08-agent-durable-ledger.md) | M3 | DONE | max | D07 | 事务采样/水位/冻结累计、按序补发与确认清理、恢复握手和旧快照策略接入完成；Go 针对性、Linux 崩溃/ACK 与冰岛真实流量通过，授权回归整机峰值 100.8MiB、OOM 零且清理通过。 |
| [D09 — Panel 幂等入账与双端补报贯通](D09-panel-idempotent-ingest.md) | M3 | DONE | max | D08 | Panel traffic recovery/ingest routes, transactional K watermarks, raw reports, closed-prefix receipts, row isolation, SQLite/HTTPS tests; go test ./... and go vet ./... pass. |
| [D10 — 三维配额、限制状态与真实用量页面](D10-quotas-and-usage-ui.md) | M3 | DONE | high | D06, D09 | 三维配额/预算持久化、限制原因与到期时间快照、离线/待对账围栏、Agent 应用回报、管理 API/表单与真实用量页完成；定向/全量 Go 测试及 vet 通过。 |
| [D11 — 隐藏 Exit、一跳转发与扣账归属](D11-one-hop-exit.md) | M4 | PARTIAL | high | D10 | 原双机转发/归属/轮换/硬停证据保留于 `.work/d11-20261025T081717155Z-35972`。新 Debian VM 常驻组合发现权限围栏丢弃 Exit/Transit 元数据，使本地重渲染停止 Exit、拒绝 Entry 并阻断补报；已修复完整快照保留、Transit/Exit 旧授权交集、路由变更门控。真实 Xray Entry/Exit 连续重渲染不重启、Transit 轮换不恢复旧凭据、已有 LKG 撤销回归及全仓 test/vet 通过；三 VM 修复版单用户两 Direct/One-Hop 与 512MiB 共机 hy2 HTTP/UDP 已通过。继续多用户/常驻入账复验，尚不能恢复 DONE。 |
| [D12 — 动态 NAT/DNS 教程与有限连通性检查](D12-nat-dns-guidance.md) | M4 | DONE | high | D06 | 节点详情动态生成 NAT/DNS/Reality/Agent/Core 引导，复制按钮与 `public/local` 有限 TCP 检查完成；一次 DNS 解析绑定地址，拒绝私网、回环、mapped IPv6 与 DNS 重绑定；Go 全量测试/vet、CSP 回归及真实浏览器剪贴板/API smoke 通过。 |
| [D13 — 受限协议扩展的完整生命周期](D13-bounded-protocol-extensions.md) | M4 | DONE | high | D06 | 受限 ZIP 声明式包、固定 Trojan/TCP/TLS Xray 与 URI/Mihomo renderer、加密 secret、Node 显式版本引用、启停/删除引用保护、管理页面与规范完成；Xray 26.10.21 `run -test`、浏览器管理 smoke、Go 全量测试/vet、diff check 通过。公网代理握手与证书链未在本项宣称验证。 |
| [D14 — 安装、文件所有权与两级卸载](D14-install-ownership-cleanup.md) | M5 | DONE | high | D04 | Phoenix Ubuntu 24.04.5/aarch64 默认 `panel-agent` 非 root 安装、管理员更新、普通卸载保留、幂等重装与 purge 通过；runtime Core、ownership、DB/key/identity、日志属主和 root 程序入口符合边界，Agent unit 引用数据目录 runtime。当前修复后的 Debian WSL amd64 发布 smoke 另覆盖服务账户、两把锁、更新、卸载/重装/purge、归档校验；Go 全量 test/vet、Linux amd64 编译、shell/diff check 通过。证据 `.work/d14-d15-nonroot-20261025/evidence.md`。 |
| [D15 — 手动 Core 升级与 CLI 更新回滚](D15-manual-upgrade-rollback.md) | M5 | DONE | max | D16 | 官方包校验后的 `26.9.30 → 26.10.21` 成功升级、重复请求、启动失败回滚、prepared/stopped/swapped 子进程退出恢复及代理/Stats/未 ACK 账本保留通过；Iceland Alpine x86_64 由 `nobody` 非 root 真实运行五场景矩阵，均 `proxy=ok`。同时完成 Agent/Panel 管理员更新后的锁属主继承修复，服务账户可重新打开 `agent.lock`/`panel.lock`；128MiB 整机、48MiB 低盘、下载失败、Agent 七场景、Panel 六场景及现有权限/到期 Core 回归仍保留原证据。arm64 独立 Reality 夹具请求复位，未计入通过，未停止主机现有服务；D17 仍负责双架构组合与资源门槛。证据 `.work/d14-d15-nonroot-20261025/evidence.md` 与 `.work/d15-20261025T084212199Z/host-final.txt`。 |
| [D16 — 一致性备份、恢复与 Agent 重连](D16-backup-restore-reconcile.md) | M5 | DONE | max | D09, D13, D14 | 一致性 `VACUUM INTO` 快照、有界 Argon2id/AES-GCM 备份包、扩展/密钥/安装身份校验、journal 化旧代回滚与启动恢复、维护屏障、authority/会话/Join/操作凭据失效、Agent TLS `SyncOnce` 重连及 receipt 证明的 recovery gap 完成；全量 Go 测试、vet、diff check、并发快照与未提交切换回滚测试通过。 |
| [D17 — 完整发布回归与资源验收](D17-release-resource-gate.md) | M5 | WAITING | high | D11, D12, D15 | 等待 D11 常驻组合修复复验。`.work/release-hy2-ed20e0d-20261029` 双架构候选源码/96 项校验和/执行权限核对通过，但早于新权限围栏修复，需重建。Phoenix 原生 arm64 新模式切换/转发/补账与定向 Linux race 通过。已创建 Debian 13.7/6.12.107 三台 KVM VM（1C/512MiB+两台1C/128MiB，每台1GiB盘、无swap）；首轮双内核128MiB出现真实OOM，原日志保留 `.work/vm-fixture/before-fix`。128MiB `/run` 默认上限不足导致 systemd reload 拒绝，隔离环境改24MiB上限；候选暂存使用磁盘而非 `/tmp` tmpfs。90秒失去心跳停止VM已验证；完整最低规格、四机/跨周期和固定长稳仍未通过。 |

M1 D01–D04：Core/后台/接入/节点；M2 D05–D06：用户与订阅；M3 D07–D10：周期/账本/入账/配额；M4 D11–D13：一跳/教程/扩展；M5 D14–D17：运维/发布。运维实际顺序为 D14→D16→D15→D17，先验证备份恢复再实现含 Schema 迁移的 Panel 更新，编号不代表依赖顺序。

## 连续执行与状态维护

- 状态取 WAITING、READY、IN_PROGRESS、DONE、PARTIAL、BLOCKED。依赖全部 DONE 才可执行；编号仅是默认选取顺序。D01 与 D02 无相互依赖，D01 缺测试环境时可继续 D02/D03，不得假装 D01 已通过来开始 D04。
- 每次只执行一个任务，只读它、此索引及指定源码。开始核查一次实际基线；完成必要验证后更新该行状态和一条短验证摘要，刷新后继 READY 与下一项，自动继续。无需在每项完成后等待用户确认。
- 优先继续未完成且可恢复的 IN_PROGRESS；否则取依赖满足、无真实 blocker 的最小编号任务。BLOCKED/PARTIAL 仅在前提变化后重试，不把它们当 DONE。只有相关分支阻塞时转做可独立执行的任务。
- 同一失败两次修复无效后，先建立最小复现并定位根因；有新证据支持的不同方案可继续。只有缺少必要前提或没有安全可验证方案时记录 blocker，禁止重复盲试、跳过失败测试或降低验收。没有可执行项、用户叫停、预算/上下文无法安全继续时结束并报告。
- 中断恢复先读索引与当前源码差异；DONE 表示必要验收有证据，不是模型自述。后续改动若破坏先前合同/验证，将受影响项改为 PARTIAL，并将依赖其有效性的后继置 WAITING；仅复验实际受影响范围，不能继续依赖失效的 DONE。上下文不足尽量在任务边界结束；任务中断保持 IN_PROGRESS/PARTIAL 与一行准确待办。
- 日常只维护本索引；不创建 HANDOFF、STATE、Decision Log、每日记录或任务完成总结。静态契约确实变化才修订对应任务，不追加交接日志。每项不默认全量回归，D17 才集中发布验收。
- 本轮规划未运行产品代码；真实 Linux/128MB/双架构/NAT 环境由对应任务检查。连续开发不授权购买资源、操作未指定生产机、自动并行 Agent、推送或公开发布。

## 模型设置

2026-10-22 核查官方文档：`deepseek-flash` 当前对应 V4.1-Flash-1017，思考档位为 `low/high/max`。第三方服务需确认实际映射；`xhigh` 在官方映射为 `high`，不能当作 `max`。默认启用思考，只对四项状态/恢复任务提高到 max，不自动换模型。

这是本项目的任务设计判断，并非实测证明某模型可以无监督完成全部项目。首轮 D01–D04 后按失败类型和实际耗时校准范围，优先缩小当前问题，不重新扩成大任务。

参考：[官方接入](https://api-docs.deepseek.com/quick_start/agent_integrations/)、[思考参数](https://api-docs.deepseek.com/guides/thinking_mode/)、[1017 更新](https://api-docs.deepseek.com/news/news261017)。

## 交给 Coding Agent 的启动指令

```text
在 E:\project\Panel-Agent 连续开发。先读取 tasks/README.md，按照其状态和依赖，
从推荐的可执行任务开始，一次完整处理一个任务：读取对应任务包及指定源码，
实现、验证、更新唯一索引，然后自动继续下一项，不要每项完成后询问我。

使用 DeepSeek V4.1 Flash，默认 high；D08/D09/D15/D16 用 max。
若工具不能调整档位，不虚构已切换；按可用档位工作并在相关高风险任务说明限制。
只执行本套 D01–D17，不恢复旧任务或重新拆分全项目。
保留全部 V1 要求，遇到阻塞按索引规则转做无依赖任务或准确停止。
不以提供测试脚本代替实际通过，不伪造资源指标，不新增交接/状态文档。
在本轮无法继续时，按当前任务末尾的结果格式汇报。
```

