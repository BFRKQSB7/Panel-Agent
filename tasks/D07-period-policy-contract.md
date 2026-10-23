# D07 — 独立周期规则与可执行边界样例

TASK ID: D07  
MILESTONE: M3  
STATUS: 只在 [任务索引](README.md) 维护，本文件为静态任务包。

## GOAL

产出 Panel 与 Agent 共用的确定性周期计算和报告类型，防止后续账本各算各的。

## USER VALUE

本任务的可观察结果是：产出 Panel 与 Agent 共用的确定性周期计算和报告类型，防止后续账本各算各的。 后续任务直接使用代码、类型与验证结果，不依赖会话记忆。

## WHY NOW

已有 D05 的必要实现后，可以独立完成此边界。与相邻任务按稳定接口衔接，迁移、API、相关页面和最小测试在本任务内一起完成。

## SIZE

M。M 约半个有效开发日，L 约半天至 1.5 天，仅作初估；模型运行时长和环境等待另计。单次聚焦一个主要机制，不按文件数量硬切分。

## RISK

MEDIUM/HIGH — 跨模块与权限边界风险，按下列合同验收。

## RECOMMENDED MODEL

- CAPABILITY LEVEL: LEVEL 3（任务难度标记，不是模型官方评级）
- PREFERRED MODEL: DeepSeek V4.1 Flash；官方 API 模型名 `deepseek-flash`。
- FALLBACK MODEL: 同一模型；先做有证据的定位与最小修复。无法解决则标明阻塞，不自动换付费模型。
- THINKING: `high`，启用思考。
- WHY: 需要跨少量模块完成一个明确闭环；high 配合固定契约和真实反馈，避免边开发边重新设计项目。
- MODEL RECHECK: 仅项目首次启动、提供方/模型变化、参数不支持时核查；连续任务不重复联网比榜单。当前官方别名对应 1017 版本，第三方平台需查实际映射。

## CAN DOWNGRADE MODEL

NO，本任务首次实现维持上方档位；先减少无关读取与重复验证，不以降低正确性要求节省费用。

## DEPENDS ON

D05

## BLOCKS

D08

只认真实代码与契约依赖；编号是默认执行顺序，不是额外前置。

## CONTEXT

配额维度为 User 全局、User×入口 Node、Machine，周期可以不同；User×Node 默认跟随用户，可覆盖，Machine 独立。实际边界以 UTC 存储并保留 IANA 时区和策略 revision。建议默认每月按设定日重置，短月取最后一天；手动重置建立新 period，策略修改在明确 effective_at 切分。离线 Agent 必须能自行计算未来边界，不只收到当前 period_id。本任务是能运行的共同算法和类型，不是写一份新 RFC；不接真实 Stats 或做数据库入账。

## REQUIRED READING

### Always read

- 本任务全文。

### Source files to inspect

- internal/protocol/
- internal/control/ 用户/节点/机器额度字段
- internal/storage/ 相关模型、internal/period/（若存在）

只检查相关入口及其直接调用/类型。路径为预期定位，实际命名不同就找等价实现；不存在不等于必须创建所有列出的目录。

### Additional docs

- [tasks/README.md](README.md)：依赖状态及连续执行规则。

不要求读全量 PROJECT_PLAN、历史会话、旧任务或其他交接文件。真实契约冲突才定位原计划对应小节。

## EXPECTED BASELINE

D05 用户/节点/机器和额度字段存在；没有可假定的周期或流量协议实现。

## START REALITY CHECK

开始只做一次：核查依赖 DONE 及实际代码/API/Schema；读取相关入口，找出当前可执行验证命令；对照 baseline。命名/目录小差异直接适配，不能因此重新规划。若发现已经实现，验证后复用，不重写。必要环境、能力或契约缺失才标 blocker。

## CONTRACT

- Period identity 包含稳定 scope ID、不可复用 policy_id（或等价内容身份）、policy_revision、start_utc/end_utc，区间左闭右开；三个维度不得共用一个 period_id。revision 只作排序，不足以区分备份回退后同序号的不同策略。
- 默认 UTC；非 UTC 规则需固定 DST 不存在/重复本地时刻的处理并给样例；不把 Go 时间归一化偶然行为当规范。
- 手动重置/策略变更保留旧周期；时钟回拨不重新打开已闭合周期。
- 报告键 K=(machine_id,ledger_epoch,stream_id,segment_id)，segment 绑定不可变 scope/period/policy 元组；相同 K 的归属不得修改。user_entry 有用户/入口/机器三维，machine_only_transit 仅有机器维度，禁止给它虚构用户 period。Core generation 不入账本 epoch。

- byte 总量/配额在线路 JSON 使用规范十进制字符串，存储范围 0..2^63-1；拒负数、小数、指数和越界，所有增量与汇总加法检查溢出。不限用 null，与 "0" 区分；浏览器不得经浮点 Number 破坏精度。
- 已下发策略不可原地覆盖。离线期间管理员改策略/手动重置，未获知的 Agent 按旧 policy 计量；重新同步在最终采样后切段，新 policy 对该机从应用点生效，不追溯猜测旧段。UI 明示待同步和实际切换时刻。同一 scope 的新 policy/period 边界由 Panel 固定，不因每台应用时间产生不同的全局 period；机器应用点只切分该机 segment。旧策略期间仍入旧策略账，明确最终一致窗口，不宣称离线修改即时生效。
- 定义 finalized 累计段及关闭序号 close_seq：每 Machine×ledger_epoch 在冻结时事务分配递增序号（不是创建段时），最终累计不再增长。Panel 仅对已提交且连续的关闭序号推进 closed_through；它是清理用的持久去重边界，不是按时间清空水位。
- ACK 明确标记普通累计确认、final 确认或 retired/closed_through，以及 K/epoch/authority/request。批量记录分别给结果，未提交或无法归属记录不发成功 ACK，不让一个坏记录阻塞其他合法补报。

