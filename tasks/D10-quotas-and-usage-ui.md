# D10 — 三维配额、限制状态与真实用量页面

TASK ID: D10  
MILESTONE: M3  
STATUS: 只在 [任务索引](README.md) 维护，本文件为静态任务包。

## GOAL

将可靠用量用于正确范围的配额限制，并让管理员和用户看懂余额、重置及受限原因。

## USER VALUE

本任务的可观察结果是：将可靠用量用于正确范围的配额限制，并让管理员和用户看懂余额、重置及受限原因。 后续任务直接使用代码、类型与验证结果，不依赖会话记忆。

## WHY NOW

已有 D06、D09 的必要实现后，可以独立完成此边界。与相邻任务按稳定接口衔接，迁移、API、相关页面和最小测试在本任务内一起完成。

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

D06, D09

## BLOCKS

D11

只认真实代码与契约依赖；编号是默认执行顺序，不是额外前置。

## CONTEXT

User 全局额度影响其全部节点，User×Node 额度只影响该入口，Machine Budget 覆盖整机代理/Transit，可选阈值提醒或硬停用。节点额度优先级为用户专属覆盖、Node 默认每用户额度、不限；追加节点额度不动全局。周期与累计沿用 D07/D09，额度可最终一致超用，不发明租约系统。离线沿用 LKG，保留已禁用状态并执行已知到期；新离线消耗只记账，不根据未知全局余量新增封禁，恢复补报后收敛。这是本计划采用的工程默认解释。

## REQUIRED READING

### Always read

- 本任务全文。

### Source files to inspect

- internal/control/ quota/usage、用户/节点/机器 API
- internal/protocol/ Desired State、internal/agent/ apply
- internal/web/ dashboard/users/machines、internal/period/

只检查相关入口及其直接调用/类型。路径为预期定位，实际命名不同就找等价实现；不存在不等于必须创建所有列出的目录。

### Additional docs

- [tasks/README.md](README.md)：依赖状态及连续执行规则。

不要求读全量 PROJECT_PLAN、历史会话、旧任务或其他交接文件。真实契约冲突才定位原计划对应小节。

## EXPECTED BASELINE

D06 有用户页和订阅，D09 已有真实三维用量与可补报账本。

## START REALITY CHECK

开始只做一次：核查依赖 DONE 及实际代码/API/Schema；读取相关入口，找出当前可执行验证命令；对照 baseline。命名/目录小差异直接适配，不能因此重新规划。若发现已经实现，验证后复用，不重写。必要环境、能力或契约缺失才标 blocker。

## CONTRACT

- 不限和零额度语义分开；到期、管理员禁用、权限撤销、额度耗尽、机器停用分别存原因，解除一个不抹其他原因。
- 在线限制按实际 scope 下发，机器预算不等于厂商网卡账单；告知采样/传播超用窗口。
- 离线不自动解禁旧限制，无 Emergency Budget/Lease/Bypass；已知到期独立执行。
- 重置新建 period，修改额度/加额不抹历史；订阅和实际 Core 授权最终保持一致。

- 恢复补报采用每机可持久确认的追平栅栏：握手先冻结已欠报告范围，所有可计入范围提交后再解除旧额度限制；期间到期/管理员撤销仍可收紧。无法归属的欠账需标为待对账，相关作用域不能直接宣布余额准确或静默解除已有额度限制；已验证的其他作用域可独立收敛，不新增全机离线应急封禁。
- 新限制必须终止适用范围的已建立 TCP/UDP 会话，而不只是隐藏订阅/移除新连接凭证；允许适配器通过一次受控 Core 重启落实并显示实际影响。
- 离线时政策改动只对已收到的规则生效；恢复后保留旧 policy 用量、显示同步延迟，新周期不吞掉补报旧账。订阅恢复可用的时点以已实施状态和追平结果决定。

上述是本轮固定的工程默认与边界，不冒充原会话已逐项指定实现；真实证据证明不可行时说明冲突，不静默改变产品行为。

## IN SCOPE

- 贯通三维策略/覆盖/加额/重置 API 与表单。
- 计算限制状态，生成快照，Agent 实施并报告原因/时间。
- 替换用户页未启用提示，展示真实用量、余额、下一重置、同步时间；概览突出异常和预算提醒。

## OUT OF SCOPE

不新增应急额度、限速/设备限制、商业套餐或精确跨机零超用承诺。

共同排除：商业系统、QuotaTemplate、Emergency Budget/Lease/Bypass、任意 Shell、Agent 插件运行时、多 Core/多跳、额外常驻依赖；不为未来功能做框架。

## IMPLEMENTATION TARGET

按 IN SCOPE 顺序形成最小可用闭环。先用现有代码/类型解决本项，不提出多套架构选项；只在当前任务产出上做必要重构。开发验证使用隔离数据；不会因任务包含安装/恢复就获得操作生产机器的授权。

## FILES EXPECTED

- internal/control/ quota enforcement、快照字段
- internal/agent/ restrictions、管理与用户用量页面

只创建真正必要文件；代码、Schema、测试与正式使用说明就是交接，不新增工作日志。

## MINIMUM TESTS

- 全局耗尽、节点耗尽、机器软提醒/硬停用各代表一例。
- 节点加额不改全局；解除额度不绕过到期/禁用。
- 断网跨周期继续记账，恢复补报后限制收敛；普通测试组无隐式豁免。

- 长连接跨越额度/到期边界后确实断开；取消一个限制原因不能绕过其他原因。
- 补报未完成时不先解除额度禁用；未知历史策略给出待对账而非假余额；正常追平后恢复。

优先复用已有测试；不为简单布局/getter/实现细节镜像写测试，不把编译或 mock 通过当真实代理/资源验证。

## VALIDATION

执行相关 targeted tests 与本任务的一次实际 smoke；只有改动共享契约或发现具体回归风险才扩大范围。根据真实工具链执行命令，回复给出命令/结果/未验证项。失败先判断来自环境、已有问题还是本次改动，不盲跑全套。

## ACCEPTANCE

- GOAL 形成真实可用结果，IN SCOPE 必需行为完成。
- CONTRACT 不变量成立，MINIMUM TESTS 和必要 smoke 有实际证据。
- 全局耗尽、节点耗尽、机器软提醒/硬停用各代表一例。
- 必需环境未测、存在影响合同的失败时保持 PARTIAL/BLOCKED；提供脚本不等于已经运行。
- 对相关既有行为完成下述回归，不以全仓重写掩盖失败。

## REGRESSION

D09 去重/周期保持，D06 双 Token 和三输出不泄露或虚构数据。

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
