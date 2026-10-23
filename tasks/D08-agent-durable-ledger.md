# D08 — Agent 持久采样、累计账本与重启恢复

TASK ID: D08  
MILESTONE: M3  
STATUS: 只在 [任务索引](README.md) 维护，本文件为静态任务包。

## GOAL

把真实 Core 统计可靠转换成持久周期累计，重启和重试不重复累计，未确认数据不会丢。

## USER VALUE

本任务的可观察结果是：把真实 Core 统计可靠转换成持久周期累计，重启和重试不重复累计，未确认数据不会丢。 后续任务直接使用代码、类型与验证结果，不依赖会话记忆。

## WHY NOW

已有 D07 的必要实现后，可以独立完成此边界。与相邻任务按稳定接口衔接，迁移、API、相关页面和最小测试在本任务内一起完成。

## SIZE

L。M 约半个有效开发日，L 约半天至 1.5 天，仅作初估；模型运行时长和环境等待另计。单次聚焦一个主要机制，不按文件数量硬切分。

## RISK

HIGH — 状态/数据完整性风险，需要故障恢复证据。

## RECOMMENDED MODEL

- CAPABILITY LEVEL: LEVEL 4（任务难度标记，不是模型官方评级）
- PREFERRED MODEL: DeepSeek V4.1 Flash；官方 API 模型名 `deepseek-flash`。
- FALLBACK MODEL: 同一模型；先做有证据的定位与最小修复。无法解决则标明阻塞，不自动换付费模型。
- THINKING: `max`，启用思考。
- WHY: 涉及持久状态、恢复或事务边界，使用 max 并以故障测试验收；无需同时修改其他复杂子系统。
- MODEL RECHECK: 仅项目首次启动、提供方/模型变化、参数不支持时核查；连续任务不重复联网比榜单。当前官方别名对应 1017 版本，第三方平台需查实际映射。

## CAN DOWNGRADE MODEL

NO，本任务首次实现维持上方档位；先减少无关读取与重复验证，不以降低正确性要求节省费用。

## DEPENDS ON

D07

## BLOCKS

D09

只认真实代码与契约依赖；编号是默认执行顺序，不是额外前置。

## CONTEXT

Xray Stats 只是采集源。Agent 必须协调保存采集水位与账本增量，up/down 用整数 bytes 分别累计。ledger_epoch 只在本地账本真正重建时变化；Core 重启只改 generation，普通 Agent 重启两者不能随意重置。报告采用 D07 固定的 K 和 period 元组，保留当前段与未 ACK 旧段。三个周期边界取并集切分，离线继续记账。采样存在时间精度与物理崩溃损失窗口，必须用真实结果说明，不宣称绝对零漏计；暂用确定性接收器验证发送端，正式 Panel 入账属于 D09。

## REQUIRED READING

### Always read

- 本任务全文。

### Source files to inspect

- internal/core/xray/ stats、internal/agent/ apply/recover
- internal/period/、internal/protocol/ traffic
- internal/agent/ ledger（若存在）、已有持久存储封装

只检查相关入口及其直接调用/类型。路径为预期定位，实际命名不同就找等价实现；不存在不等于必须创建所有列出的目录。

### Additional docs

- [tasks/README.md](README.md)：依赖状态及连续执行规则。

不要求读全量 PROJECT_PLAN、历史会话、旧任务或其他交接文件。真实契约冲突才定位原计划对应小节。

## EXPECTED BASELINE

D04 有 Core 生命周期和 Stats 映射，D07 有周期函数和报告/ACK 类型。

## START REALITY CHECK

开始只做一次：核查依赖 DONE 及实际代码/API/Schema；读取相关入口，找出当前可执行验证命令；对照 baseline。命名/目录小差异直接适配，不能因此重新规划。若发现已经实现，验证后复用，不重写。必要环境、能力或契约缺失才标 blocker。

## CONTRACT

- 一次持久提交包含 source generation、采样水位和各 segment 累计；先持久后发送。计数回退必须区分 Core generation 变化与坏读数。
- 冻结的旧 segment 重发使用同一 K/归属；不能以新配置把旧用户/Node 的流量重新归属。
- 普通/final ACK 必须匹配 K/epoch、当前同步 authority/request，累计不超过本地且不回退；先持久确认水位再清理。retired ACK 只证明本 Agent 已持久分配的 close_seq 在对端 closed_through 以内，不是普通流量累计 ACK。
- 在周期边界主动采样；错过边界的一个增量默认按采样完成时可确定的最新已知周期归属，并保存整个含糊采样区间。只有测得限制条件才给误差上界；停机/休眠/时钟跳变使区间无界时明确无法保证界限，不伪造精确发生时间。
- 磁盘失败不推进未持久水位、不删除未 ACK 数据；离线只保留必要周期累计，不存每连接日志。

