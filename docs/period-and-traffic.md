# 周期与流量协议

`internal/period` 是 Panel 与 Agent 共用的纯计算模块；`internal/protocol/traffic.go` 是版本 1 累计报告与确认契约。D07 提供算法、校验和可执行例子，D08 已接入 [Agent 持久采样与恢复](agent-ledger.md)，D09 已接入 Panel 正式恢复/入账端点、K 水位、逐行报告、连续关闭前缀与签名回执，D10 已接入三维额度执行、待对账围栏和真实用量页面。既有用户配额 API 已采用下述精确字符串格式，SQLite 原有整数列无需迁移。

## 周期规则

三种 scope 为 `user(user_id)`、`user_node(user_id,node_id)`、`machine(machine_id)`。User×入口 Node 默认继承用户的规则值，也可覆盖；继承时仍须发布独立 scope 的策略和周期身份。机器规则独立。默认 UTC，每月设定日的 00:00:00 重置，31 日在短月取该月最后一天；其他时分秒须显式指定。区间左闭右开。

策略携带随机、不可复用的 `policy_id`、revision、scope、固定 `effective_at` 以及时区规则。`CheckPublication` 拒绝同 ID 改写内容及同 scope 同生效点的冲突身份。revision 不能单独充当身份：从备份恢复后，相同 revision 的新 ID 仍是新策略。周期 ID 是版本化、固定顺序 JSON 编码中 scope、策略 ID/revision、UTC 起止点的 SHA-256；接收入账时还必须用已授权策略重新计算并核对，不能信任报告自行提供的边界或 scope。

已知策略的在线切分点是新策略的 `effective_at`。**旧周期保存原有计划边界，不反向改写其 ID 或结束点**；离线 Agent 未收到新策略时仍记旧策略账。同步时最终采样旧段、冻结，再从实际应用时刻建新段；新段对应的全局周期仍由 Panel 发布的策略决定。即使两个机器分别在 15 日和 20 日应用，若都在新策略同一周期内，也必须使用相同 period_id。旧策略有效数据与新策略周期可存在时间重叠，这表示已披露的最终一致窗口，不应重分配或删账。

手动重置通过 `New` 发布新 ID 和固定生效时间完成，不删除旧策略、旧周期或旧账。`Resolve` 将计量时刻下限限制为已持久保存的 high-water；时钟回拨不重新打开已关闭周期。D08 必须将 high-water 与段冻结一并持久化，恢复时使用它；此规则不声称修改了系统时钟。`Split` 计算所有已知 scope 的边界交集，最多 4096 段，**不对缺少采样的字节按时间比例摊分**。

## 固定时区与夏令时

`internal/period/zoneinfo.zip` 来自本项目实测 Go 1.27.1 工具链的 `lib/time/zoneinfo.zip`，随代码固定；SHA-256 为 `b2d18a7c8fa8142097a48c99609fb3c92db5ee98bc740294e57eab8ae9f94779`。Go 分发许可见同目录 `TZDATA-LICENSE`，IANA 时区数据属于公共领域。规则带 `tzdata_id`，不使用宿主系统时区文件或 `ZONEINFO`；未知数据版本拒绝。后续升级不得丢弃仍被历史策略引用的数据版本。当前只支持该固定版本；正常周期输入为 1970 至 9998 年 UTC，边界超出范围拒绝。

重复本地时刻选择第一次出现（较早 UTC）；不存在的本地时刻取跳变后第一个有效时刻。通过明确遍历 zone intervals 计算，不依赖 `time.Date` 在 DST 歧义中的选择。以下是实际算法样例：

| 本地目标 | UTC 结果 | 规则 |
|---|---|---|
| New York 2024-03-10 02:30 | 2024-03-10 07:00Z | 缺失时刻移至 03:00 |
| New York 2024-11-03 01:30 | 2024-11-03 05:30Z | 重复时刻取第一次 |
| Lord Howe 2024-10-06 02:15 | 2024-10-05 15:30Z | 半小时缺口移至 02:30 |
| Apia 2011-12-30 12:00 | 2011-12-30 10:00Z | 被跳过的一天移至当地 31 日 00:00 |

