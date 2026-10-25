[CmdletBinding()]
param(
    [Parameter(Mandatory = $true)][string]$PhoenixKey,
    [Parameter(Mandatory = $true)][string]$PhoenixKnownHosts,
    [Parameter(Mandatory = $true)][string]$IcelandKey,
    [Parameter(Mandatory = $true)][string]$IcelandKnownHosts,
    [Parameter(Mandatory = $true)][string]$XrayAmd64Path,
    [Parameter(Mandatory = $true)][string]$XrayArm64Path,
    [string]$WorkspaceRoot,
    [string]$PhoenixHost = '161.153.88.8',
    [int]$PhoenixSshPort = 22,
    [string]$IcelandHost = '194.247.182.108',
    [int]$IcelandSshPort = 45100,
    [string]$ExitHost = '161.153.88.8',
    [int]$ExitPort = 28444,
    [switch]$KeepRemote
)

$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest

if ([string]::IsNullOrWhiteSpace($WorkspaceRoot)) {
    $WorkspaceRoot = (Resolve-Path (Join-Path $PSScriptRoot '..')).Path
}
$WorkspaceRoot = (Resolve-Path $WorkspaceRoot).Path

function Assert-RegularFile([string]$Path, [string]$Name) {
    $item = Get-Item -LiteralPath $Path -ErrorAction Stop
    if (-not $item.PSIsContainer -and [string]::IsNullOrEmpty($item.LinkType)) {
        return
    }
    throw "$Name 必须是普通文件：$Path"
}

foreach ($item in @(
        @{ Path = $PhoenixKey; Name = 'Phoenix SSH 私钥' },
        @{ Path = $PhoenixKnownHosts; Name = 'Phoenix known_hosts' },
        @{ Path = $IcelandKey; Name = 'Iceland SSH 私钥' },
        @{ Path = $IcelandKnownHosts; Name = 'Iceland known_hosts' },
        @{ Path = $XrayAmd64Path; Name = 'amd64 Xray' },
        @{ Path = $XrayArm64Path; Name = 'arm64 Xray' }
    )) {
    Assert-RegularFile $item.Path $item.Name
}

if ($PhoenixSshPort -lt 1 -or $PhoenixSshPort -gt 65535 -or $IcelandSshPort -lt 1 -or $IcelandSshPort -gt 65535) {
    throw 'SSH 端口无效'
}
if ($ExitPort -lt 1 -or $ExitPort -gt 65535) {
    throw 'Exit 端口无效'
}
foreach ($targetHost in @($PhoenixHost, $IcelandHost, $ExitHost)) {
    if ($targetHost -notmatch '^[A-Za-z0-9.-]+$') {
        throw "主机名包含未允许的字符：$targetHost"
    }
}

$runID = 'd11-' + [DateTime]::UtcNow.ToString('yyyyMMddTHHmmssfffZ') + '-' + $PID
$localRoot = Join-Path $WorkspaceRoot (Join-Path '.work' $runID)
$logRoot = Join-Path $localRoot 'logs'
New-Item -ItemType Directory -Path $logRoot -Force | Out-Null

$entry = @{
    Name = 'iceland-entry'
    User = 'root'
    Host = $IcelandHost
    Port = $IcelandSshPort
    Key = $IcelandKey
    KnownHosts = $IcelandKnownHosts
    ExpectedHostname = 'iceland-gWNzeh'
    RemoteRoot = "/tmp/panel-agent-$runID-entry"
    TestBinary = 'd11-agent-amd64'
    Xray = 'xray-amd64'
}
$exit = @{
    Name = 'phoenix-exit'
    User = 'ubuntu'
    Host = $PhoenixHost
    Port = $PhoenixSshPort
    Key = $PhoenixKey
    KnownHosts = $PhoenixKnownHosts
    ExpectedHostname = 'phoenix-free-vnic'
    RemoteRoot = "/tmp/panel-agent-$runID-exit"
    TestBinary = 'd11-agent-arm64'
    Xray = 'xray-arm64'
}

