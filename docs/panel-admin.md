# Panel 管理入口

Panel 使用 Go 单体、SQLite 和内嵌页面。默认数据目录为 `./data`，本机开发监听 `127.0.0.1:8080`；未初始化管理员时，`serve` 会报错退出，不会打开管理端口，也没有可远程抢注的初始化 API。

## 本机开发

在仓库根目录先运行初始化命令。终端会隐藏输入并要求确认管理员口令，口令至少 12 字节、最多 72 字节；口令不会放进命令参数或环境变量。

```powershell
go run ./cmd/control admin init --data-dir .\data
go run ./cmd/control serve --data-dir .\data --listen 127.0.0.1:8080
```

浏览器打开 `http://127.0.0.1:8080`。登录后可以修改面板名称、注销并重新登录。管理员口令使用 bcrypt 保存，会话在进程重启后失效，登录失败达到五次后会限流 15 分钟。自动化初始化可用 `--password-stdin`，标准输入必须恰好包含两行口令；不要把口令写入脚本参数、环境变量或日志。

## HTTPS 运行

直接对外监听时必须配置证书和私钥，服务最低使用 TLS 1.2。证书路径通过环境变量传入：

```powershell
$env:PANEL_TLS_CERT = 'C:\panel\tls\fullchain.pem'
$env:PANEL_TLS_KEY = 'C:\panel\tls\private.key'
go run ./cmd/control serve --data-dir C:\panel\data --listen 0.0.0.0:8443
```

也可以让已有反向代理终止 TLS。此时 Panel 只绑定 loopback，代理必须覆盖 `X-Forwarded-Proto` 为 `https` 并保留浏览器请求的 Host；服务仅在连接来自本机 loopback 代理时才信任该协议头：

```powershell
$env:PANEL_TRUST_PROXY = 'true'
go run ./cmd/control serve --data-dir C:\panel\data --listen 127.0.0.1:8080
```

服务会使用 HttpOnly、SameSite Strict Cookie、CSRF token、同源 Origin 检查、限流和安全响应头。数据目录包含 `panel.db` 与 `master.key`，应一起纳入受限权限的备份；如果数据库仍在而密钥缺失，服务会拒绝启动且不会生成替代密钥。当前实现默认单个 Panel 写进程，并使用进程锁阻止并发启动。

## Agent 配对与控制通道

管理员登录后打开“机器管理”，签发一次性 Join Token，并在 30 分钟内将它安全地传到目标机器。Agent 只主动连接 Panel 的 HTTPS 地址；若使用自签证书或私有 CA，把签发证书保存到 Agent 主机并用 `--ca-cert` 指定，公有可信证书不需要该参数。不要把 Join Token 放进命令行参数、环境变量或日志，下面的示例通过标准输入传递：

```powershell
$panelUrl = 'https://panel.example.com'
$joinToken = Read-Host '粘贴一次性 Join Token'
$joinToken | .\control-agent.exe pair --panel-url $panelUrl --data-dir C:\panel-agent\data --name '边缘机器 1' --join-token-stdin
```

配对会在本地持久保存机器身份，然后立即拉取并校验完整 desired-state 快照。若注册已提交但连接中断，使用相同数据目录再次执行 `pair` 会凭本地候选密钥安全恢复结果；不要删除或复制 `agent.identity.json`，它包含机器凭据，必须限制为 Agent 服务账户可读。确认配对成功后，可以前台方式持续同步，Ctrl+C 安全退出：

```powershell
.\control-agent.exe run --data-dir C:\panel-agent\data
```

`run` 成功后每 30～35 秒同步一次，失败时指数退避并加入抖动，恢复连接后自动继续。`sync` 可手动执行单次同步。重新配对时，先在机器管理页面为目标机器签发专用凭据，再在同一 Agent 数据目录执行 `re-pair`；它保留机器 ID 并轮换认证密钥。吊销机器凭据会立即拒绝该机器的后续 API 请求，但不会远程停止机器上的进程。未传 `--xray-path` 时只同步控制通道，不报告 Core 已应用。