- Stats 默认用非清零累计读取；Core 实例身份必须能在 Agent 单独重启后识别，不能仅凭计数下降判定重启（新 Core 计数可能已超过旧水位）。无法证明身份连续时记录测量断档，不重扣整段历史。
- 切换配置/周期策略、删除统计标签前最终采样并原子冻结旧归属；分配 close_seq 与最终累计在同次本地持久提交中完成。已冻结段只重发，不能恢复增长。
- 按序补发 finalized 段，处理部分成功 ACK 和关闭边界；允许保留少量当前段。Panel 关闭前缀已持久且可重试确认后，删除对应本地 final 明细，绝不删除尚有缺口的记录。
- 恢复握手必须先于消费新 ACK/继续清理；Panel authority 改变时暂停清理并核对接收水位，不能把旧实例迟到 ACK 当恢复后入账证据。对未知归属记录保留待对账并退避，不无限紧循环。

- 本地清理事务同时保留最高关闭前缀及完整回执；恢复握手报告该前缀和仍持有的 final 范围，不能删掉最后一份恢复证明。前缀回退不把本地 close_seq 重置，已分配序号不复用。

上述是本轮固定的工程默认与边界，不冒充原会话已逐项指定实现；真实证据证明不可行时说明冲突，不静默改变产品行为。

## IN SCOPE

- 选择最小事务存储或原子状态方案，落实崩溃恢复；复用已有依赖而非自制数据库。
- 实现真实采样、周期分段、pending 累计报告与 ACK 消费，支持重试和关闭旧段。
- 提供有限账本诊断/断档事件、低盘告警和已 ACK 清理；与配置/Core 切换协调。

## OUT OF SCOPE

不写 Panel 入账 SQL、配额封禁和漂亮历史图，不在一项里改两端事务算法。

共同排除：商业系统、QuotaTemplate、Emergency Budget/Lease/Bypass、任意 Shell、Agent 插件运行时、多 Core/多跳、额外常驻依赖；不为未来功能做框架。

## IMPLEMENTATION TARGET

按 IN SCOPE 顺序形成最小可用闭环。先用现有代码/类型解决本项，不提出多套架构选项；只在当前任务产出上做必要重构。开发验证使用隔离数据；不会因任务包含安装/恢复就获得操作生产机器的授权。

## FILES EXPECTED

- internal/agent/ sampler/ledger/recovery
- 持久存储实现、相关 integration fixtures 与 fake receiver

只创建真正必要文件；代码、Schema、测试与正式使用说明就是交接，不新增工作日志。

## MINIMUM TESTS

- 持久提交前后终止：已持久流量恢复且不双计。
- Core 与 Agent 分别重启、相同报告重发和 ACK 落盘前终止。
- 离线跨两个边界后旧段保留；伪造/越界 ACK 不删账。

- Core 更换后计数高于旧水位仍识别新实例；配置删除前最后一段流量有确定归属。
- final 落盘/ACK/关闭前缀确认前后崩溃；乱序 ACK 与部分失败不误删，关闭前缀有洞时不能跳过。

优先复用已有测试；不为简单布局/getter/实现细节镜像写测试，不把编译或 mock 通过当真实代理/资源验证。

## VALIDATION

执行相关 targeted tests 与本任务的一次实际 smoke；只有改动共享契约或发现具体回归风险才扩大范围。根据真实工具链执行命令，回复给出命令/结果/未验证项。失败先判断来自环境、已有问题还是本次改动，不盲跑全套。

## ACCEPTANCE

- GOAL 形成真实可用结果，IN SCOPE 必需行为完成。
- CONTRACT 不变量成立，MINIMUM TESTS 和必要 smoke 有实际证据。
- 持久提交前后终止：已持久流量恢复且不双计。
- 必需环境未测、存在影响合同的失败时保持 PARTIAL/BLOCKED；提供脚本不等于已经运行。
- 对相关既有行为完成下述回归，不以全仓重写掩盖失败。

## REGRESSION

单 Core 应用/LKG、共享 Credential 统计身份、128MB 内存与有界日志保持。

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
