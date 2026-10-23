# D01 — 验证真实 Core、统计粒度与 128MB 原型

TASK ID: D01  
MILESTONE: M1  
STATUS: 只在 [任务索引](README.md) 维护，本文件为静态任务包。

## GOAL

得到可重现的真实 Xray 原型，证明同一用户凭证跨 Node 仍能分开统计，并验证最低配置的基础可行性。

## USER VALUE

本任务的可观察结果是：得到可重现的真实 Xray 原型，证明同一用户凭证跨 Node 仍能分开统计，并验证最低配置的基础可行性。 后续任务直接使用代码、类型与验证结果，不依赖会话记忆。

## WHY NOW

本任务无代码前置，可从当前项目开始。与相邻任务按稳定接口衔接，迁移、API、相关页面和最小测试在本任务内一起完成。

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

NONE

## BLOCKS

D04

只认真实代码与契约依赖；编号是默认执行顺序，不是额外前置。

## CONTEXT

产品是 Go Panel + SQLite + 内嵌轻量页面、Go Thin Agent、独立 Xray。节点一机一个 Core，承载多个 Node，最低整机 128MB RAM/1GB 总盘，正式协议先做 VLESS Reality。相同用户的同类型 UUID 跨授权 Node 复用，但计量必须能区分 User×入口 Node。统计身份不能简单等同 UUID。此任务只解决真实 Core 和资源未知，不要求一次决定整个项目架构。Panel 本机 Agent 以后使用相同流程；不采用每 Node 一个 Core、完整现成面板或额外常驻数据库。

## REQUIRED READING

### Always read

- 本任务全文。

### Source files to inspect

- go.mod、go.sum（存在才读）
- internal/core/xray/、bench/（存在才读）

只检查相关入口及其直接调用/类型。路径为预期定位，实际命名不同就找等价实现；不存在不等于必须创建所有列出的目录。

### Additional docs

- [tasks/README.md](README.md)：依赖状态及连续执行规则。

不要求读全量 PROJECT_PLAN、历史会话、旧任务或其他交接文件。真实契约冲突才定位原计划对应小节。

## EXPECTED BASELINE

项目目前仅有设计与任务文档；不要假设 Go、Linux、Xray 或测试 VPS 已安装。

## START REALITY CHECK

开始只做一次：核查依赖 DONE 及实际代码/API/Schema；读取相关入口，找出当前可执行验证命令；对照 baseline。命名/目录小差异直接适配，不能因此重新规划。若发现已经实现，验证后复用，不重写。必要环境、能力或契约缺失才标 blocker。

## CONTRACT

- 固定一个经实际官方文档和运行验证的 Xray 版本；保存版本及校验来源，不猜测 API。
- 一机一个 Core；相同 UUID 的两入口要以实际 Stats 结果证明可分离。不能分离就记录技术阻塞，不偷偷改成每机/每 Node 独立用户凭证。
- 128MB 必须含 OS；cgroup 和 256MB 只作辅助证据，不能替代。

- 原型同时验证非破坏性的累计 Stats 读取（不以每次读取清零作为持久账本前提）、Core 实例标识与计数器变化。由 D08 复用确定的读取语义，不能在写账本时才发现采样会清零。

上述是本轮固定的工程默认与边界，不冒充原会话已逐项指定实现；真实证据证明不可行时说明冲突，不静默改变产品行为。

## IN SCOPE

- 建立最小 Go 模块和 Xray 适配原型，不建空业务框架。
- 完成 config test、真实代理、Stats 读取及两个入口身份映射；保留最小配置 fixture。
- 测启动、空闲、低并发、配置检查峰值；将可重跑命令、环境和真实结果放 bench/。

## OUT OF SCOPE

不做 Panel CRUD、安装器、账本或全部工程契约讨论。

共同排除：商业系统、QuotaTemplate、Emergency Budget/Lease/Bypass、任意 Shell、Agent 插件运行时、多 Core/多跳、额外常驻依赖；不为未来功能做框架。

## IMPLEMENTATION TARGET

按 IN SCOPE 顺序形成最小可用闭环。先用现有代码/类型解决本项，不提出多套架构选项；只在当前任务产出上做必要重构。开发验证使用隔离数据；不会因任务包含安装/恢复就获得操作生产机器的授权。

## FILES EXPECTED

- go.mod/go.sum、internal/core/xray/ 最小适配
- bench/ 运行入口和真实结果、tests/fixtures/ 配置

只创建真正必要文件；代码、Schema、测试与正式使用说明就是交接，不新增工作日志。

## MINIMUM TESTS

- 同 UUID 两入口分别产生流量，统计可归属且一个 Core。
- 128MB 整机或等价 VM 原型无 OOM；不可用时精确标记环境阻塞。

- 连续读取同一计数器不清零；明确一次 Core 重启前后实例与计数表现。

优先复用已有测试；不为简单布局/getter/实现细节镜像写测试，不把编译或 mock 通过当真实代理/资源验证。

## VALIDATION

执行相关 targeted tests 与本任务的一次实际 smoke；只有改动共享契约或发现具体回归风险才扩大范围。根据真实工具链执行命令，回复给出命令/结果/未验证项。失败先判断来自环境、已有问题还是本次改动，不盲跑全套。

## ACCEPTANCE

- GOAL 形成真实可用结果，IN SCOPE 必需行为完成。
- CONTRACT 不变量成立，MINIMUM TESTS 和必要 smoke 有实际证据。
- 同 UUID 两入口分别产生流量，统计可归属且一个 Core。
- 必需环境未测、存在影响合同的失败时保持 PARTIAL/BLOCKED；提供脚本不等于已经运行。
- 对相关既有行为完成下述回归，不以全仓重写掩盖失败。

## REGRESSION

保持最低配置、共享凭证与单 Core 约束；不得为了原型成功降低 V1 要求。

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