## Reality 节点与 Linux Core

进入机器管理中的“Reality 节点”，填写公网主机、公网端口、监听 IP、监听端口、Reality SNI 和目标。公网值仅是管理员录入，不代表已验证 NAT 映射；监听端口 10085 保留给回环 Stats API。节点自动生成独立 X25519 密钥和 Short ID，私钥与机器快照在 Panel 数据库中加密；管理员页面只展示公钥。普通节点默认没有开发凭证，正式用户授权由用户管理功能提供；隔离试验可显式勾选开发凭证，同一开发 UUID 在不同 Node 使用不同统计 email。

### 一跳 Exit

在机器管理中可为每台机器配置一个隐藏 Exit。当前固定的 Transit 实现是独立端口上的 VLESS + REALITY + RAW：Panel 为每个入口机器与 Exit 机器对生成独立 Transit UUID，入口只拿目标 Exit 的公网映射、REALITY 公钥和该 UUID，Exit 只拿对应入口机器的 Transit 凭证；这些字段不会进入用户页面或 URI/Mihomo 订阅。Exit 的监听端口必须与本机 Node 不冲突，公网主机和端口需要管理员按实际 NAT 映射填写，控制通道在线不代表该公网入口已被验证。

节点编辑页的“出口”选择只能指向另一台机器的 Exit。Exit 快照先应用并报告成功后，入口 Node 才会启用一跳路由；Exit 未应用、被吊销、应用失败、轮换中或机器硬停时，入口保持禁用，不回退到本机 Direct。Exit 删除在仍有 Node 引用时拒绝；保存配置或轮换 Transit 后，两端重新应用完成前同样保持 Fail Closed。入口用户流量只生成 `user_entry` 账本流，Transit 入站只生成 `machine_only_transit` 账本流，后者只归属 Exit 机器，不重复计入用户或出口 Node。

节点详情页会根据当前机器心跳、Core 应用版本和 Node 的公网/监听字段生成 NAT、DNS、Agent/Core 和 VLESS Reality 引导。复制按钮只复制页面已有的只读值；页面不会自动修改 DNS、云防火墙、证书、CDN 或 NAT。公网检查只面向当前 Node 的公网主机和端口，先做一次 DNS 解析，再把 TCP 拨号绑定到选中的地址；解析结果包含私网、回环、链路本地、共享地址或 IPv4-mapped IPv6 时直接拒绝，避免 DNS 重绑定后继续拨号。

页面上的“检查公网 TCP”使用 `public` 模式，适用于公网域名或公网字面 IP；“检查本机回环”仅接受字面回环地址，不解析域名。检查有固定的 DNS、拨号超时和并发上限，不接受任意 URL、重定向或元数据目标；成功只表示所选地址的 TCP 监听可达，不能证明 Agent 在线、Core 已应用、Reality 握手、UDP、订阅内容或最终出站链路已经验收。自动化管理员可调用 `POST /api/admin/machines/{machineID}/nodes/{nodeID}/connectivity-check`，提交 `{"mode":"public"}` 或 `{"mode":"local"}`，响应分别给出 `dns`、`tcp`、`resolved_ip`、`target` 和 `ok`；DNS 目标被拒绝时返回 422，其他检查结果返回 200。

Linux Agent 使用已验证的 Xray **26.10.21** 可执行文件。以下命令中的路径由管理员提供；此阶段不会下载或安装 Core：

```sh
./control-agent run --data-dir /var/lib/panel-agent --xray-path /opt/panel-agent/xray
# 自签证书时追加 --ca-cert /path/to/panel-ca.pem
```

