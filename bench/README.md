# D01 Core feasibility smoke

Run **go test ./...** and then **pwsh -NoProfile -File ./bench/run-d01-smoke.ps1** from the repository root. The smoke requires Go 1.26 or newer, PowerShell 7, the existing Debian WSL distribution, and **curl.exe**; it uses ports 10085, 10808, 18080, 18443, and 18444. It preserves the Debian running/stopped state and never changes **.wslconfig**. Xray binaries, generated credentials, logs, caches, and temporary configs stay under the ignored **bench/.work/** directory or a unique <code>/tmp/panel-agent-d01-*</code> directory.

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

These are process **VmHWM** and guest **/proc/meminfo** readings from the default WSL configuration (memory=4GB, processors=4, swap=1GB). They are supplementary measurements, not a 128MB acceptance result or a whole-host peak. WSL failed to create its VM when temporarily capped at 128MB (**CreateVm/0x8007274c**); no Hyper-V, QEMU, VirtualBox, or Docker VM backend was available. The original **.wslconfig** was restored and its SHA-256 verified as **954753E75DECF036D07A087CC64F3E0A66594271F8562C0ED86B4FB9988F6D6B**. Therefore D01 remains **PARTIAL** until the 128MB whole-system test can run in a suitable VM or host.

The smoke validates a local HTTP destination through the real Reality proxy and local counters. It does not establish unrestricted public Internet egress or production-port behavior.
