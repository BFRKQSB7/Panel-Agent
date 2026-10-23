# D09 — Panel 幂等入账与双端补报贯通

TASK ID: D09  
MILESTONE: M3  
STATUS: 只在 [任务索引](README.md) 维护，本文件为静态任务包。

## GOAL

将 Agent 累计报告接入真实 SQLite 事务，完成重复、乱序和 ACK 丢失下的一次入账。

## USER VALUE

本任务的可观察结果是：将 Agent 累计报告接入真实 SQLite 事务，完成重复、乱序和 ACK 丢失下的一次入账。 后续任务直接使用代码、类型与验证结果，不依赖会话记忆。

## WHY NOW

已有 D08 的必要实现后，可以独立完成此边界。与相邻任务按稳定接口衔接，迁移、API、相关页面和最小测试在本任务内一起完成。

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

D08

## BLOCKS

D10, D16

只认真实代码与契约依赖；编号是默认执行顺序，不是额外前置。

## CONTEXT

Agent 已保存周期累计；Panel 用每个 K 的 accepted_up/down 水位计算新增长量，同时汇总 User、User×Node、Machine。所有水位与汇总更新必须同一事务，成功提交后才 ACK。上报时刻不决定消费周期，周期来自经过验证的报告归属。Panel 可以删除原始报告，不能同时删除去重水位；已删除对象通过墓碑保留迟到归属。只凭周期累计无法重建离线小时曲线，历史应标注补报。此任务完成两端计量闭环，配额控制和可视化由 D10 消费现成数据。

## REQUIRED READING

### Always read

- 本任务全文。

### Source files to inspect

- internal/protocol/ traffic、internal/agent/ report/ACK
- internal/storage/ migrations、internal/control/ agent routes
- internal/period/ 与 scope/tombstone 模型

只检查相关入口及其直接调用/类型。路径为预期定位，实际命名不同就找等价实现；不存在不等于必须创建所有列出的目录。

### Additional docs

- [tasks/README.md](README.md)：依赖状态及连续执行规则。

不要求读全量 PROJECT_PLAN、历史会话、旧任务或其他交接文件。真实契约冲突才定位原计划对应小节。

## EXPECTED BASELINE

D08 已有真实持久采样和报告队列，D07 类型固定；D03 有 Machine 认证。

## START REALITY CHECK

开始只做一次：核查依赖 DONE 及实际代码/API/Schema；读取相关入口，找出当前可执行验证命令；对照 baseline。命名/目录小差异直接适配，不能因此重新规划。若发现已经实现，验证后复用，不重写。必要环境、能力或契约缺失才标 blocker。

## CONTRACT

- K 水位 100/40 收 150/50：只入 50/10；重发 150/50 入 0/0；迟到 120/45 不回退也不再扣。某一分量进而另一分量退视为非法混合累计并拒绝。
- 水位推进、三个 scope 累计与必要汇总原子提交；失败无成功 ACK，同一 K 并发串行处理。
- user_entry 扣用户/入口节点/机器；machine_only_transit 只扣机器。Machine 认证、stream 归属和历史策略必须验证。
- 新 ledger_epoch 需经认证登记并记录断档；旧 epoch 只可处理已知归属的迟到/重放，不自动取代当前身份。
- 必须实现 D07 的 finalized/close_seq/closed_through 关闭协议：final 累计入账与连续关闭前缀推进同一事务；只有前缀内的已冻结段才能压缩明细。低于前缀的重放返回 retired，不再次入账。不能以永久保留每周期水位作为 V1 有界存储验收。

- 校验 K 的原始 scope/period/policy 绑定，相同 K 改用户、节点或周期必须拒绝；总量及汇总有 checked integer arithmetic。已删除历史对象可只作计量墓碑，不能恢复为有效授权对象。
- 每条报告独立事务/结果，批次允许部分成功；一个未知 policy/损坏记录不阻止其他可验证记录入账。旧备份缺少归属元数据时明确 quarantine，不 ACK 丢弃、不按当前月重映射。
- 关闭前缀只前进不回退，final 重试不可改变最终累计。当前/未确认明细、关闭前缀和未解断档不能按日志保留期删；不活跃 stream 的墓碑仅在相关 final 全关闭且统计源退役后回收。

- 关闭凭据仅在对应事务提交后签发，不能为未提交数据签发更大前缀。恢复时只有经验证的旧凭据及 D16 对账流程可跨越已丢失的旧确认范围；普通新报告携带大 close_seq 不能跳过缺口。

上述是本轮固定的工程默认与边界，不冒充原会话已逐项指定实现；真实证据证明不可行时说明冲突，不静默改变产品行为。

## IN SCOPE

- 实现流量表/水位/用量/墓碑最小迁移和带大小限制的 traffic API。
- 完成事务去重与真实 ACK，接通 Agent，处理乱序/丢包/重复/数据库失败。
- 加入小时/日/月有限 rollup 与历史保留配置，补报标记；不保留无限 raw reports。

## OUT OF SCOPE

不重写 Agent 采样方案、客户端订阅或做配额 UI。

共同排除：商业系统、QuotaTemplate、Emergency Budget/Lease/Bypass、任意 Shell、Agent 插件运行时、多 Core/多跳、额外常驻依赖；不为未来功能做框架。

## IMPLEMENTATION TARGET

按 IN SCOPE 顺序形成最小可用闭环。先用现有代码/类型解决本项，不提出多套架构选项；只在当前任务产出上做必要重构。开发验证使用隔离数据；不会因任务包含安装/恢复就获得操作生产机器的授权。

## FILES EXPECTED

- internal/control/ traffic ingest
- internal/storage/ watermark/usage/rollup 迁移
- 双端补报 integration cases

只创建真正必要文件；代码、Schema、测试与正式使用说明就是交接，不新增工作日志。

## MINIMUM TESTS

- 按合同数值测试重复/乱序/混合回退，提交后 ACK 丢失重发只记一次。
- 事务失败与同 K 并发，水位和汇总一致。
- 跨独立周期补报、对象删除迟到、未经授权 stream/new epoch 被拒。

- 同 K 改 scope/policy、超大数字/溢出拒绝；合法与未知归属混合批次仅确认已提交记录。
- 连续多周期关闭压缩后重放仍零新增；事务提交/前缀确认丢失仍可恢复，确认前缀有洞不前移。

优先复用已有测试；不为简单布局/getter/实现细节镜像写测试，不把编译或 mock 通过当真实代理/资源验证。

## VALIDATION

执行相关 targeted tests 与本任务的一次实际 smoke；只有改动共享契约或发现具体回归风险才扩大范围。根据真实工具链执行命令，回复给出命令/结果/未验证项。失败先判断来自环境、已有问题还是本次改动，不盲跑全套。

## ACCEPTANCE

- GOAL 形成真实可用结果，IN SCOPE 必需行为完成。
- CONTRACT 不变量成立，MINIMUM TESTS 和必要 smoke 有实际证据。
- 按合同数值测试重复/乱序/混合回退，提交后 ACK 丢失重发只记一次。
- 必需环境未测、存在影响合同的失败时保持 PARTIAL/BLOCKED；提供脚本不等于已经运行。
- 对相关既有行为完成下述回归，不以全仓重写掩盖失败。

## REGRESSION

D08 未 ACK 数据完整；三周期身份不能被统一成收到报告的当前月。

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