function ConvertTo-PosixLiteral([string]$Value) {
    if ($Value -match "[^A-Za-z0-9_./:-]") {
        throw "远端命令值包含未允许的字符：$Value"
    }
    return "'" + $Value + "'"
}

function Write-RunLog([string]$Name, [string]$Text) {
    $safe = $Name -replace '[^A-Za-z0-9_.-]', '_'
    Set-Content -LiteralPath (Join-Path $logRoot "$safe.txt") -Value $Text -Encoding UTF8
}

function SshArguments([hashtable]$Side) {
    return @(
        '-F', 'NUL',
        '-i', $Side.Key,
        '-o', 'IdentitiesOnly=yes',
        '-o', 'StrictHostKeyChecking=yes',
        '-o', "UserKnownHostsFile=$($Side.KnownHosts)",
        '-o', 'GlobalKnownHostsFile=none',
        '-o', 'ConnectTimeout=15',
        '-p', [string]$Side.Port
    )
}

function ScpArguments([hashtable]$Side) {
    return @(
        '-F', 'NUL',
        '-i', $Side.Key,
        '-o', 'IdentitiesOnly=yes',
        '-o', 'StrictHostKeyChecking=yes',
        '-o', "UserKnownHostsFile=$($Side.KnownHosts)",
        '-o', 'GlobalKnownHostsFile=none',
        '-o', 'ConnectTimeout=15',
        '-P', [string]$Side.Port
    )
}

function Invoke-Ssh([hashtable]$Side, [string]$Name, [string]$Command, [switch]$AllowFailure) {
    $args = @(SshArguments $Side) + @("$($Side.User)@$($Side.Host)", $Command)
    $output = (& ssh.exe @args 2>&1 | Out-String)
    $code = $LASTEXITCODE
    Write-RunLog $Name $output
    if (-not $AllowFailure -and $code -ne 0) {
        throw "SSH 命令失败（$($Side.Name)，$Name，exit=$code）：$($output.Trim())"
    }
    [pscustomobject]@{ Output = $output.Trim(); ExitCode = $code }
}

function Invoke-Scp([hashtable]$Side, [string]$Name, [string]$LocalPath, [string]$RemoteName) {
    $destination = "$($Side.User)@$($Side.Host):$($Side.RemoteRoot)/$RemoteName"
    $args = @(ScpArguments $Side) + @($LocalPath, $destination)
    $output = (& scp.exe @args 2>&1 | Out-String)
    $code = $LASTEXITCODE
    Write-RunLog $Name $output
    if ($code -ne 0) {
        throw "SCP 上传失败（$($Side.Name)，$Name，exit=$code）：$($output.Trim())"
    }
}

function Receive-Scp([hashtable]$Side, [string]$Name, [string]$RemoteName, [string]$LocalPath) {
    $source = "$($Side.User)@$($Side.Host):$($Side.RemoteRoot)/$RemoteName"
    $args = @(ScpArguments $Side) + @($source, $LocalPath)
    $output = (& scp.exe @args 2>&1 | Out-String)
    $code = $LASTEXITCODE
    Write-RunLog $Name $output
    if ($code -ne 0) {
        throw "SCP 下载失败（$($Side.Name)，$Name，exit=$code）：$($output.Trim())"
    }
}

function Invoke-GoBuild([string]$Goos, [string]$Goarch, [string]$Kind, [string]$OutputPath) {
    $old = @{
        GOOS = $env:GOOS
        GOARCH = $env:GOARCH
        CGO_ENABLED = $env:CGO_ENABLED
    }
    try {
        $env:GOOS = $Goos
        $env:GOARCH = $Goarch
        $env:CGO_ENABLED = '0'
        if ($Kind -eq 'agent') {
            & go test -c -o $OutputPath ./internal/agent
        } else {
            & go build -trimpath -o $OutputPath ./bench/d11-udp-probe
        }
        if ($LASTEXITCODE -ne 0) {
            throw "build failed: $Kind $Goos/$Goarch"
        }
    } finally {
        foreach ($name in $old.Keys) {
            Set-Item -Path ("Env:{0}" -f $name) -Value $old[$name]
        }
    }
}

