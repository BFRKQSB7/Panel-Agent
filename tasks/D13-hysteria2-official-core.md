# D13-HY2 — 官方 Hysteria 2 内核适配

状态只在 [README](README.md) 维护。此任务是用户新增协议的静态工作包，不重编号原 D01–D17；D17 仍是最终发布关口。

## 契约

沿用 [项目补充契约](../PROJECT_PLAN.md)。官方二进制独立运行，禁止把 hy2 渲染成 Xray 入站或执行扩展提供的脚本。复用用户、权限、配额、周期、账本和机器身份；不新增独立用户库。

## 实施顺序

1. 官方适配边界：回环认证、累计统计和踢人客户端，固定结构配置渲染；留下可执行验证用例。
2. Agent 生命周期：按 Node 的独立实例、受保护文件、进程归属、UDP 监听、健康检查、候选应用、恢复及安全停止。全部目标实例完成后才能确认整机版本；旧 Agent 拒绝未知能力，禁止漏节点后 ACK。
3. 认证与账本接入：本地快照、离线到期、三维配额、撤销已有连接、独立 source/generation、水位事务、周期边界与未 ACK 补报。回滚不得恢复已撤销权限；Agent 认证服务故障时不能留下不受监管的旧连接。
4. Panel 与订阅：显式 hy2 类型、加密凭证/证书、能力门控、公网/本地 UDP 端口、URI/Mihomo、动态教程；未接完前不暴露可启用选项。
5. 运维：固定官方发行版本及校验、按需安装、独立手动升级与回滚、一致性备份恢复、普通卸载及 purge 的所有权边界。完成工作包后各自本地提交，不推送。

## 后续验收（当前不执行）

- 单元/HTTP 用例：错误认证、过期、撤销、重复 ID、负数/溢出统计、统计 API 重定向、鉴权错误、响应大小上限、超时、不清零采样。
- 官方 Core：真实 TCP/UDP 转发、逐用户逐节点计量、并发撤销与重连、Agent/Core 崩溃、Panel 离线跨周期及恢复、证书更新、双内核相互隔离。
- 四机：凤凰城提供 Panel/公网 hy2/可达 Exit；冰岛两机分别覆盖 Reality 和 hy2 NAT；日本覆盖跨地区访问及恢复。UDP 映射在执行前确认与设置，不覆盖现有 TCP 服务。
- 发布：amd64/arm64、固定版本升级失败回滚、资源与磁盘稳态。单协议/多实例/双内核结果分开记录；Alpine、cgroup 或大内存宿主结果不冒充原 Debian/Ubuntu 整机最低规格。

## 官方接口依据

- https://v2.hysteria.network/docs/advanced/Full-Server-Config/
- https://v2.hysteria.network/docs/advanced/Traffic-Stats-API/
- https://v2.hysteria.network/docs/developers/URI-Scheme/
