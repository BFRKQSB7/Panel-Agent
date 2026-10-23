# D16 — 一致性备份、恢复与 Agent 重连

TASK ID: D16  
MILESTONE: M5  
STATUS: 只在 [任务索引](README.md) 维护，本文件为静态任务包。

## GOAL

导出并恢复完整 Panel 备份，保留密钥/机器身份，准确处理旧备份版本和流量缺口。

## USER VALUE

本任务的可观察结果是：导出并恢复完整 Panel 备份，保留密钥/机器身份，准确处理旧备份版本和流量缺口。 后续任务直接使用代码、类型与验证结果，不依赖会话记忆。

## WHY NOW

已有 D09、D13、D14 的必要实现后，可以独立完成此边界。与相邻任务按稳定接口衔接，迁移、API、相关页面和最小测试在本任务内一起完成。

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

D09, D13, D14

## BLOCKS

D15

只认真实代码与契约依赖；编号是默认执行顺序，不是额外前置。

## CONTEXT

Panel 单文件备份包含一致性 SQLite、master key、Panel 配置、协议扩展与版本/校验元数据，可选密码。Agent 不建备份系统。迁移后 Panel 地址/API 身份与 Machine Token 验证值保持时，Agent 自动重连。旧备份不能找回备份后已被 ACK 且 Agent 已清理的数据，应提示时间缺口。配置恢复若只比较 revision 整数会永久卡住 Agent，需使用 D03 的 authority_epoch/revision/digest。备份含秘密，下载、临时文件和密码处理沿用管理权限与所有权边界。

## REQUIRED READING

### Always read

- 本任务全文。

### Source files to inspect

- internal/storage/ SQLite 与 migrations
- internal/security/ key、internal/extensions/ 存储
- internal/control/ settings/backup、internal/agent/ authority handling
- internal/web/ 系统设置、ownership/path

只检查相关入口及其直接调用/类型。路径为预期定位，实际命名不同就找等价实现；不存在不等于必须创建所有列出的目录。

### Additional docs

- [tasks/README.md](README.md)：依赖状态及连续执行规则。

不要求读全量 PROJECT_PLAN、历史会话、旧任务或其他交接文件。真实契约冲突才定位原计划对应小节。

## EXPECTED BASELINE

D09 真实账本/水位，D13 扩展持久化，D14 ownership；D03 支持配置 authority，D02 有独立 master key。

## START REALITY CHECK

开始只做一次：核查依赖 DONE 及实际代码/API/Schema；读取相关入口，找出当前可执行验证命令；对照 baseline。命名/目录小差异直接适配，不能因此重新规划。若发现已经实现，验证后复用，不重写。必要环境、能力或契约缺失才标 blocker。

## CONTRACT

- 使用一致性 SQLite 备份 API/等价机制，不能裸复制正在写的 DB 漏 WAL。
- 先验证包/校验/版本/密钥/密码/路径，staging 全部成功后切换，失败不毁现有可用数据。
- 恢复后生成新的配置 authority，已认证 Agent 接收新 authority 的完整状态，不因 revision 较小拒绝；恶意未认证回退拒绝。
- 保留 installation_id、Machine 身份/Token hashes 与 Credential 可解密性；只重建配置 authority，local ledger_epoch 不因 Panel 恢复而重建。
- 记录备份时点和可确定缺口，旧水位重报须正确，已清理已 ACK 流量明确无法自动找回。

- 备份不仅要求 SQLite 一致，还要保证同一提交代的 DB、配置、master key、扩展文件相互一致：使用短暂写入屏障/不可变资源快照，复制结束前不清理所引用资源；不能在拷 DB 后随意读取已更新文件。
- 恢复进入单实例维护屏障，暂停流量入账、配置/升级下发与后台写任务，返回可重试状态且不 ACK。验证完整新代后关闭旧 DB/WAL 连接并切换 DB+key+资源，重启恢复失败可回到完整旧代，不能出现混合版本。
- 恢复后作废备份中的管理员会话和未消费 Join Token，取消旧 Upgrade Intent。Machine Token/用户 Credential 若备份后已轮换或吊销，恢复可能回退：仅凭据仍匹配的 Agent 自动重连，其余明确重新配对/轮换，不能宣称全体无条件重连或旧吊销永久保持。
- 旧备份可能缺少后来创建的 User/Node/policy 元数据：可验证的历史归属用墓碑对账，无法验证者保留本地未 ACK 数据并 quarantine/告警，等待可信元数据或人工明确处理；绝不按当前 scope 猜测入账或成功 ACK 丢弃。
- 新 authority 通过 D03 的 nonce/单在途握手切换；清空旧在途快照/ACK/意图的影响。恢复前已确认且已清理的流量仍存在无法找回的缺口，这不是重新创建 ledger_epoch 能修复的。