function Invoke-LocalPlanInit([string]$PlanRoot) {
    $old = @{}
    foreach ($name in @('PANEL_AGENT_D11_ROOT', 'PANEL_AGENT_D11_ACTION', 'PANEL_AGENT_D11_EXIT_PORT', 'PANEL_AGENT_D11_EXIT_HOST', 'PANEL_AGENT_D11_ROLE', 'PANEL_AGENT_TEST_XRAY')) {
        $old[$name] = [Environment]::GetEnvironmentVariable($name)
    }
    try {
        $env:PANEL_AGENT_D11_ROOT = $PlanRoot
        $env:PANEL_AGENT_D11_ACTION = 'init'
        $env:PANEL_AGENT_D11_EXIT_PORT = [string]$ExitPort
        $env:PANEL_AGENT_D11_EXIT_HOST = $ExitHost
        $env:PANEL_AGENT_D11_ROLE = 'entry'
        Remove-Item Env:PANEL_AGENT_TEST_XRAY -ErrorAction SilentlyContinue
        $output = (& go test -count=1 -timeout=60s -run '^TestRealCoreOneHopStage$' ./internal/agent 2>&1 | Out-String)
        $code = $LASTEXITCODE
        Write-RunLog 'local-plan-init' $output
        if ($code -ne 0) {
            throw "生成 D11 plan 失败：$($output.Trim())"
        }
    } finally {
        foreach ($name in $old.Keys) {
            if ($null -eq $old[$name]) {
                Remove-Item -LiteralPath ("Env:{0}" -f $name) -ErrorAction SilentlyContinue
            } else {
                Set-Item -Path ("Env:{0}" -f $name) -Value $old[$name]
            }
        }
    }
}

function Verify-Identity([hashtable]$Side) {
    $command = 'hostname; id -un; uname -m; ip -4 -o addr show'
    $result = Invoke-Ssh $Side "$($Side.Name)-identity" $command
    $lines = $result.Output -split "`r?`n"
    if ($lines -notcontains $Side.ExpectedHostname) {
        throw "$($Side.Name) 主机名与授权记录不匹配：$($result.Output)"
    }
    $expectedUser = [string]$Side.User
    if ($lines -notcontains $expectedUser) {
        throw "$($Side.Name) SSH 用户与授权记录不匹配：$($result.Output)"
    }
    $expectedArch = if ($Side.Name -eq 'phoenix-exit') { 'aarch64' } else { 'x86_64' }
    if ($lines -notcontains $expectedArch) {
        throw "$($Side.Name) 架构与授权记录不匹配：$($result.Output)"
    }
}

function Create-RemoteRoot([hashtable]$Side) {
    if ($Side.RemoteRoot -notmatch '^/tmp/panel-agent-d11-[A-Za-z0-9TZ.-]+-(entry|exit)$') {
        throw "拒绝使用非隔离的远端路径：$($Side.RemoteRoot)"
    }
    $root = ConvertTo-PosixLiteral $Side.RemoteRoot
    Invoke-Ssh $Side "$($Side.Name)-mkdir" "umask 077; mkdir -p $root; chmod 700 $root"
}

function RemotePath([hashtable]$Side, [string]$Name) {
    return (ConvertTo-PosixLiteral "$($Side.RemoteRoot)/$Name")
}