日历依据：[Go ZoneBounds](https://pkg.go.dev/time#Time.ZoneBounds)、[Go Date 的 DST 歧义说明](https://pkg.go.dev/time#Date)。

## 精确金额与归属

所有流量字节及配额 JSON 值必须是无前导零的十进制字符串，例如 `"9007199254740993"`，范围 0..9223372036854775807。数字 JSON、负数、小数、指数、转义数字及越界全部拒绝；不限为 `null`，零为 `"0"`。Go 使用 `quantity.Bytes`，SQLite 使用有符号 64 位整数；增量、上下行合计和维度汇总必须使用 `quantity.Add` 检查溢出。网页表单/API 保留字符串，不经 JavaScript `Number`。

报告 K 是 `(machine_id,ledger_epoch,stream_id,segment_id)`；Core generation 不进入 K 或 ledger epoch。`user_entry` 恰有用户、用户×入口、机器三个 period；`machine_only_transit` 只携带机器 period，禁止用户和入口字段。段的 K、stream、start、全部 period/policy 绑定不可变；同一 stream 跨段的归属也由后续持久账本检查。上下行累计及 through 单调，跨周期时冻结旧段再创建新 K；无法精确分割的观测必须另行披露，不能伪造分界样本。

`finalized` 与正数 `close_seq` 同时出现。序号在冻结事务中按 Machine×ledger_epoch 分配，不能创建段时预占；冻结后总量、through 和序号都不变。报告的累计值代表同一段总量，禁止当作每次新增量相加。批量 wire 使用原始 JSON 行逐项解码；无效金额/未知字段只使该行失败，合法行继续处理。外层身份、版本、字节大小和认证仍须在入口检查。序号使用十进制字符串；当前范围同 SQLite 正整数。

## ACK 与关闭凭据

ACK 绑定协议版本、request_id、authority_epoch、machine_id、ledger_epoch，并按 row + K 返回结果。`cumulative` 只确认已提交累计，不准删除段；`final` 确认精确最终总量与 close_seq；`retired` 不虚构零金额，必须附覆盖该 close_seq 的已确认连续前缀。`rejected` 只返回错误码，不携带已提交总量。未知归属、认证错误或尚未提交的行不能成功确认。重复/迟到的较低累计值由入账端识别并返回已持久化高值，不能回退累计。

`AdvanceClosedThrough` 只能接收同一事务已提交的关闭序号：收到 1、3 时前缀只到 1，之后提交 2 才能推进到 3。此边界用于持久去重和清理，不能按时间清空，也不能用最大已见序号替代。D08/D09 负责真实事务、去重索引、前缀保存和清理执行，本任务的纯函数不替代这些存储保证。

关闭凭据先以 master key 作 HMAC-SHA256、固定用途标签派生密钥，再以标准 HMAC-SHA256 签署版本化固定顺序 JSON 字符串数组。字段包含安装 ID、机器、ledger epoch、closed_through 和协议版本，MAC 用无填充 Base64URL。与操作 Token 签名采用不同用途标签。Agent 通过已认证通道接收，持久保留最高前缀和凭据；**Agent 不持有 Panel master key，也不能自行验 MAC**。Panel 从旧备份恢复后验证凭据，可识别曾提交范围，仍不能重建已经丢失的字节；恢复策略由 D16 接入。

## 可执行例子

`go run ./bench/d07-smoke` 输出固定 JSON：1/15/31 日三维边界交集、DST gap/fold、同 revision 新策略、离线归属、时钟回拨、active/final/retired 报告与凭据。`bench/run-d07-smoke.ps1` 在 Windows 和指定 Alpine VPS 运行同源码，比较输出并清理仅本次创建的临时目录。针对单元与既有 API 的回归命令：`go test ./internal/quantity ./internal/period ./internal/protocol ./internal/security ./internal/storage ./internal/control`。资源、真实代理与完整发布验收仍由对应任务负责。
