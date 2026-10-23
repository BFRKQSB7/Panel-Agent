# D13 — 受限协议扩展的完整生命周期

TASK ID: D13  
MILESTONE: M4  
STATUS: 只在 [任务索引](README.md) 维护，本文件为静态任务包。

## GOAL

让一个真实声明式扩展从导入、验证、试用到启停/删除完整可用，且不执行任意代码。

## USER VALUE

本任务的可观察结果是：让一个真实声明式扩展从导入、验证、试用到启停/删除完整可用，且不执行任意代码。 后续任务直接使用代码、类型与验证结果，不依赖会话记忆。

## WHY NOW

已有 D06 的必要实现后，可以独立完成此边界。与相邻任务按稳定接口衔接，迁移、API、相关页面和最小测试在本任务内一起完成。

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

D06

## BLOCKS

D16

只认真实代码与契约依赖；编号是默认执行顺序，不是额外前置。

## CONTEXT

V1 只实现 Xray，Reality 为正式内置协议，其他协议通过受限声明式包扩展。包由外部 AI 生成，Panel 不内置模型 API。包描述 manifest、UI Schema、固定字段映射、订阅 Renderer、Core 版本范围、secret 类型和 Direct/One-Hop/URI/Mihomo/sing-box/证书能力。Panel 验证和编译后下发标准化数据，Agent 仍使用固定逻辑，不加载插件运行时。无订阅输出的协议只能供测试组实验；扩展停用保留 Node 定义、已有引用不得被删除级联清空。

## REQUIRED READING

### Always read

- 本任务全文。

### Source files to inspect

- internal/control/ nodes/subscription、internal/core/xray/ 白名单边界
- internal/security/ secret、internal/storage/ migrations
- internal/web/ 系统设置/高级表单、internal/extensions/（若存在）

只检查相关入口及其直接调用/类型。路径为预期定位，实际命名不同就找等价实现；不存在不等于必须创建所有列出的目录。

### Additional docs

- [tasks/README.md](README.md)：依赖状态及连续执行规则。

不要求读全量 PROJECT_PLAN、历史会话、旧任务或其他交接文件。真实契约冲突才定位原计划对应小节。

## EXPECTED BASELINE

D06 有权限与内置 Reality 三输出；D11/D12 可能未完成，能力启用必须检查真实运行支持。

## START REALITY CHECK

开始只做一次：核查依赖 DONE 及实际代码/API/Schema；读取相关入口，找出当前可执行验证命令；对照 baseline。命名/目录小差异直接适配，不能因此重新规划。若发现已经实现，验证后复用，不重写。必要环境、能力或契约缺失才标 blocker。

## CONTRACT

- 选固定字段映射和有限占位符替换，拒脚本/动态库/二进制/任意表达式/模板函数/网络或文件访问。
- 扩展不能覆盖 API/Stats/日志路径等系统保留段；包大小、深度、路径和未知字段均受限。
- Node 显式引用扩展版本，更新包不静默改全部节点；有引用禁止直接删除。
- 缺 Renderer 不伪造订阅；仅声明 one_hop 但运行尚未支持时仍禁该组合。

- 解包同时限制压缩大小、展开总量、条目数和嵌套深度，拒绝绝对路径、..、符号链接/硬链接；在私有 staging 验证后落盘，失败不得覆盖已启用版本。
- 对最终编译出的 Xray/订阅结构再做白名单校验，不只校验 manifest。证书字段只能引用项目受控资源或受限内联内容，不能让扩展指定任意文件路径/外部加载器。secret 不回显到错误信息。
- 扩展字段名、标签、说明和预览都按纯文本或安全结构渲染，不插入原始 HTML/脚本；schema 声明能力不能自行绕过 Core/Renderer 的真实能力验证。

上述是本轮固定的工程默认与边界，不冒充原会话已逐项指定实现；真实证据证明不可行时说明冲突，不静默改变产品行为。

## IN SCOPE

- 实现最小 Schema/validator/compiler，提供一个真正可运行的额外协议示例。
- 贯通导入→静态验证→测试机 config test→启用→停用/删除页面与引用检查。
- 接动态字段、secret 存储及订阅 Renderer；写紧凑产品格式规范供外部 AI 使用。

## OUT OF SCOPE

不做通用脚本解释器、任意 Core 支持、Agent 插件框架或新发布状态机。

共同排除：商业系统、QuotaTemplate、Emergency Budget/Lease/Bypass、任意 Shell、Agent 插件运行时、多 Core/多跳、额外常驻依赖；不为未来功能做框架。

## IMPLEMENTATION TARGET

按 IN SCOPE 顺序形成最小可用闭环。先用现有代码/类型解决本项，不提出多套架构选项；只在当前任务产出上做必要重构。开发验证使用隔离数据；不会因任务包含安装/恢复就获得操作生产机器的授权。

## FILES EXPECTED

- internal/extensions/、examples/extensions/
- 节点/订阅接入与系统页面、必要迁移
- docs/protocol-extension.md（产品规范）

只创建真正必要文件；代码、Schema、测试与正式使用说明就是交接，不新增工作日志。

## MINIMUM TESTS

- 合法示例贯通到真实 Core/订阅；字段映射保真。
- 路径逃逸、脚本/未知表达式、覆盖 Stats、超大包代表负例。
- 缺能力/版本不兼容/测试组隔离/停用和带引用删除行为。

- 小压缩包大展开、链接文件、字段说明含 HTML/事件处理器、合法 schema 输出非法 Xray 字段均拒绝或安全转义。

优先复用已有测试；不为简单布局/getter/实现细节镜像写测试，不把编译或 mock 通过当真实代理/资源验证。

## VALIDATION

执行相关 targeted tests 与本任务的一次实际 smoke；只有改动共享契约或发现具体回归风险才扩大范围。根据真实工具链执行命令，回复给出命令/结果/未验证项。失败先判断来自环境、已有问题还是本次改动，不盲跑全套。

## ACCEPTANCE

- GOAL 形成真实可用结果，IN SCOPE 必需行为完成。
- CONTRACT 不变量成立，MINIMUM TESTS 和必要 smoke 有实际证据。
- 合法示例贯通到真实 Core/订阅；字段映射保真。
- 必需环境未测、存在影响合同的失败时保持 PARTIAL/BLOCKED；提供脚本不等于已经运行。
- 对相关既有行为完成下述回归，不以全仓重写掩盖失败。

## REGRESSION

内置 Reality、Token 权限和已有 Node 定义保持；无新增常驻进程。

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
