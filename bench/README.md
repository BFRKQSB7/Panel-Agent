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
