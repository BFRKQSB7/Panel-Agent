# D02 — 启动 Panel、管理员认证与持久设置

TASK ID: D02  
MILESTONE: M1  
STATUS: 只在 [任务索引](README.md) 维护，本文件为静态任务包。

## GOAL

形成可启动、可登录、重启后仍保留设置的最小管理后台。

## USER VALUE

本任务的可观察结果是：形成可启动、可登录、重启后仍保留设置的最小管理后台。 后续任务直接使用代码、类型与验证结果，不依赖会话记忆。

## WHY NOW

本任务无代码前置，可从当前项目开始。与相邻任务按稳定接口衔接，迁移、API、相关页面和最小测试在本任务内一起完成。

## SIZE

M。M 约半个有效开发日，L 约半天至 1.5 天，仅作初估；模型运行时长和环境等待另计。单次聚焦一个主要机制，不按文件数量硬切分。

## RISK

MEDIUM — 实现边界明确，认证/探测/输入限制仍须实际验证。

## RECOMMENDED MODEL

- CAPABILITY LEVEL: LEVEL 2（任务难度标记，不是模型官方评级）
- PREFERRED MODEL: DeepSeek V4.1 Flash；官方 API 模型名 `deepseek-flash`。
- FALLBACK MODEL: 同一模型；先做有证据的定位与最小修复。无法解决则标明阻塞，不自动换付费模型。
- THINKING: `high`，启用思考。
- WHY: 范围与输入输出明确；首次实现使用 high，后续纯文案/样式可局部降 low。
- MODEL RECHECK: 仅项目首次启动、提供方/模型变化、参数不支持时核查；连续任务不重复联网比榜单。当前官方别名对应 1017 版本，第三方平台需查实际映射。

## CAN DOWNGRADE MODEL

YES，仅当本次工作变成纯展示微调或执行已有检查时可用同模型 low；实现认证/网络边界、处理失败或修复代码仍按上方档位。

## DEPENDS ON

NONE

## BLOCKS

D03

只认真实代码与契约依赖；编号是默认执行顺序，不是额外前置。

## CONTEXT

Panel 只有一个管理员，使用本地初始化口令与安全会话；代理用户没有账号密码，不开放注册或多管理员。采用 Go 单体、SQLite 和内嵌轻量 UI，目标机不依赖 Node.js、Redis、Docker 或外部数据库。管理入口是后续机器、用户等功能的共同基础；本任务只提供启动、登录、注销、设置和明确错误反馈。项目尚无实现时选择最小依赖并固定版本，避免先生成整个后台。HTTPS 支持证书文件，也可使用管理员已有反代；不能自动安装 nginx 或关闭远端 TLS 校验。

## REQUIRED READING

### Always read

- 本任务全文。

### Source files to inspect

- go.mod、cmd/control/
- internal/control/、internal/storage/、internal/security/、internal/web/ 的启动/认证入口

只检查相关入口及其直接调用/类型。路径为预期定位，实际命名不同就找等价实现；不存在不等于必须创建所有列出的目录。

### Additional docs

- [tasks/README.md](README.md)：依赖状态及连续执行规则。

不要求读全量 PROJECT_PLAN、历史会话、旧任务或其他交接文件。真实契约冲突才定位原计划对应小节。

## EXPECTED BASELINE

可从空项目开始；D01 原型可能已创建 Go 模块，存在就沿用，无 Core 依赖。

## START REALITY CHECK

开始只做一次：核查依赖 DONE 及实际代码/API/Schema；读取相关入口，找出当前可执行验证命令；对照 baseline。命名/目录小差异直接适配，不能因此重新规划。若发现已经实现，验证后复用，不重写。必要环境、能力或契约缺失才标 blocker。

## CONTRACT

- 初始化只能在未建立管理员身份时进行；认证失败限流，口令用成熟密码哈希，会话可注销并设有效期。
- 远端管理入口通过 HTTPS；敏感 Cookie/CSRF/日志脱敏落实到真实请求路径。
- master key 初始化一次，后续发现缺失须报错，不能静默生成替代密钥。

- 首次管理员初始化只通过本机 CLI/明确受限的本机入口完成；未初始化时不向公网开放可被抢注的初始化 API。持久化管理员/密钥失败不进入已初始化状态。
- 缺失密钥时区分全新数据目录与已有加密数据；已有数据库不能自动重新初始化。初始化同时生成不可变 installation_id；默认单 Panel 写实例，启动加进程锁，禁止多个进程并发迁移/恢复。

上述是本轮固定的工程默认与边界，不冒充原会话已逐项指定实现；真实证据证明不可行时说明冲突，不静默改变产品行为。

## IN SCOPE

- 实现最小迁移与配置加载、管理员初始化/会话和认证中间件。
- 完成登录/注销/设置页面及嵌入资源，建立有限日志默认值。
- 给出本机开发和 HTTPS 运行命令，故障不泄露口令/密钥。

## OUT OF SCOPE

不创建未来所有表、Machine/Node CRUD、用户账号体系或全站页面占位。

共同排除：商业系统、QuotaTemplate、Emergency Budget/Lease/Bypass、任意 Shell、Agent 插件运行时、多 Core/多跳、额外常驻依赖；不为未来功能做框架。

## IMPLEMENTATION TARGET

按 IN SCOPE 顺序形成最小可用闭环。先用现有代码/类型解决本项，不提出多套架构选项；只在当前任务产出上做必要重构。开发验证使用隔离数据；不会因任务包含安装/恢复就获得操作生产机器的授权。

## FILES EXPECTED

- cmd/control/、internal/control/ 启动认证
- internal/storage/ 最小迁移、internal/security/、internal/web/

只创建真正必要文件；代码、Schema、测试与正式使用说明就是交接，不新增工作日志。

## MINIMUM TESTS

- 未登录管理 API 被拒；错误口令/注销后的会话无效；CSRF 代表负例。
- 初始化→登录→保存设置→重启→读取设置的 smoke。

- 未初始化的远端请求不能抢先设置管理员；已有数据库缺 master key 时启动失败且原数据不变。

优先复用已有测试；不为简单布局/getter/实现细节镜像写测试，不把编译或 mock 通过当真实代理/资源验证。

## VALIDATION

执行相关 targeted tests 与本任务的一次实际 smoke；只有改动共享契约或发现具体回归风险才扩大范围。根据真实工具链执行命令，回复给出命令/结果/未验证项。失败先判断来自环境、已有问题还是本次改动，不盲跑全套。

## ACCEPTANCE

- GOAL 形成真实可用结果，IN SCOPE 必需行为完成。
- CONTRACT 不变量成立，MINIMUM TESTS 和必要 smoke 有实际证据。
- 未登录管理 API 被拒；错误口令/注销后的会话无效；CSRF 代表负例。
- 必需环境未测、存在影响合同的失败时保持 PARTIAL/BLOCKED；提供脚本不等于已经运行。
- 对相关既有行为完成下述回归，不以全仓重写掩盖失败。

## REGRESSION

已有 D01 原型及 go.mod 可运行，不引入目标机额外服务。

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
