# D05 — 用户、组与实际代理权限

TASK ID: D05  
MILESTONE: M2  
STATUS: 只在 [任务索引](README.md) 维护，本文件为静态任务包。

## GOAL

完成用户/组管理，使组关系变化同时决定页面可见性和 Core 中实际可用的连接凭证。

## USER VALUE

本任务的可观察结果是：完成用户/组管理，使组关系变化同时决定页面可见性和 Core 中实际可用的连接凭证。 后续任务直接使用代码、类型与验证结果，不依赖会话记忆。

## WHY NOW

已有 D04 的必要实现后，可以独立完成此边界。与相邻任务按稳定接口衔接，迁移、API、相关页面和最小测试在本任务内一起完成。

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

D04

## BLOCKS

D06, D07

只认真实代码与契约依赖；编号是默认执行顺序，不是额外前置。

## CONTEXT

User 只属于一个 UserGroup；UserGroup 关联多个 NodeGroup；NodeGroup 包含多个 Node。实验节点靠组隔离，不添加发布状态机和逐用户节点权限。用户由管理员手动/批量创建，可设置到期、启停、额度字段；额度先保存，D10 才执行。相同用户同类型协议 Credential 跨授权节点复用，Panel 以 master key 可恢复加密；连接秘密与页面/订阅 Token 独立。创建节点并加入授权组后，相关用户无需逐人修改。删除或禁用不得抹去未来迟到流量的归属。

## REQUIRED READING

### Always read

- 本任务全文。

### Source files to inspect

- internal/control/ users/groups/nodes（按实际入口）
- internal/storage/ migrations、internal/security/
- internal/core/xray/ 用户映射、internal/web/ 用户与组页面

只检查相关入口及其直接调用/类型。路径为预期定位，实际命名不同就找等价实现；不存在不等于必须创建所有列出的目录。

### Additional docs

- [tasks/README.md](README.md)：依赖状态及连续执行规则。

不要求读全量 PROJECT_PLAN、历史会话、旧任务或其他交接文件。真实契约冲突才定位原计划对应小节。

## EXPECTED BASELINE

D04 已有 Node、Core 应用和开发凭证；D02 有 master key 与管理认证。

## START REALITY CHECK

开始只做一次：核查依赖 DONE 及实际代码/API/Schema；读取相关入口，找出当前可执行验证命令；对照 baseline。命名/目录小差异直接适配，不能因此重新规划。若发现已经实现，验证后复用，不重写。必要环境、能力或契约缺失才标 blocker。

## CONTRACT

- 权限计算只用 User→单 UserGroup→NodeGroup→Node，并去重；写入真实 Agent 快照，不仅隐藏 UI。
- 用户 Credential 轮换独立操作；无双凭证过渡期，Agent 应用后生效。
- 额度区分不限与零，不预建全部 User×Node 覆盖行；删除使用必要墓碑/稳定身份。

- User/Node/Machine/stream 使用不可复用身份；删除后重建同名对象也不能复用历史 ID。组/凭证修改不能重写既有统计归属；移除前交给应用/采样边界处理。

上述是本轮固定的工程默认与边界，不冒充原会话已逐项指定实现；真实证据证明不可行时说明冲突，不静默改变产品行为。

## IN SCOPE

- 实现 User/UserGroup/NodeGroup/关系/Credential 的最小迁移、API 与页面。
- 贯通批量创建/切组/到期/启停/额度字段/凭证轮换，并显示部分失败。
- 以正式授权凭证替换开发凭证路径；失联状态不撤销 Node 定义。

## OUT OF SCOPE

不做用户登录体系、商业功能、QuotaTemplate、实际流量和订阅 Renderer。

共同排除：商业系统、QuotaTemplate、Emergency Budget/Lease/Bypass、任意 Shell、Agent 插件运行时、多 Core/多跳、额外常驻依赖；不为未来功能做框架。

## IMPLEMENTATION TARGET

按 IN SCOPE 顺序形成最小可用闭环。先用现有代码/类型解决本项，不提出多套架构选项；只在当前任务产出上做必要重构。开发验证使用隔离数据；不会因任务包含安装/恢复就获得操作生产机器的授权。

## FILES EXPECTED

- internal/control/ users/groups/authorization
- 相关迁移、Credential 加密与 UI、Core 用户列表

只创建真正必要文件；代码、Schema、测试与正式使用说明就是交接，不新增工作日志。

## MINIMUM TESTS

- 普通与测试组在列表、快照、实际代理三处权限一致。
- Node 加组自动获得、重复关系去重、Credential 轮换生效。
- 批量部分失败可见，master key 缺失不静默重置。

优先复用已有测试；不为简单布局/getter/实现细节镜像写测试，不把编译或 mock 通过当真实代理/资源验证。

## VALIDATION

执行相关 targeted tests 与本任务的一次实际 smoke；只有改动共享契约或发现具体回归风险才扩大范围。根据真实工具链执行命令，回复给出命令/结果/未验证项。失败先判断来自环境、已有问题还是本次改动，不盲跑全套。

## ACCEPTANCE

- GOAL 形成真实可用结果，IN SCOPE 必需行为完成。
- CONTRACT 不变量成立，MINIMUM TESTS 和必要 smoke 有实际证据。
- 普通与测试组在列表、快照、实际代理三处权限一致。
- 必需环境未测、存在影响合同的失败时保持 PARTIAL/BLOCKED；提供脚本不等于已经运行。
- 对相关既有行为完成下述回归，不以全仓重写掩盖失败。

## REGRESSION

Node 手填 NAT、单 Core 及坏配置恢复保持；已有禁用原因不互相覆盖。

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