- 恢复对账核对 Agent 持久关闭前缀/关闭凭据及仍有的 final 清单：先重新入账可验证的保留记录，再对有旧提交证明且两端已无明细的缺口记录 recovery_gap，允许关闭边界跨过该已丢失范围；不补造字节。仍持有但归属未知的记录不得被当作缺失清除；缺证明/密钥不匹配时保持待对账，不能无条件抬高前缀。由此避免恢复旧备份后 close_seq 永久等待已不存在记录。

- 备份解包限制压缩/展开大小、条目与路径，拒链接逃逸；密码保护使用成熟认证加密方案，验证失败无副作用，KDF/解包资源使用受限。

上述是本轮固定的工程默认与边界，不冒充原会话已逐项指定实现；真实证据证明不可行时说明冲突，不静默改变产品行为。

## IN SCOPE

- 实现备份下载/密码选项、有限临时文件与恢复验证/替换。
- 实现恢复后的配置 authority 切换和重连协调，适配监听地址但保留身份。
- 完成系统 UI、恢复结果和真实限制提示，演练在写数据库和旧快照。

## OUT OF SCOPE

不承诺自动恢复已清理流量、不做 Agent 备份或云备份调度。

共同排除：商业系统、QuotaTemplate、Emergency Budget/Lease/Bypass、任意 Shell、Agent 插件运行时、多 Core/多跳、额外常驻依赖；不为未来功能做框架。

## IMPLEMENTATION TARGET

按 IN SCOPE 顺序形成最小可用闭环。先用现有代码/类型解决本项，不提出多套架构选项；只在当前任务产出上做必要重构。开发验证使用隔离数据；不会因任务包含安装/恢复就获得操作生产机器的授权。

## FILES EXPECTED

- internal/control/ backup/restore、系统设置页面
- 一致性存储快照/恢复、相关 Agent 协调
- 隔离恢复 fixtures 与必要用户说明

只创建真正必要文件；代码、Schema、测试与正式使用说明就是交接，不新增工作日志。

## MINIMUM TESTS

- 在写 SQLite 完整备份恢复，密钥/扩展/Machine Token 均有效。
- 损坏包/错密码/版本不兼容/越界路径拒绝，不覆盖旧数据。
- 旧备份恢复后 Agent 重连、配置应用、重报不双计，缺口明确。

- 备份时并发修改扩展/设置仍得到同一代；恢复切换 DB/key/资源期间终止进程，重启得到完整新代或旧代。
- 旧会话/Join/升级意图不复活；Token 已轮换的机器准确报告重新配对需要。
- 备份后新建用户/策略的迟到报告保留待对账；旧配置/ACK 延迟到达不使已完成恢复倒退。

- Panel 备份关闭前缀为 5、Agent 已清理到 10：验证旧关闭凭据、登记 6–10 的真实缺口后可继续 11 之后；若 8 仍有可恢复记录先补账，若 8 归属未知则保留待对账，伪造凭据不能越过缺口。

优先复用已有测试；不为简单布局/getter/实现细节镜像写测试，不把编译或 mock 通过当真实代理/资源验证。

## VALIDATION

执行相关 targeted tests 与本任务的一次实际 smoke；只有改动共享契约或发现具体回归风险才扩大范围。根据真实工具链执行命令，回复给出命令/结果/未验证项。失败先判断来自环境、已有问题还是本次改动，不盲跑全套。

## ACCEPTANCE

- GOAL 形成真实可用结果，IN SCOPE 必需行为完成。
- CONTRACT 不变量成立，MINIMUM TESTS 和必要 smoke 有实际证据。
- 在写 SQLite 完整备份恢复，密钥/扩展/Machine Token 均有效。
- 必需环境未测、存在影响合同的失败时保持 PARTIAL/BLOCKED；提供脚本不等于已经运行。
- 对相关既有行为完成下述回归，不以全仓重写掩盖失败。

## REGRESSION

D09 去重与墓碑、D13 扩展引用、D14 ownership 保持。

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