function D11Command([hashtable]$Side, [string]$Action) {
    $vars = @(
        'PANEL_AGENT_D11_ROOT=' + (ConvertTo-PosixLiteral $Side.RemoteRoot),
        'PANEL_AGENT_D11_ACTION=' + (ConvertTo-PosixLiteral $Action),
        'PANEL_AGENT_D11_EXIT_PORT=' + (ConvertTo-PosixLiteral ([string]$ExitPort)),
        'PANEL_AGENT_D11_EXIT_HOST=' + (ConvertTo-PosixLiteral $ExitHost),
        'PANEL_AGENT_D11_ROLE=' + (ConvertTo-PosixLiteral $(if ($Side.Name -eq 'phoenix-exit') { 'exit' } else { 'entry' })),
        'PANEL_AGENT_TEST_XRAY=' + (RemotePath $Side $Side.Xray)
    )
    return 'env ' + ($vars -join ' ') + ' ' + (RemotePath $Side $Side.TestBinary) + " -test.run '^TestRealCoreOneHopStage`$' -test.count=1 -test.v"
}

function Invoke-D11Action([hashtable]$Side, [string]$Action) {
    $result = Invoke-Ssh $Side "$($Side.Name)-$Action" (D11Command $Side $Action)
    if ($result.ExitCode -ne 0) {
        throw "D11 action 失败（$($Side.Name)/$Action）：$($result.Output)"
    }
    return $result
}

function Start-Client([hashtable]$Side) {
    $xray = RemotePath $Side $Side.Xray
    $config = RemotePath $Side 'client.json'
    $pidFile = RemotePath $Side 'client.pid'
    $logFile = RemotePath $Side 'client.log'
    $command = 'set -eu; nohup ' + $xray + ' run -config ' + $config + ' >' + $logFile + ' 2>&1 </dev/null & echo $! >' + $pidFile + '; sleep 1; kill -0 $(cat ' + $pidFile + ')'
    Invoke-Ssh $Side "$($Side.Name)-client-start" $command | Out-Null
}

function Stop-Client([hashtable]$Side) {
    $root = ConvertTo-PosixLiteral $Side.RemoteRoot
    $pidFile = RemotePath $Side 'client.pid'
    $marker = "$($Side.RemoteRoot)/client.json"
    $removeLog = if ($KeepRemote) { '' } else { ' ' + $root + '/client.log' }
    $command = 'set +e; pid=$(cat ' + $pidFile + ' 2>/dev/null); if [ -z "$pid" ]; then exit 0; fi; case "$pid" in *[!0-9]*) exit 0;; esac; if [ -r /proc/$pid/cmdline ]; then cmd=$(cat /proc/$pid/cmdline); case "$cmd" in *' + $marker + '*) kill "$pid" 2>/dev/null || true;; esac; fi; rm -f ' + $pidFile + $removeLog
    Invoke-Ssh $Side "$($Side.Name)-client-stop" $command -AllowFailure | Out-Null
}

function Remove-RemoteRoot([hashtable]$Side) {
    $root = ConvertTo-PosixLiteral $Side.RemoteRoot
    $command = 'case ' + $root + ' in /tmp/panel-agent-d11-*) rm -rf -- ' + $root + ';; *) exit 1;; esac'
    Invoke-Ssh $Side "$($Side.Name)-cleanup" $command -AllowFailure | Out-Null
}

function Invoke-ProxyHTTP([hashtable]$Side, [string]$Name) {
    return Invoke-Ssh $Side $Name "curl --socks5-hostname 127.0.0.1:10818 --fail --silent --show-error --max-time 20 -o /dev/null -w '%{http_code}' https://www.google.com/generate_204"
}

function Assert-ProxyHTTP([hashtable]$Side, [string]$Name) {
    $result = Invoke-ProxyHTTP $Side $Name
    if ($result.Output -notmatch '204') {
        throw "代理 HTTP 验收不是 204（$Name）：$($result.Output)"
    }
}