- closed_through 回执附带用 Panel master key 派生密钥、标准 HMAC-SHA256 库生成的关闭凭据，绑定安装/机器/账本 epoch/前缀/协议版本；签名输入使用版本化、无歧义的规范编码。Agent 持久保存最高已确认前缀及凭据，便于旧备份恢复时验证曾经提交的范围；MAC 不表示丢失字节可重建。

上述是本轮固定的工程默认与边界，不冒充原会话已逐项指定实现；真实证据证明不可行时说明冲突，不静默改变产品行为。

## IN SCOPE

- 实现可复用周期函数、策略校验与有效期切分，写最少可执行 fixtures。
- 定义 user_entry 与 machine_only_transit 两类 stream；后者不携带可扣用户的归属。
- 定义累计报告/ACK 的版本化类型，给跨三种边界分段的具体例子，供 D08/D09 直接使用。

## OUT OF SCOPE

不写完整账本、采样器、配额执行、历史 UI 或新的设计日志。

共同排除：商业系统、QuotaTemplate、Emergency Budget/Lease/Bypass、任意 Shell、Agent 插件运行时、多 Core/多跳、额外常驻依赖；不为未来功能做框架。

## IMPLEMENTATION TARGET

按 IN SCOPE 顺序形成最小可用闭环。先用现有代码/类型解决本项，不提出多套架构选项；只在当前任务产出上做必要重构。开发验证使用隔离数据；不会因任务包含安装/恢复就获得操作生产机器的授权。

## FILES EXPECTED

- internal/period/ 或等价纯函数模块
- internal/protocol/ traffic/ACK 类型、tests/fixtures/ periods

只创建真正必要文件；代码、Schema、测试与正式使用说明就是交接，不新增工作日志。

## MINIMUM TESTS

- 三个不同重置日、31 日短月、一次 DST 转换、手动重置和策略修改代表用例。
- 边界前/等于/后与时钟回拨结果确定，Panel/Agent 共用算法输出一致。

- 大于 2^53 的整数、2^63-1 边界和溢出拒绝；null/0 不混淆；Transit 无用户 period。
- 旧 policy 离线计量后切换到同 revision 不同 policy_id，旧段归属不变；final/close_seq/ACK 样例可供两端共用。

优先复用已有测试；不为简单布局/getter/实现细节镜像写测试，不把编译或 mock 通过当真实代理/资源验证。

## VALIDATION

执行相关 targeted tests 与本任务的一次实际 smoke；只有改动共享契约或发现具体回归风险才扩大范围。根据真实工具链执行命令，回复给出命令/结果/未验证项。失败先判断来自环境、已有问题还是本次改动，不盲跑全套。

## ACCEPTANCE

- GOAL 形成真实可用结果，IN SCOPE 必需行为完成。
- CONTRACT 不变量成立，MINIMUM TESTS 和必要 smoke 有实际证据。
- 三个不同重置日、31 日短月、一次 DST 转换、手动重置和策略修改代表用例。
- 必需环境未测、存在影响合同的失败时保持 PARTIAL/BLOCKED；提供脚本不等于已经运行。
- 对相关既有行为完成下述回归，不以全仓重写掩盖失败。

## REGRESSION

不改权限/连接凭证，不因重置删除历史；不新增 QuotaTemplate。

## STOP CONDITION

本任务达到验收就停止扩大本项范围。用户已授权连续开发时，按 README 更新唯一状态并选择下一项继续，**不用每完成一个任务就等待确认**。出现以下情况停止受影响分支：关键依赖缺失、必要环境不可用、契约冲突，或在最小复现/根因定位后仍无安全且可验证的下一修复方案。同一失败两次修复无效时先停止盲试并定位；有新证据支持的不同方案可继续，不设机械总尝试次数。不得反复执行同一命令、删测试或降低验收来继续。

如果存在其他依赖已完成的任务，可以转做它；没有可执行任务时结束并报告准确阻塞。上下文不足时在完整任务边界结束，保持 README 可恢复；中断后核查现有差异和状态，不凭记忆假定已完成。不擅自启动并行 Agent、购买资源、改变平台权限或公开发布。

日常只更新 README 状态、短验证摘要/阻塞及下一项，不在本任务尾部追加完成日志。真实接口变更时仅修订受影响任务的静态 CONTRACT/EXPECTED BASELINE，必要时更新设计。禁止 HANDOFF.md、SESSION_NOTES、STATE、Decision Log、每日记录和每任务总结文件。若有 Git 且获授权，可一项一个逻辑提交；无 Git/提交权限不构成阻塞。

连续执行中用简短进度汇报，不把固定格式反复写进文件；本轮结束时使用：

```text
TASK RESULT: PASS | PARTIAL | BLOCKED
WHAT CHANGED:
VALIDATION:
KNOWN ISSUES:
NEW RISKS:
FULL REGRESSION RUN: YES | NO
NEXT RECOMMENDED TASK:
NEXT CAPABILITY LEVEL:
NEXT PREFERRED MODEL: DeepSeek V4.1 Flash
NEXT THINKING:
WHY NEXT:
CAN USE CHEAPER MODEL:
MODEL RECHECK:
BLOCKER BEFORE NEXT:
```