所有 Node 合并为一个 Core，由同一个数据锁和串行应用边界管理。Agent 先持久保存候选，执行配置检查及端口预检查，再采样旧 Core、停止旧进程、启动候选并检查全部监听和 Stats 健康；通过后才报告 applied。Core generation 与 Core instance ID 独立持久化，不是账本 epoch。仅名称、公网地址等元数据变化不会重启；authority 改变也会检查候选并保留回滚配置。配置失败保留 Current，启动或健康失败恢复 Current/LKG；Agent 中途退出后，重启会先按本地 journal 恢复，Panel 失联仍使用已确认配置。退出 Agent 不停止 Core。

**节点或凭证撤销通过完整 Core 重启执行，同机其他节点的现有连接也会中断并需重连。**已接收 revision 与已应用 revision 分开显示；应用失败不代表旧 Core 已停止服务，具体原因见节点页的应用错误。Agent 正常每 30～35 秒取配置，撤销生效需等待同步和应用完成。

`agent-data/core/` 保存权限 0600 的配置及 journal，目录必须为 0700；不得手工编辑、跨机器复制或删除。只保留 Current、上一成功 LKG 和正在应用的候选配置。重启前保留旧统计映射与不清零的累计读数，候选尚未确认时的可观测流量也保留；`boundaries/` 的累计观察由持久账本事务去重消费，不能直接相加。固定 Stats API 不能原子完成采样和停止，边界明确记录不可观测尾部；丢失 Core 时记录 gap，不伪造零用量。旧版无周期策略、纯开发流量的原始观察仍保留，达到 126 份未消费边界时拒绝新应用，额外两份保留给候选故障恢复，不能删除观察来绕过上限。常驻 Agent 每 5 秒持续采样，账本、恢复与 `control-agent ledger` 诊断见 [Agent 流量采样与恢复](agent-ledger.md)；当前正式 Panel 尚未启用流量入账和配额执行。

到期处理还会在该目录保存 Current/LKG 所需的私有原始快照和最新已确认的授权摘要。恢复旧配置时，只保留最新已确认配置仍允许的节点、用户与 UUID，不能重新启用已撤销入口或轮换前的秘密；这可能使部分用户暂时不可用，但不会恢复已撤销的权限。旧版本回滚源没有足够授权信息时拒绝恢复该源。到期后重启会先重新生成并检查配置，不会从 LKG 恢复过期凭证；确认过的私有策略文件丢失时停止受管 Core，不生成替代凭证。

Core 不写 access/error 日志，Agent 每次连续失联只输出一次有长度限制的错误，恢复时输出一次；常驻运行时应由服务管理器限制 stderr 保存量。Windows 可运行 Panel 和控制通道，但本阶段受管 Core 的进程恢复实现仅支持 Linux。

### 隔离回环测试

