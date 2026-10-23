# D01 Core feasibility smoke

Run **go test ./internal/core/xray** and then **pwsh -NoProfile -File ./bench/run-d01-smoke.ps1** from the repository root. The default smoke requires Go 1.26 or newer, PowerShell 7, the existing Debian WSL distribution, and **curl.exe**; it uses ports 10085, 10808, 18080, 18443, and 18444. It preserves the Debian running/stopped state and never changes **.wslconfig**. The same runner also supports the SSH test-host mode below. Xray binaries, generated credentials, logs, caches, and temporary configs stay under the ignored **bench/.work/** directory or a unique <code>/tmp/panel-agent-d01-*</code> directory.

The runner pins official [Xray v26.10.21](https://github.com/XTLS/Xray-core/releases/tag/v26.10.21) release archives and checks their SHA-256 before extraction: Linux x64 **0EEC3ED4163C273D8C8BF821F7D219E257921CF584AA3FB970715E458E631978**; Windows x64 **FC58C39EE2F2717B4C54D28D374362389E5319B20AE44AEA76EA45A5243EC827**. The server fixture has two VLESS/REALITY inbounds in one Core, sharing a generated UUID but using emails **u0001-n01** and **u0001-n02**. A generated Windows client makes four parallel requests through each Reality inbound to a temporary HTTP target bound to the Debian guest address. The fixture's Freedom rule permits only that guest **/32**, TCP, port **18080**. The [Xray REALITY configuration](https://xtls.github.io/en/config/transports/reality.html) and [StatsService counters](https://xtls.github.io/en/config/stats.html) are generated only for this isolated run.

The Go adapter in **internal/core/xray** connects only to a literal loopback IP because Xray's local StatsService uses plaintext gRPC. **QueryCounter** sends **Reset=false**, then selects the exact returned stat name because Xray may return other names matching the query pattern. A missing exact name is an error; the restart check treats only that specific response as zero. The runner identifies a Core process by Linux PID plus process start time; Xray does not expose a persistent Core instance ID through this API. The observed restart produced a new process identity and zeroed both users' in-memory counters.

## Observed result

On 2026-10-22, the full smoke passed with one Linux Xray Core v26.10.21 and one Windows Xray client at a time. Both Node emails, despite sharing the same UUID, received independent uplink/downlink counters; traffic through either inbound left the other email's values unchanged. Each counter increased on a second traffic pass, and repeated non-resetting reads did not decrease it. The final counters were **node-a=664/1080** and **node-b=664/1080** bytes; after restarting the same Core config, both were **0/0**.

| Stage | Xray process peak RSS | Debian guest total memory |
|---|---:|---:|
| Linux config check | 29,856 KiB | — |
| Core startup | 33,236 KiB | 4,010,612 KiB |
| Idle after 5 seconds | 36,696 KiB | 4,010,612 KiB |
| Four parallel requests through Node A | 37,040 KiB | 4,010,612 KiB |
| Four parallel requests through Node B | 37,668 KiB | 4,010,612 KiB |

These are process **VmHWM** and guest **/proc/meminfo** readings from the default WSL configuration (memory=4GB, processors=4, swap=1GB). They are supplementary measurements, not a 128MB acceptance result or a whole-host peak. WSL failed to create its VM when temporarily capped at 128MB (**CreateVm/0x8007274c**); no Hyper-V, QEMU, VirtualBox, or Docker VM backend was available. The original **.wslconfig** was restored and its SHA-256 verified as **954753E75DECF036D07A087CC64F3E0A66594271F8562C0ED86B4FB9988F6D6B**. This was the original resource-test blocker; the user-approved VPS result below now supplies D01's resource acceptance.

The smoke validates a local HTTP destination through the real Reality proxy and local counters. It does not establish unrestricted public Internet egress or production-port behavior.

## D04 节点与安全应用 smoke

`run-d04-smoke.ps1` 在指定 Linux 测试机的独立 `/tmp/panel-agent-d04-*` 目录运行真实 Panel、同机 Agent 和唯一受管 Xray Core，Windows 运行真实 Xray 客户端。需要已由 D01 校验的 Linux/Windows Xray 26.10.21 缓存、Go、PowerShell 7、OpenSSH、`tar.exe` 和 `curl.exe`；沿用上方玩具冰岛的身份和密钥变量：

```powershell
pwsh -NoProfile -File .\bench\run-d04-smoke.ps1 `
  -SshDestination root@194.247.182.108 -SshPort 45000 `
  -SshIdentityFile $keyPath -SshKnownHostsFile $knownHostsPath `
  -ExpectedHostname iceland-W8eRiY -ExpectedIPv4 10.88.234.165
```

该 smoke 从真实 HTTPS 管理表单创建三个 Node，核对机器的 applied 状态、共享开发 UUID 与独立统计映射，验证三个 Reality 入口代理、无变化和元数据变更不重启、Panel 失联继续服务、健康确认前杀掉 Agent 后离线恢复，以及撤销已有 TCP/UDP 会话。同机其他 Node 的 TCP 会话也会因完整 Core 重启中断，测试随后验证它们可以重新连接。Linux opt-in 故障套件使用真实固定版 Core 验证坏配置、端口占用、预检查后端口被抢占导致的启动失败和各持久应用阶段恢复；未指定 `PANEL_AGENT_TEST_XRAY` 时常规 Go 测试不会假装运行这套真实 Core 测试。

固定版 Freedom 默认阻止服务端代理访问私网/回环地址。smoke 显式传 `--development-loopback-test`，仅为纯开发凭证 Node 放行 TCP `127.0.0.1:18080` 和 UDP `127.0.0.1:18081`，默认生产配置无此例外。管理 HTTPS、Stats 和 Reality 监听都在远端回环，Windows 通过严格校验既有 ED25519 主机密钥的 SSH exec 字节中继访问；D04 使用同一个测试目标程序的 stdio 中继模式及时传递远端 TCP EOF，D01 默认仍使用已有 `nc`。不修改 SSH、NAT、防火墙或现有 FilePanel/nginx。Windows 对这个隔离回环管理入口接受临时自签证书，Agent 则通过生成的 PEM 校验服务端证书。

测试读取根 cgroup 内存、OOM 计数和 Xray `VmHWM`。配置检查进程很短，报告的是成功采到的 HWM 和样本数，不把轮询样本冒充无遗漏的瞬时峰值。每次运行的结果、私有测试凭证、诊断和构建缓存保存在忽略目录 `bench/.work/d04-*`；远端按专属目录与进程身份停止，比较原服务 PID/命令行及监听，并清理隔离目录。原服务变化、残留监听或 OOM 都会使 smoke 失败。

2026-10-23 的完整实测通过，见 [d04-iceland.json](results/d04-iceland.json)。三个入口真实代理成功，未变化与元数据修改不重启；Panel 离线继续服务，健康确认前杀掉 Agent 后离线恢复原配置。撤销节点的已有 TCP/UDP 会话结束，同机其他节点的已有 TCP 也中断，随后重新连接成功；保留了旧映射、非零累计观察和不可观测尾部。11,607 个整机样本最大 103,399,424 字节（约 98.6MiB）；配置检查采到 5 个 HWM 样本、最大 17,756 KiB，受管 Core 运行样本 HWM 最大 24,728 KiB。OOM 计数保持零，原服务 PID/监听及清理检查通过；D03 HTTPS 进程 smoke 与相关 Go 测试通过。该结果不代替 D17 完整发布资源验收。

## D05 正式用户授权 smoke

使用相同连接参数执行 `bench/run-d05-smoke.ps1`。它复用 D04 的隔离运行器，创建默认关闭开发凭证的 Node，通过真实管理 API 创建普通/测试用户组与节点组，并使用正式 UUID 经 Reality 请求 `https://example.com/`。会验证组隔离在列表与实际代理一致、重叠关系去重、节点加组自动授权、组变更、旧 UUID 失效和新 UUID 成功、页面显示批量部分失败，以及持续运行 Agent 时 Panel 失联后的到期与重启行为。还会在独立测试数据中临时移走 master key，确认启动拒绝且恢复原 key 后凭证不变。

仅补跑当前源码的真实 Core 套件时，可在 D04 运行器追加 `-Authorization -CoreTestsOnly`；它不运行管理和代理集成测试。该套件还验证已确认策略文件丢失时停止受管 Core、请求无响应跨过到期点时及时移除凭证、LKG 回退不能恢复已撤销节点或旧 UUID，以及已消失候选的身份与不可观测 gap 保留。

2026-10-23 的 D05 集成与后续当前源码 Core 套件均通过，见 [d05-iceland.json](results/d05-iceland.json)。集成运行的 23,128 个整机样本最大 100,782,080 字节（约 96.1MiB），配置检查采样 HWM 最大 17,388 KiB，运行采样 HWM 最大 24,052 KiB；OOM、原有服务/监听和清理检查通过。D03 HTTPS 进程回归与 D04 完整节点/撤销 smoke 通过，正式记录分别保留集成二进制与后续 Core 套件的 SHA-256，不合并为一次发布验收。额度仅保存；到期需要 Agent 持续运行，单次 `sync` 退出不执行未来定时检查。

## 指定 VPS 的 SSH 模式

用户指定的测试机为 VPS 项目中的 `servers/LazyCatCloud-玩具冰岛`，对应 SSH `194.247.182.108:45000`、主机名 `iceland-W8eRiY`、内网 `10.88.234.165`。连接材料沿用该机器目录中的独立维护密钥和 `known_hosts`，不复制到本仓库；不能与 SSH 45100 的另一冰岛实例混用。以下路径变量应指向上述现有文件：

```powershell
pwsh -NoProfile -File .\bench\run-d01-smoke.ps1 `
  -SshDestination root@194.247.182.108 -SshPort 45000 `
  -SshIdentityFile $keyPath -SshKnownHostsFile $knownHostsPath `
  -ExpectedHostname iceland-W8eRiY -ExpectedIPv4 10.88.234.165 -Require128MiB
```

SSH 模式先校验机器身份、已记录的主机密钥、资源和测试端口。服务端、HTTP 目标与 Stats API 均绑定远端回环地址，Windows 运行唯一的测试客户端；远端只有一个 Xray Core。由于维护 SSH 禁用了端口转发，本地 `bench/ssh-relay` 使用已有 SSH 依赖和远端 `nc` 的命令通道传送字节流，固定使用已有 ED25519 主机密钥记录，不修改 SSH 配置或 NAT 映射。所有测试进程按专属目录和命令行核对后停止，隔离目录随之删除；失败时先把诊断日志取回忽略目录。

采样读取根 cgroup 的总内存占用（包含容器 OS 服务、既有 FilePanel/nginx、测试 Core 与 SSH），并比较前后 `oom`/`oom_kill`；不会重置系统统计。2026-10-23 用户明确接受这台 128MiB 整机容器作为 D01 的资源验收环境，其共享内核和 128MiB swap 应随结果报告；这不代表独立内核虚拟机或后续完整 Panel + Agent 发布资源验收。

2026-10-23 的 SSH smoke 通过，结构化实测结果见 [d01-iceland-128m.json](results/d01-iceland-128m.json)。16 次真实代理请求全部返回 HTTP 200；两入口各自的累计统计为 `624/1080` 字节，重复读取不清零，重启后均为 `0/0`。配置检查采样 HWM 为 14,332 KiB，启动/空闲为 20,420 KiB，Node A/B 低并发阶段的进程 HWM 分别为 21,284/21,676 KiB；50ms 间隔的 1,349 个系统样本最大为 67,182,592 字节（约 64.1MiB）。最终 swap 使用 933,888 字节，`oom`、`oom_kill`、`oom_group_kill` 均保持零；原 FilePanel/nginx 进程未重启，测试目录和进程清理后根盘可用空间恢复为 906,368 KiB，原有 SSH/443/8081 监听保留。改动后的默认 WSL smoke 也实际复验通过。
