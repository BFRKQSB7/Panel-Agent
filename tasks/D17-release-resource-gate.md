# D17 — 完整发布回归与资源验收

TASK ID: D17  
MILESTONE: M5  
STATUS: 只在 [任务索引](README.md) 维护，本文件为静态任务包。

## GOAL

取得可审核的完整 V1 运行证据和本地发行产物，只有所有硬门槛通过才宣称完成。

## USER VALUE

本任务的可观察结果是：取得可审核的完整 V1 运行证据和本地发行产物，只有所有硬门槛通过才宣称完成。 后续任务直接使用代码、类型与验证结果，不依赖会话记忆。

## WHY NOW

已有 D11、D12、D15 及其传递依赖的必要实现后，可以独立完成此边界。与相邻任务按稳定接口衔接，迁移、API、相关页面和最小测试在本任务内一起完成。

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

YES，仅当本次工作变成纯展示微调或执行已有检查时可用同模型 low；实现认证/网络边界、处理失败或修复代码仍按上方档位。

## DEPENDS ON

D11, D12, D15

## BLOCKS

NONE

只认真实代码与契约依赖；编号是默认执行顺序，不是额外前置。

## CONTEXT

V1 必须交付用户/组/三订阅、单 Core 主动控制、独立周期补报、三维配额、一跳 Exit、动态教程、受限扩展和必要运维。基准为 128MB 整机/1GB 总盘、1～3 Node、10～20 User、低并发 Reality，包含 Direct 与 One-Hop；Panel+本机节点目标 1C/512MB。官方范围 Debian/Ubuntu、amd64/arm64，交叉编译不是运行证据。用现有测试和基准集中做一次发布验收，不重写整套测试，不以删功能/改最低配置完成发布。环境缺失与代码失败需准确区分。

## REQUIRED READING

### Always read

- 本任务全文。

### Source files to inspect

- bench/、tests/integration/ 现有入口
- scripts/、packaging/、go.mod 与发行配置
- 只读失败场景相关源文件

只检查相关入口及其直接调用/类型。路径为预期定位，实际命名不同就找等价实现；不存在不等于必须创建所有列出的目录。

### Additional docs

- [tasks/README.md](README.md)：依赖状态及连续执行规则。
- 根 README.md（若存在）：核对真实部署命令/兼容说明。

不要求读全量 PROJECT_PLAN、历史会话、旧任务或其他交接文件。真实契约冲突才定位原计划对应小节。

## EXPECTED BASELINE

所有传递依赖 DONE，已有 targeted tests 和基准入口；真实 NAT/双架构/低配环境仍需核实。

## START REALITY CHECK

开始只做一次：核查依赖 DONE 及实际代码/API/Schema；读取相关入口，找出当前可执行验证命令；对照 baseline。命名/目录小差异直接适配，不能因此重新规划。若发现已经实现，验证后复用，不重写。必要环境、能力或契约缺失才标 blocker。

## CONTRACT

- 用户新增的官方 Hysteria 2 以 [D13-HY2](D13-hysteria2-official-core.md) 补充契约为准，覆盖本文旧单 Core 排除项；该适配完成及验收前不能将扩展后的 V1 标为完成。测试机器改为凤凰城、冰岛 NAT 两台、日本 NAT 一台，UDP 映射待配置；用户已允许继续测试。

- 硬门槛未验证不可 DONE；cgroup/256MB/交叉编译不能替代整机 128MB/双架构运行。
- 记录 OS/Core/Agent/客户端版本、swap、并发、持续时间、字节量和检测方法。
- 缺陷只修复验收直接阻断项，不趁机扩需求或重构全仓；复杂根因阻塞则停止。
- 生成本地产物/校验/许可证和兼容说明，不自动推送远程发布。

- 跨任务边界必须有最终组合验收：扩展节点的一跳能力与计量、升级后 Stats/权限、恢复后的补报/配置身份、安装清理后的备份/密钥/账本。不能用每个模块单独通过替代这些组合。
- 计量清理验收覆盖连续多个周期的 final/关闭前缀压缩、再重放旧报告；检查在线明细不会每周期永久增长。环境缺失仍是发布 blocker。

上述是本轮固定的工程默认与边界，不冒充原会话已逐项指定实现；真实证据证明不可行时说明冲突，不静默改变产品行为。

## IN SCOPE

- 执行三机生命周期：Panel-US/JP-NAT/HK-NAT，普通/测试组发布、代理、三维计量、离线跨周期补报、一跳及出口故障。
- 测整机 128MB/1GB 与 Panel 512MB，1/3/5 活跃连接建议档、启动/配置/升级峰值，至少一次固定长稳（建议 2 小时）和受控清理压力。
- 跑实际存在的全量检查一次，核对发行包、安装/恢复/更新/purge、许可证与兼容说明；失败修复后只重跑受影响范围。

## OUT OF SCOPE

不扩大平台、不部署用户生产机、不伪造数值、不另建验收状态文档。

共同排除：商业系统、QuotaTemplate、Emergency Budget/Lease/Bypass、任意 Shell、Agent 插件运行时、多 Core/多跳、额外常驻依赖；不为未来功能做框架。

## IMPLEMENTATION TARGET

按 IN SCOPE 顺序形成最小可用闭环。先用现有代码/类型解决本项，不提出多套架构选项；只在当前任务产出上做必要重构。开发验证使用隔离数据；不会因任务包含安装/恢复就获得操作生产机器的授权。

## FILES EXPECTED

- bench/ 可复现结果、本地发行包/校验
- 项目 README 的真实支持范围和迁移限制
- 必要的直接缺陷修复

只创建真正必要文件；代码、Schema、测试与正式使用说明就是交接，不新增工作日志。

## MINIMUM TESTS

- 全生命周期和身份/账本/恢复/扩展/清理既有核心测试一次。
- 真实 128MB/1GB、两架构及 512MB 共机记录；加速清理不冒充全年运行。
- 发行包安装/更新/恢复/卸载 smoke，文档命令与产物一致。

- 同 K 篡改归属、大整数、关闭压缩后重放、旧 authority 迟到消息、未知历史元数据，以及已有长连接的配额撤销，复用各任务现成故障用例。
- 额外协议的 Direct/One-Hop 按真实 capability 验证，unsupported 明确拒绝；所有权受保护文件在安装/升级/恢复/清理组合后完整。

优先复用已有测试；不为简单布局/getter/实现细节镜像写测试，不把编译或 mock 通过当真实代理/资源验证。

## VALIDATION

执行一次已有全量检查与本任务列出的真实发行/资源验证。根据真实工具链执行命令，回复给出命令/结果/未验证项。失败先判断来自环境、已有问题还是本次改动，不盲跑全套。

## ACCEPTANCE

- GOAL 形成真实可用结果，IN SCOPE 必需行为完成。
- CONTRACT 不变量成立，MINIMUM TESTS 和必要 smoke 有实际证据。
- 全生命周期和身份/账本/恢复/扩展/清理既有核心测试一次。
- 必需环境未测、存在影响合同的失败时保持 PARTIAL/BLOCKED；提供脚本不等于已经运行。
- 对相关既有行为完成下述回归，不以全仓重写掩盖失败。

## REGRESSION

完整 V1 边界：无应急额度/QuotaTemplate，单 Core，Fail Closed，独立 Token，安全清理。

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