固定 Xray 对服务端代理默认阻止私网和回环目标，参见 [Freedom 的默认策略](https://xtls.github.io/en/config/outbounds/freedom.html)。仅在隔离数据目录和全部凭证明确标识为开发凭证的 Node 上，可追加 `--development-loopback-test`；它只允许这些 Node 访问 TCP `127.0.0.1:18080`、UDP `127.0.0.1:18081`，普通 Node 不加入例外路由。该开关默认关闭，模式写入 journal，后续恢复必须使用相同模式；不要对生产目录启用。

## 用户、组与正式连接凭证

在“用户组与节点组”页面先创建节点组并勾选节点，再创建用户组并关联节点组；随后在“用户管理”中选择一个用户组创建用户。每个用户恰好属于一个用户组，重叠节点组会去重；实验节点通过独立组隔离。把新节点加入已有授权组即可让相关用户获得它，无需逐个修改用户。停用、删除或移出组会发布新的机器配置，机器失联不会删除 Node 定义。

每个正式用户有独立 VLESS UUID，在同协议的所有授权节点复用；不同 Node 使用稳定且独立的统计标签。UUID 由 master key 可恢复加密保存，和后续页面/订阅 Token 独立。“查看正式连接凭证”展示管理员可复制的 UUID，“轮换连接凭证”是单独操作，没有双凭证过渡期；Agent 成功应用后旧 UUID 失效，现存连接也会随 Core 重启结束。删除后同名重建获得全新的用户 ID 与 UUID，旧身份保留用于迟到流量归属。

到期时间在页面中使用 **UTC**，留空为永久；到期不改写手动启停状态，所以可以同时看到“手动停用”和“已到期”。Node 快照可携带明确到期时间，Agent 在本地移除过期凭证，Panel 失联也按已确认策略执行。**需要持续运行 Agent 的 `run` 模式；仅执行一次 `sync` 后退出不会执行后续定时检查，Xray 本身不识别本系统的绝对到期时间。**Agent 恢复时会先检查到期，再接触控制通道。

额度以字节保存，留空/JSON `null` 表示不限，JSON `"0"` 表示零（表单输入 `0`）；当前阶段只保存，不执行配额限制。批量创建最多 100 个，逐行显示成功或失败；也可勾选用户后批量切组、启停、设置到期/额度、轮换或删除。部分成功不会回滚已经完成的项目，重复提交轮换会再次生成秘密。

管理 API 使用现有管理员会话；写操作还需要同源 `Origin` 和 `X-CSRF-Token`。机器 Bearer 凭据不能访问这些 API，未知 JSON 字段会拒绝。

| 路径 | 操作与字段 |
|---|---|
| `/api/admin/groups/node`、`/api/admin/groups/user` | GET 列表，POST 创建；节点组提交 `name,node_ids`，用户组提交 `name,node_group_ids` |
| `/api/admin/groups/{node或user}/{groupID}` | PUT 替换组及关系，DELETE 删除；删除保留稳定身份 |
| `/api/admin/users` | GET 列表，POST 创建；字段为 `name,user_group_id,enabled,expires_at,quota_bytes` |
| `/api/admin/users/{userID}` | GET 当前账户条件与有效节点；PUT 修改，DELETE 删除 |
| `/api/admin/users/{userID}/rotate` | POST 独立轮换正式凭证 |
| `/api/admin/users/batch/create` | POST `users` 数组，返回每项结果 |
| `/api/admin/users/batch/change` | POST `operation,user_ids` 及对应字段；操作为 `move,enable,disable,expire,quota,rotate,delete` |

API 的 `quota_bytes` 必须是规范十进制字符串或 `null`，例如 `"9007199254740993"`；最大值为 `"9223372036854775807"`，数字 JSON、负数、小数、指数及越界拒绝。页面直接保存文本，不经浮点转换。API 的 `expires_at` 留空或使用带时区的 RFC3339 时间；页面转换为 UTC。批量结果的错误码区分名称冲突、对象不存在、字段无效和保存失败，页面显示中文说明。返回的账户有效节点反映当前授权目标，实际生效进度以机器的已应用版本和心跳为准。

## 私有用户页与订阅

在“用户管理 → 私有链接管理”中选择用户，显式创建或重置用户页、订阅或两类独立 Token。默认使用 256 位随机秘密，可加 16 字符以内的可读前缀；完整自定义值必须通过长度、字符多样性及重复模式校验，推荐由密码管理器生成。完整值不能同时带前缀，也不能用于一次同时重置两类 Token。不同用户、不同类型及已撤销 Token 都禁止复用同一值。

服务端只保存 hash，成功响应只显示一次明文。当前标签页用 sessionStorage 临时保留链接，30 分钟后读取失效；可点击清除按钮删除临时记录和当前交付区域的明文。换浏览器、丢失响应或关闭标签页后不能由服务端恢复秘密，需要明确发起新操作。批量操作最多 100 位用户，逐项交付并显示失败；15 分钟内使用同一签名操作凭据重试，已成功项只返回“已处理”，不再次轮换，也不重新交付明文。需要再次轮换成功项时先点击“开始新的一次操作”。

用户页地址为 `/u/DASHBOARD_TOKEN`，订阅地址为 `/s/SUBSCRIPTION_TOKEN/uri` 或 `/s/SUBSCRIPTION_TOKEN/mihomo`。Dashboard 只显示本人的有效授权节点、账户到期和额度配置；当前无可靠用量，页面明确显示计量和额度执行尚未启用。心跳失联不会删除节点，心跳与已应用状态也不代表公网连通性。用户唯一写操作是重置自己的订阅 Token，重置后旧 URL 立即失效，已缓存的 VLESS UUID 保持有效；撤销连接需要管理员轮换协议凭证或改变授权，并等待 Agent 应用。

| 格式 | 当前支持与导入方式 |
|---|---|
| URI | 逐行 VLESS Reality URI，包含正式 UUID、公网地址、公钥、SNI 和 Short ID。已按 Xray 26.10.21 验证连接；客户端自行设置分流，URI 不包含完整路由。 |
| Mihomo | 导入完整配置 URL，或下载 `mihomo.yaml` 后作为配置文件加载；内容采用 JSON（YAML 子集）。固定按 Mihomo 1.19.33 验证，显式启用 `support-x25519mlkem768`，默认本机混合代理端口为 7890。 |
| sing-box Reality | 用户已确认禁用当前组合。`/s/TOKEN/sing-box` 返回 HTTP 422 与明确原因，不能作为客户端配置导入。sing-box 1.14.3 与固定 Xray 26.10.21 的该组合实测认证失败。 |

Mihomo 预设按顺序将局域网、国内域名和国内 IP 设为 DIRECT，其余交给 PROXY 选择组。国内数据来自 [MetaCubeX 的规则库](https://github.com/MetaCubeX/meta-rules-dat)，由客户端首次下载并每天更新；首次使用须确认两份规则已成功加载，网络无法下载时应先处理客户端的规则加载错误。导出配置不启用 TUN、不修改系统代理，也不包含外部控制接口；客户端需要自行选取 PROXY 组内节点。无有效授权节点时，PROXY 使用 REJECT，不输出虚构节点。

页面和订阅使用 `private, no-store`、`no-referrer`，无外部页面脚本或追踪；服务端访问日志在进入路由前遮蔽私有路径，并且不记录 query、headers 或 body。若另接反向代理，应关闭这些路径的代理访问日志，避免代理在请求到达 Panel 前保存 Token。现代浏览器在 `no-referrer` 下的表单 POST 可能使用 `Origin: null`；仅 `Sec-Fetch-Site: same-origin` 且对应 CSRF / 签名操作凭据有效时接受，跨站或缺失签名仍拒绝。

| 路径 | 操作 |
|---|---|
| `GET /api/admin/user-tokens/action` | 管理员获取 `action_token` 和 `csrf`；不创建或重置用户 Token。 |
| `POST /api/admin/user-tokens/batch` | 管理员提交 `kind`（`dashboard` / `subscription` / `both`）、`action_token`、`items`；每项为 `user_id` 与可选 `spec: {prefix,value}`，返回逐项 `results`。需会话、CSRF 与来源校验。 |
| `POST /u/DASHBOARD_TOKEN/subscription/reset` | 用户提交页面给出的 `action_token` 与可选 `spec`；必须来自本站，拒绝额外的用户 ID、组、额度和到期字段。GET 不执行重置。 |

客户端字段依据 [Mihomo VLESS](https://wiki.metacubex.one/en/config/proxies/vless/) 和 [Reality TLS 选项](https://wiki.metacubex.one/en/config/proxies/tls/)。sing-box 的当前限制可见其 [Reality 实现](https://github.com/SagerNet/sing-box/blob/v1.14.3/common/tls/reality_client.go)；配置解析通过不能代替实际握手验收。

## 验证

```powershell
go test ./internal/security ./internal/storage ./internal/control
pwsh -File .\bench\run-d02-smoke.ps1
```
