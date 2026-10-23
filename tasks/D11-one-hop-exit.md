# D11 — 隐藏 Exit、一跳转发与扣账归属

TASK ID: D11  
MILESTONE: M4  
STATUS: 只在 [任务索引](README.md) 维护，本文件为静态任务包。

## GOAL

完成两机一跳代理，出口故障确实断流且用户只在入口扣一次。

## USER VALUE

本任务的可观察结果是：完成两机一跳代理，出口故障确实断流且用户只在入口扣一次。 后续任务直接使用代码、类型与验证结果，不依赖会话记忆。

## WHY NOW

已有 D10 的必要实现后，可以独立完成此边界。与相邻任务按稳定接口衔接，迁移、API、相关页面和最小测试在本任务内一起完成。

## SIZE

L。M 约半个有效开发日，L 约半天至 1.5 天，仅作初估；模型运行时长和环境等待另计。单次聚焦一个主要机制，不按文件数量硬切分。

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

D10

## BLOCKS

D17

只认真实代码与契约依赖；编号是默认执行顺序，不是额外前置。

## CONTEXT

Node 是用户入口，Exit 是隐藏的机器间入口，不可复用用户 Node/UUID。每台机器 0..N Node、0..1 默认 Exit，共用一个 Core。Node 只能选 Direct 或另一 Machine 的 Exit；Exit 永远直出，不做任意多跳。Transit 凭证由 Panel 独立生成并仅给相关机器。用户和入口 Node 记一次，Entry/Exit Machine 分别记各自实际代理量，不要求两端因开销差异仍完全相等。NAT Exit 必须手填可达公网映射，Agent 控制通道连通不代表 Exit 入站可用。

## REQUIRED READING

### Always read

- 本任务全文。

### Source files to inspect

- internal/control/ nodes/machines/credentials/usage
- internal/core/xray/ 渲染与统计、internal/agent/ apply
- internal/protocol/ snapshots、节点/机器页面

只检查相关入口及其直接调用/类型。路径为预期定位，实际命名不同就找等价实现；不存在不等于必须创建所有列出的目录。

### Additional docs

- [tasks/README.md](README.md)：依赖状态及连续执行规则。

不要求读全量 PROJECT_PLAN、历史会话、旧任务或其他交接文件。真实契约冲突才定位原计划对应小节。

## EXPECTED BASELINE

D10 有完整配额限制，D09 支持 machine_only_transit，D04 可合并多入口。

## START REALITY CHECK

开始只做一次：核查依赖 DONE 及实际代码/API/Schema；读取相关入口，找出当前可执行验证命令；对照 baseline。命名/目录小差异直接适配，不能因此重新规划。若发现已经实现，验证后复用，不重写。必要环境、能力或契约缺失才标 blocker。

## CONTRACT

- 拒自身 Exit、再跳和不支持组合；Exit 及 Transit secrets 不出现在用户订阅。
- 先使出口配置已应用/可用，再启用入口路由；移除/轮换时按依赖协调。
- Exit 不可达/额度硬停用时 Fail Closed，禁止 Direct fallback。
- 每个统计来源只选一处计算机器量，不能 inbound/outbound 再加一遍。

- 在配置校验/更新/回滚时维持出口策略：已设 One-Hop 的 Node 在 Exit 未就绪/被删除/轮换失败时保持禁用或阻断，不能被整机默认 Direct 出站接管。删除 Exit 必须处理引用。
- 分别检查 TCP 与 UDP；Core/协议不支持的传输要明确禁用/拒绝，不可悄悄直出。出口预算包含 Transit 的在途连接限制，管理控制流量不计为用户代理。

上述是本轮固定的工程默认与边界，不冒充原会话已逐项指定实现；真实证据证明不可行时说明冲突，不静默改变产品行为。

## IN SCOPE

- 固定一种经真实 Xray 验证的 Transit 协议和独立端口/凭证结构。
- 贯通 Exit 管理、Node 出口选择、两端快照与就绪状态。
- 接 machine_only_transit 流量、预算限制和故障反馈，验证 Panel 本机也可作为普通 Exit。

## OUT OF SCOPE

不做通用路由、测速、多跳、外部机自动探测映射。

共同排除：商业系统、QuotaTemplate、Emergency Budget/Lease/Bypass、任意 Shell、Agent 插件运行时、多 Core/多跳、额外常驻依赖；不为未来功能做框架。

## IMPLEMENTATION TARGET

按 IN SCOPE 顺序形成最小可用闭环。先用现有代码/类型解决本项，不提出多套架构选项；只在当前任务产出上做必要重构。开发验证使用隔离数据；不会因任务包含安装/恢复就获得操作生产机器的授权。

## FILES EXPECTED

- internal/control/ exits/transit、相关迁移和 UI
- Xray 整机路由/统计映射、链路状态

只创建真正必要文件；代码、Schema、测试与正式使用说明就是交接，不新增工作日志。

## MINIMUM TESTS

- 真实两机一跳：用户/入口 Node 一次，两个机器分别计量。
- 断出口/停用出口预算后无任何直出；恢复仍走原出口。
- 秘密和实验组隔离、拒自身或多跳组合。

- Exit 删除/轮换中故障、候选回滚以及 UDP/域名目标的实际转发；检测的是业务出口而非仅 UI 状态。

优先复用已有测试；不为简单布局/getter/实现细节镜像写测试，不把编译或 mock 通过当真实代理/资源验证。

## VALIDATION

执行相关 targeted tests 与本任务的一次实际 smoke；只有改动共享契约或发现具体回归风险才扩大范围。根据真实工具链执行命令，回复给出命令/结果/未验证项。失败先判断来自环境、已有问题还是本次改动，不盲跑全套。

## ACCEPTANCE

- GOAL 形成真实可用结果，IN SCOPE 必需行为完成。
- CONTRACT 不变量成立，MINIMUM TESTS 和必要 smoke 有实际证据。
- 真实两机一跳：用户/入口 Node 一次，两个机器分别计量。
- 必需环境未测、存在影响合同的失败时保持 PARTIAL/BLOCKED；提供脚本不等于已经运行。
- 对相关既有行为完成下述回归，不以全仓重写掩盖失败。

## REGRESSION

Direct、订阅和三维补账保持，整机仍一个 Core。

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
