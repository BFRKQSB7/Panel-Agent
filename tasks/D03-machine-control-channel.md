# D03 — 机器注册、身份隔离与主动同步

TASK ID: D03  
MILESTONE: M1  
STATUS: 只在 [任务索引](README.md) 维护，本文件为静态任务包。

## GOAL

浏览器签发接入凭据，Agent 主动注册、拉取本机快照并回报状态，重启后保持身份。

## USER VALUE

本任务的可观察结果是：浏览器签发接入凭据，Agent 主动注册、拉取本机快照并回报状态，重启后保持身份。 后续任务直接使用代码、类型与验证结果，不依赖会话记忆。

## WHY NOW

已有 D02 的必要实现后，可以独立完成此边界。与相邻任务按稳定接口衔接，迁移、API、相关页面和最小测试在本任务内一起完成。

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

D02

## BLOCKS

D04

只认真实代码与契约依赖；编号是默认执行顺序，不是额外前置。

## CONTEXT

Machine 是机器身份，Node 是以后建立的用户入口。一个 Machine 只有一个 Agent；注册成功不自动创建 Node。约 30 分钟的一次性 Join Token 换取独立高强度 Machine Token，Panel 只存验证 hash，Agent 持久保存自身凭据。控制通道均由 Agent 主动 HTTPS 发起，不要求 NAT 提供管理端口。界面显示机器 hostname、可改名称/备注、系统架构、资源、最后心跳与版本。完整 Desired State 支持无变化响应及退避；先同步空 Nodes 快照即可，运行 Core 由下一任务负责。

## REQUIRED READING

### Always read

- 本任务全文。

### Source files to inspect

- cmd/control/、cmd/agent/
- internal/control/ 认证路由、internal/storage/ 迁移
- internal/protocol/、internal/agent/（存在才读）

只检查相关入口及其直接调用/类型。路径为预期定位，实际命名不同就找等价实现；不存在不等于必须创建所有列出的目录。

### Additional docs

- [tasks/README.md](README.md)：依赖状态及连续执行规则。

不要求读全量 PROJECT_PLAN、历史会话、旧任务或其他交接文件。真实契约冲突才定位原计划对应小节。

## EXPECTED BASELINE

D02 已有管理认证、SQLite、配置与嵌入页面；暂无真实 Node 应用。

## START REALITY CHECK

开始只做一次：核查依赖 DONE 及实际代码/API/Schema；读取相关入口，找出当前可执行验证命令；对照 baseline。命名/目录小差异直接适配，不能因此重新规划。若发现已经实现，验证后复用，不重写。必要环境、能力或契约缺失才标 blocker。

## CONTRACT

- API 以认证身份限定 Machine，不信请求 body 的 machine_id；Machine A 不得访问 B 或 admin。
- 注册事务消费 Join Token。默认由 Agent 在请求前生成并持久化注册 ID 与高强度候选 Machine Secret，HTTPS 注册成功后才成为正式 Machine Token；Panel 仅存验证 hash。成功回复丢失时以同一候选 secret 认证查询已完成注册结果，不需服务器从 hash 还原秘密，不凭注册 ID 单独取回身份。新注册仍校验未用/未过期 Join Token。
- 快照身份为 authority_epoch、revision、digest；只有已认证 Panel 可改变 authority，跨 authority 不仅比较 revision 大小。
- 先固定 register、desired-state、status 消息和错误类型；后续字段向后兼容，不另建命令队列。

- 配置 authority 是随机身份而非可排序整数。切换需一次新 nonce 绑定的已认证全同步握手；每机只允许一个配置拉取/应用序列在途，切换时取消旧响应，持久保存当前 authority。迟到回复、旧握手和旧 applied/status 不可把 authority 切回。
- 同一 authority 下拒绝旧 revision、同 revision 不同 digest；未变化响应也校验当前 authority/request。重新配对保持可恢复 ledger 身份；仅机器认证 secret 更新不自动新建账本，真正重装另走断档登记。

上述是本轮固定的工程默认与边界，不冒充原会话已逐项指定实现；真实证据证明不可行时说明冲突，不静默改变产品行为。

## IN SCOPE

- 实现机器表、签发/注册/吊销/重新配对、CLI 配对与机器页面。
- 实现约 30 秒拉取、30～60 秒状态回报、抖动退避及身份持久化。
- 实现空完整快照与 desired/received 状态；未实现 Core 时不能标 applied 成功。

## OUT OF SCOPE

不做 Core 安装、节点代理、账本、远程 Shell。

共同排除：商业系统、QuotaTemplate、Emergency Budget/Lease/Bypass、任意 Shell、Agent 插件运行时、多 Core/多跳、额外常驻依赖；不为未来功能做框架。

## IMPLEMENTATION TARGET

按 IN SCOPE 顺序形成最小可用闭环。先用现有代码/类型解决本项，不提出多套架构选项；只在当前任务产出上做必要重构。开发验证使用隔离数据；不会因任务包含安装/恢复就获得操作生产机器的授权。

## FILES EXPECTED

- internal/protocol/ 消息、internal/agent/ 同步与状态
- internal/control/ machines、对应迁移与页面、cmd/agent/

只创建真正必要文件；代码、Schema、测试与正式使用说明就是交接，不新增工作日志。

## MINIMUM TESTS

- Join 过期/复用、注册响应丢失、跨机器/admin 越权代表用例。
- 重启保持身份；断网退避恢复；吊销后拒绝 API，重新配对有效。

- 注册已提交但响应丢失，重启后仅恢复一个 Machine；只有注册 ID/原 Join Token 的另一客户端不能取回身份。
- 新 authority 握手后到达旧快照/旧状态；同 revision 不同 digest；均不回滚已确认身份。

优先复用已有测试；不为简单布局/getter/实现细节镜像写测试，不把编译或 mock 通过当真实代理/资源验证。

## VALIDATION

执行相关 targeted tests 与本任务的一次实际 smoke；只有改动共享契约或发现具体回归风险才扩大范围。根据真实工具链执行命令，回复给出命令/结果/未验证项。失败先判断来自环境、已有问题还是本次改动，不盲跑全套。

## ACCEPTANCE

- GOAL 形成真实可用结果，IN SCOPE 必需行为完成。
- CONTRACT 不变量成立，MINIMUM TESTS 和必要 smoke 有实际证据。
- Join 过期/复用、注册响应丢失、跨机器/admin 越权代表用例。
- 必需环境未测、存在影响合同的失败时保持 PARTIAL/BLOCKED；提供脚本不等于已经运行。
- 对相关既有行为完成下述回归，不以全仓重写掩盖失败。

## REGRESSION

D02 管理员认证和日志脱敏保持；吊销 API 不伪称离线机器已停止代理。

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
