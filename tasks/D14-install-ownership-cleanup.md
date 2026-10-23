# D14 — 安装、文件所有权与两级卸载

TASK ID: D14  
MILESTONE: M5  
STATUS: 只在 [任务索引](README.md) 维护，本文件为静态任务包。

## GOAL

在支持 Linux 平台完整安装各角色，并通过明确所有权安全保留或删除项目文件。

## USER VALUE

本任务的可观察结果是：在支持 Linux 平台完整安装各角色，并通过明确所有权安全保留或删除项目文件。 后续任务直接使用代码、类型与验证结果，不依赖会话记忆。

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

D16

只认真实代码与契约依赖；编号是默认执行顺序，不是额外前置。

## CONTEXT

部署支持 control-only、agent-only、Panel+Agent 共机，默认单二进制+systemd；Debian 12+/Ubuntu 22.04+、amd64/arm64。安装器只检查平台/资源/冲突、下载校验、建项目目录与服务、注册启动。不自动改 DNS、防火墙、sysctl、BBR，也不装 nginx。Xray 默认由项目安装，复用外部二进制时标 external 并使用独立配置/服务，不接管原实例。普通卸载保留数据，显式 --purge 才删该角色自身数据；这是后续升级与清理的路径所有权基础。

## REQUIRED READING

### Always read

- 本任务全文。

### Source files to inspect

- cmd/control/、cmd/agent/ 配置/启动
- scripts/、packaging/（若存在）
- internal/agent/ 路径/日志/临时文件与运行身份

只检查相关入口及其直接调用/类型。路径为预期定位，实际命名不同就找等价实现；不存在不等于必须创建所有列出的目录。

### Additional docs

- [tasks/README.md](README.md)：依赖状态及连续执行规则。

不要求读全量 PROJECT_PLAN、历史会话、旧任务或其他交接文件。真实契约冲突才定位原计划对应小节。

## EXPECTED BASELINE

D04 两角色可启动、配对及代理；尚无可假定的安装器。账本/扩展若已实现要按角色通用保留。

## START REALITY CHECK

开始只做一次：核查依赖 DONE 及实际代码/API/Schema；读取相关入口，找出当前可执行验证命令；对照 baseline。命名/目录小差异直接适配，不能因此重新规划。若发现已经实现，验证后复用，不重写。必要环境、能力或契约缺失才标 blocker。

## CONTRACT

- 所有权清单记录规范化路径、角色、来源、可 purge。递归操作仅限项目私有数据根；systemd unit 与 CLI 入口可位于标准系统目录，但只按显式允许的精确文件路径操作并核对项目所有权，绝不递归删除系统父目录，不覆盖已有同名外部文件。
- 普通卸载保留数据库/密钥/配置/账本，purge 不碰外部 Core/配置/服务；共机按角色处理。
- 低盘清理只删确定可再生残留；Current/LKG、密钥、数据库和未 ACK 数据永不自动删。
- 日志轮转含 Core 输出，服务级有限策略，不修改主机全局 journal 保留。

- 安装/更新/清理串行，目标父目录不允许非受信用户写；执行时使用不跟随链接的路径/句柄操作，不能只做一次字符串前缀校验后放任路径被替换。项目创建的入口链接卸载时只 unlink 本链接，不删除目标。
- 首次目录/数据库/密钥创建采用受限权限和原子落盘；拒绝错误属主/既有外部服务冲突，下载校验通过前不执行候选。

上述是本轮固定的工程默认与边界，不冒充原会话已逐项指定实现；真实证据证明不可行时说明冲突，不静默改变产品行为。

## IN SCOPE

- 实现构建/校验、安装命令、systemd、角色选择和已有 Core 模式。
- 实现所有权清单、普通卸载/purge、有限日志/临时清理/调试自动到期。
- 提供真实安装与卸载说明，重试不重复注册或覆盖用户文件。

## OUT OF SCOPE

不做升级 Intent、备份恢复、全平台最终验收或远程部署用户生产机。

共同排除：商业系统、QuotaTemplate、Emergency Budget/Lease/Bypass、任意 Shell、Agent 插件运行时、多 Core/多跳、额外常驻依赖；不为未来功能做框架。

## IMPLEMENTATION TARGET

按 IN SCOPE 顺序形成最小可用闭环。先用现有代码/类型解决本项，不提出多套架构选项；只在当前任务产出上做必要重构。开发验证使用隔离数据；不会因任务包含安装/恢复就获得操作生产机器的授权。

## FILES EXPECTED

- scripts/、packaging/systemd/、构建入口
- 共享 ownership/path 模块、CLI 安装卸载
- 项目 README 的必要部署说明

只创建真正必要文件；代码、Schema、测试与正式使用说明就是交接，不新增工作日志。

## MINIMUM TESTS

- 隔离 VM 安装→重跑→普通卸载→purge，保留/删除与清单一致。
- 外部 Core、越界路径、符号链接、共机角色代表保护用例。
- 日志压力清理不删必要数据。

- 项目专用 systemd unit 可安装/移除，同目录其他 unit 不受影响；校验后路径替换和入口链接指向外部文件均不误删目标。

优先复用已有测试；不为简单布局/getter/实现细节镜像写测试，不把编译或 mock 通过当真实代理/资源验证。

## VALIDATION

执行相关 targeted tests 与本任务的一次实际 smoke；只有改动共享契约或发现具体回归风险才扩大范围。根据真实工具链执行命令，回复给出命令/结果/未验证项。失败先判断来自环境、已有问题还是本次改动，不盲跑全套。

## ACCEPTANCE

- GOAL 形成真实可用结果，IN SCOPE 必需行为完成。
- CONTRACT 不变量成立，MINIMUM TESTS 和必要 smoke 有实际证据。
- 隔离 VM 安装→重跑→普通卸载→purge，保留/删除与清单一致。
- 必需环境未测、存在影响合同的失败时保持 PARTIAL/BLOCKED；提供脚本不等于已经运行。
- 对相关既有行为完成下述回归，不以全仓重写掩盖失败。

## REGRESSION

D04 代理/配对不变，后续账本数据必须按角色保留；禁止误删系统资源。

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