function Assert-ProxyFailure([hashtable]$Side, [string]$Name) {
    $command = "set +e; curl --socks5-hostname 127.0.0.1:10818 --silent --show-error --max-time 8 -o /dev/null https://www.google.com/generate_204; rc=`$?; printf 'curl_rc=%s\n' `"`$rc`"; test `"`$rc`" -ne 0"
    $result = Invoke-Ssh $Side $Name $command
    if ($result.Output -notmatch 'curl_rc=[1-9][0-9]*') {
        throw "Exit 故障后代理请求未失败：$($result.Output)"
    }
}

function Get-IPv4([hashtable]$Side, [string]$Name, [switch]$ViaProxy) {
    $command = if ($ViaProxy) {
        'curl --socks5-hostname 127.0.0.1:10818 --fail --silent --show-error --max-time 20 https://api.ipify.org'
    } else {
        'curl -4 --fail --silent --show-error --max-time 20 https://api.ipify.org'
    }
    $result = Invoke-Ssh $Side $Name $command
    $match = [regex]::Match($result.Output, '(?<![0-9])(?:[0-9]{1,3}\.){3}[0-9]{1,3}(?![0-9])')
    if (-not $match.Success) {
        throw "未取得 IPv4 出口地址（$Name）：$($result.Output)"
    }
    return $match.Value
}

function Assert-Samples([hashtable]$Side, [string]$Name) {
    $local = Join-Path $localRoot "$Name-sample.json"
    Receive-Scp $Side "$Name-sample" 'sample.json' $local
    $sample = Get-Content -LiteralPath $local -Raw | ConvertFrom-Json
    if ([uint64]$sample.uplink -eq 0 -or [uint64]$sample.downlink -eq 0) {
        throw "$Name 没有双向真实流量：$($sample | ConvertTo-Json -Compress)"
    }
    $expected = if ($Side.Name -eq 'phoenix-exit') { 'machine_only_transit' } else { 'user_entry' }
    $streams = @($sample.ledger.PendingPreview | ForEach-Object { $_.Stream.Kind })
    if ($streams -notcontains $expected) {
        throw "$Name 账本 stream 不是 $expected：$($streams -join ',')"
    }
}

