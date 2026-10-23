# D12 — 动态 NAT/DNS 教程与有限连通性检查

TASK ID: D12  
MILESTONE: M4  
STATUS: 只在 [任务索引](README.md) 维护，本文件为静态任务包。

## GOAL

根据当前机器/节点值生成可复制的映射与域名步骤，并区分检测失败的位置。

## USER VALUE

本任务的可观察结果是：根据当前机器/节点值生成可复制的映射与域名步骤，并区分检测失败的位置。 后续任务直接使用代码、类型与验证结果，不依赖会话记忆。

## WHY NOW

已有 D06 的必要实现后，可以独立完成此边界。与相邻任务按稳定接口衔接，迁移、API、相关页面和最小测试在本任务内一起完成。

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

D06

## BLOCKS

D17

只认真实代码与契约依赖；编号是默认执行顺序，不是额外前置。

## CONTEXT

管理员需要知道 NAT 商家该填公网端口→本地端口、DNS 商家该填记录类型/主机名/目标/TTL。公网 IP/域名及内外端口来自用户确认，不能把内网地址或共享出口探测值当映射；A/AAAA 记录不携带端口，IP 直连不强迫绑域名。Reality 不要求普通 TLS 证书上传；需证书的其他协议依 capability 显示教程。检测分 Agent 在线、Core 配置、监听、DNS 和公网 TCP，成功不代表代理协议必然正确，探测位置也影响结论。此任务只在已有节点详情增加完整使用引导。

## REQUIRED READING

### Always read

- 本任务全文。

### Source files to inspect

- internal/web/ 机器/节点详情与复制控件
- internal/control/ nodes/machines 状态 API
- internal/protocol/ capabilities、受限检测入口（若存在）

只检查相关入口及其直接调用/类型。路径为预期定位，实际命名不同就找等价实现；不存在不等于必须创建所有列出的目录。

### Additional docs

- [tasks/README.md](README.md)：依赖状态及连续执行规则。

不要求读全量 PROJECT_PLAN、历史会话、旧任务或其他交接文件。真实契约冲突才定位原计划对应小节。

## EXPECTED BASELINE

D06 有节点、订阅和用户流程；D11 可能未完成，教程读取已有 capability，不假设 Exit 已存在。

## START REALITY CHECK

开始只做一次：核查依赖 DONE 及实际代码/API/Schema；读取相关入口，找出当前可执行验证命令；对照 baseline。命名/目录小差异直接适配，不能因此重新规划。若发现已经实现，验证后复用，不重写。必要环境、能力或契约缺失才标 blocker。

## CONTRACT

- 不自动改 DNS/防火墙、不猜公网端口、不建证书中心；Cloudflare 提示按协议，默认不教启用 CDN。
- 检测仅针对管理员已配置目标并限制协议、超时、并发、返回大小；拒任意 URL/重定向/云 metadata 等探测滥用。
- 私网/loopback 如属明确本机配置需独立受限分支，不能为支持本机探测放开任意内网地址。

- 为 DNS 检测/连接一次解析并验证实际选中地址，再绑定该 IP 建连，不能校验后按 hostname 二次解析。处理 IPv4-mapped IPv6、loopback/link-local/private 地址及 DNS 重绑定；外部 HTTP 重定向不作为检测捷径。

上述是本轮固定的工程默认与边界，不冒充原会话已逐项指定实现；真实证据证明不可行时说明冲突，不静默改变产品行为。

## IN SCOPE

- 生成 NAT、A/AAAA、协议/证书步骤和逐项复制，当前值修改后同步更新。
- 接状态与有限检测结果，说明探测端/限制/下一检查位置。
- 合并进上下文教程入口，保持主导航轻量。

## OUT OF SCOPE

不做自动 DNS、ACME 平台、全网扫描、延迟测速或完整 UI 重设计。

共同排除：商业系统、QuotaTemplate、Emergency Budget/Lease/Bypass、任意 Shell、Agent 插件运行时、多 Core/多跳、额外常驻依赖；不为未来功能做框架。

## IMPLEMENTATION TARGET

按 IN SCOPE 顺序形成最小可用闭环。先用现有代码/类型解决本项，不提出多套架构选项；只在当前任务产出上做必要重构。开发验证使用隔离数据；不会因任务包含安装/恢复就获得操作生产机器的授权。

## FILES EXPECTED

- internal/web/ contextual-guide
- internal/control/ bounded-connectivity-check、必要文案

只创建真正必要文件；代码、Schema、测试与正式使用说明就是交接，不新增工作日志。

## MINIMUM TESTS

- 一组不同内外端口和 IPv4/IPv6 值生成正确指引。
- DNS 成功/TCP 失败、缺映射、Reality 无证书需求显示正确。
- 代表非法目标/超时受限；浏览器复制 smoke。

- 同一 hostname 首次公网、再次内网的重绑定，以及 IPv4-mapped IPv6 内网目标，不得绕过限制。

优先复用已有测试；不为简单布局/getter/实现细节镜像写测试，不把编译或 mock 通过当真实代理/资源验证。

## VALIDATION

执行相关 targeted tests 与本任务的一次实际 smoke；只有改动共享契约或发现具体回归风险才扩大范围。根据真实工具链执行命令，回复给出命令/结果/未验证项。失败先判断来自环境、已有问题还是本次改动，不盲跑全套。

## ACCEPTANCE

- GOAL 形成真实可用结果，IN SCOPE 必需行为完成。
- CONTRACT 不变量成立，MINIMUM TESTS 和必要 smoke 有实际证据。
- 一组不同内外端口和 IPv4/IPv6 值生成正确指引。
- 必需环境未测、存在影响合同的失败时保持 PARTIAL/BLOCKED；提供脚本不等于已经运行。
- 对相关既有行为完成下述回归，不以全仓重写掩盖失败。

## REGRESSION

已有订阅导入与手填端口保持；检测不得把一次失败当 Node 删除依据。

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