$successful = $false
try {
    $planRoot = Join-Path $localRoot 'plan'
    New-Item -ItemType Directory -Path $planRoot -Force | Out-Null
    $amd64Agent = Join-Path $localRoot 'd11-agent-amd64'
    $arm64Agent = Join-Path $localRoot 'd11-agent-arm64'
    $udpProbe = Join-Path $localRoot 'd11-udp-probe-amd64'
    Invoke-GoBuild 'linux' 'amd64' 'agent' $amd64Agent
    Invoke-GoBuild 'linux' 'arm64' 'agent' $arm64Agent
    Invoke-GoBuild 'linux' 'amd64' 'udp-probe' $udpProbe
    Invoke-LocalPlanInit $planRoot
    Assert-RegularFile (Join-Path $planRoot 'plan.json') 'D11 plan'
    Assert-RegularFile (Join-Path $planRoot 'client.json') 'D11 client config'

    Verify-Identity $exit
    Verify-Identity $entry
    Create-RemoteRoot $exit
    Create-RemoteRoot $entry
    Invoke-Scp $exit 'upload-agent-arm64' $arm64Agent $exit.TestBinary
    Invoke-Scp $entry 'upload-agent-amd64' $amd64Agent $entry.TestBinary
    Invoke-Scp $exit 'upload-xray-arm64' $XrayArm64Path $exit.Xray
    Invoke-Scp $entry 'upload-xray-amd64' $XrayAmd64Path $entry.Xray
    Invoke-Scp $entry 'upload-udp-probe' $udpProbe 'd11-udp-probe'
    Invoke-Scp $exit 'upload-plan' (Join-Path $planRoot 'plan.json') 'plan.json'
    Invoke-Scp $entry 'upload-plan' (Join-Path $planRoot 'plan.json') 'plan.json'
    Invoke-Scp $entry 'upload-client' (Join-Path $planRoot 'client.json') 'client.json'
    Invoke-Ssh $exit 'chmod' ('chmod 700 ' + (RemotePath $exit $exit.TestBinary) + ' ' + (RemotePath $exit $exit.Xray)) | Out-Null
    Invoke-Ssh $entry 'chmod' ('chmod 700 ' + (RemotePath $entry $entry.TestBinary) + ' ' + (RemotePath $entry $entry.Xray) + ' ' + (RemotePath $entry 'd11-udp-probe')) | Out-Null

    Invoke-D11Action $exit 'apply' | Out-Null
    $tcpProbe = Invoke-Ssh $entry 'entry-exit-tcp-preflight' ('nc -zvw 5 ' + $ExitHost + ' ' + [string]$ExitPort) -AllowFailure
    if ($tcpProbe.ExitCode -ne 0) {
        throw "Entry 到 Exit 的真实 TCP $ExitHost`:$ExitPort 不可达：$($tcpProbe.Output)"
    }
    Invoke-D11Action $entry 'apply' | Out-Null
    Start-Client $entry
    Start-Sleep -Seconds 2

    $directIP = Get-IPv4 $exit 'phoenix-direct-ip'
    $proxiedIP = Get-IPv4 $entry 'entry-proxy-ip' -ViaProxy
    if ($directIP -ne $proxiedIP) {
        throw "代理出口 IP 未落在 Phoenix：direct=$directIP proxy=$proxiedIP"
    }
    Assert-ProxyHTTP $entry 'entry-proxy-http'
    $udp = Invoke-Ssh $entry 'entry-proxy-udp' (RemotePath $entry 'd11-udp-probe' + ' -socks 127.0.0.1:10818 -target 1.1.1.1:53 -timeout 20s')
    if ($udp.Output -notmatch 'udp_ok') {
        throw "UDP 代理验收失败：$($udp.Output)"
    }
    Invoke-D11Action $entry 'sample' | Out-Null
    Invoke-D11Action $exit 'sample' | Out-Null
    Assert-Samples $entry 'entry'
    Assert-Samples $exit 'exit'

    Invoke-D11Action $exit 'stop' | Out-Null
    Assert-ProxyFailure $entry 'exit-stop-fail-closed'
    Invoke-D11Action $exit 'apply' | Out-Null
    Assert-ProxyHTTP $entry 'exit-recovery'

    Invoke-D11Action $exit 'hard-stop' | Out-Null
    Assert-ProxyFailure $entry 'exit-budget-fail-closed'
    Invoke-D11Action $exit 'apply' | Out-Null
    Assert-ProxyHTTP $entry 'budget-recovery'

    Invoke-D11Action $entry 'bad-config' | Out-Null
    Assert-ProxyHTTP $entry 'invalid-entry-keeps-healthy-core'

    Invoke-D11Action $exit 'rotate' | Out-Null
    Assert-ProxyFailure $entry 'transit-rotation-mismatch-fail-closed'
    Invoke-D11Action $entry 'rotate' | Out-Null
    Assert-ProxyHTTP $entry 'transit-rotation-recovery'

    $successful = $true
    Write-Output "D11 真实双机 smoke 通过；证据目录：$localRoot"
} finally {
    try { Stop-Client $entry } catch { Write-Warning "清理 entry client 失败：$($_.Exception.Message)" }
    foreach ($side in @($entry, $exit)) {
        try { Invoke-D11Action $side 'stop' | Out-Null } catch { Write-Warning "清理 $($side.Name) Core 失败：$($_.Exception.Message)" }
        if (-not $KeepRemote) {
            try { Remove-RemoteRoot $side } catch { Write-Warning "清理 $($side.Name) 远端目录失败：$($_.Exception.Message)" }
        }
    }
}

if (-not $successful) {
    throw "D11 真实双机 smoke 未完成；保留本地证据目录：$localRoot"
}
