#requires -Version 7.0
param(
    [string]$SshDestination = '',
    [int]$SshPort = 22,
    [string]$SshIdentityFile = '',
    [string]$SshKnownHostsFile = '',
    [string]$ExpectedHostname = '',
    [string]$ExpectedIPv4 = '',
    [switch]$Require128MiB,
    [switch]$DebugXray
)

$ErrorActionPreference = 'Stop'

$xrayVersion = '26.10.21'
$linuxArchiveHash = '0EEC3ED4163C273D8C8BF821F7D219E257921CF584AA3FB970715E458E631978'
$windowsArchiveHash = 'FC58C39EE2F2717B4C54D28D374362389E5319B20AE44AEA76EA45A5243EC827'
$repository = (Resolve-Path (Join-Path $PSScriptRoot '..')).Path
$work = Join-Path $PSScriptRoot '.work'
if (-not (Test-Path -LiteralPath $work)) {
    New-Item -ItemType Directory -Path $work | Out-Null
}
$work = (Resolve-Path -LiteralPath $work).Path
if (-not $work.StartsWith($repository, [StringComparison]::OrdinalIgnoreCase)) {
    throw "Scratch path escaped the repository: $work"
}

$environmentNames = @('GOPATH', 'GOCACHE', 'GOMODCACHE', 'GOWORK', 'GOTOOLCHAIN', 'GOFLAGS', 'GOOS', 'GOARCH', 'CGO_ENABLED')
$originalEnvironment = @{}
foreach ($name in $environmentNames) {
    $originalEnvironment[$name] = [Environment]::GetEnvironmentVariable($name, 'Process')
}
$env:GOPATH = Join-Path $work 'gopath'
$env:GOCACHE = Join-Path $work 'gocache'
$env:GOMODCACHE = Join-Path $work 'gomodcache'
$env:GOWORK = 'off'
$env:GOTOOLCHAIN = 'local'
$env:GOFLAGS = '-mod=readonly'

$runId = [Guid]::NewGuid().ToString('N')
$linuxRunDirectory = "/tmp/panel-agent-d01-$runId"
$startedDebianByUs = $false
$debianWasRunning = $false
$linuxServerPid = $null
$linuxTargetPid = $null
$sshMode = -not [string]::IsNullOrWhiteSpace($SshDestination)
$sshTunnel = $null
$sshTunnelOutput = $null
$sshTunnelError = $null
$linuxPrepared = $false
$smokePassed = $false
if ($sshMode) {
    if ($SshDestination -notmatch '^[a-zA-Z0-9_.@:-]+$' -or $SshPort -lt 1 -or $SshPort -gt 65535 -or $ExpectedHostname -notmatch '^[a-zA-Z0-9_.-]+$') {
        throw 'SSH mode requires a valid destination, port, and expected hostname.'
    }
    $expectedAddress = [Net.IPAddress]::Parse($ExpectedIPv4)
    if ($expectedAddress.AddressFamily -ne [Net.Sockets.AddressFamily]::InterNetwork) { throw 'ExpectedIPv4 must be a literal IPv4 address.' }
    $SshIdentityFile = (Resolve-Path -LiteralPath $SshIdentityFile).Path
    $SshKnownHostsFile = (Resolve-Path -LiteralPath $SshKnownHostsFile).Path
    $sshOptions = @('-F', 'NUL', '-i', $SshIdentityFile, '-o', 'IdentitiesOnly=yes', '-o', 'BatchMode=yes', '-o', 'StrictHostKeyChecking=yes', '-o', "UserKnownHostsFile=$SshKnownHostsFile", '-o', 'ConnectTimeout=15')
}

function Assert-CommandSucceeded([string]$operation) {
    if ($LASTEXITCODE -ne 0) {
        throw "$operation failed with exit code $LASTEXITCODE"
    }
}

function Invoke-LinuxScript([string]$script) {
    $helpers = @'
set -eu
is_listening() {
  hex=$(printf '%04X' "$1")
  awk -v suffix=":$hex" '$4 == "0A" && substr($2, length($2)-4) == suffix { found=1 } END { exit !found }' /proc/net/tcp /proc/net/tcp6
}
process_args() { tr '\000' ' ' < "/proc/$1/cmdline"; }
'@
    $script = ($helpers + "`n" + $script).Replace("`r`n", "`n")
    $encoded = [Convert]::ToBase64String([Text.Encoding]::UTF8.GetBytes($script))
    if ($sshMode) {
        $output = & ssh @sshOptions -p $SshPort $SshDestination "printf '%s' '$encoded' | base64 -d | sh"
    } else {
        $output = & wsl.exe --distribution Debian --exec sh -lc "printf '%s' '$encoded' | base64 -d | sh"
    }
    Assert-CommandSucceeded 'Linux helper'
    return ,$output
}

function Get-LinuxAssetPath([string]$windowsPath) {
    if ($sshMode) { return "$linuxRunDirectory/$([IO.Path]::GetFileName($windowsPath))" }
    return Get-WslPath $windowsPath
}

function Start-SshTunnel {
    $info = [Diagnostics.ProcessStartInfo]::new()
    $info.FileName = $relayPath
    $info.UseShellExecute = $false
    $info.CreateNoWindow = $true
    $info.WindowStyle = [Diagnostics.ProcessWindowStyle]::Hidden
    $info.RedirectStandardOutput = $true
    $info.RedirectStandardError = $true
    foreach ($argument in @('-destination', $SshDestination, '-port', "$SshPort", '-identity', $SshIdentityFile, '-known-hosts', $SshKnownHostsFile)) {
        $info.ArgumentList.Add($argument)
    }
    $script:sshTunnel = [Diagnostics.Process]::new()
    $script:sshTunnel.StartInfo = $info
    if (-not $script:sshTunnel.Start()) { throw 'Could not start SSH tunnel.' }
    $script:sshTunnelOutput = $script:sshTunnel.StandardOutput.ReadToEndAsync()
    $script:sshTunnelError = $script:sshTunnel.StandardError.ReadToEndAsync()
    for ($attempt = 0; $attempt -lt 150; $attempt++) {
        if ($script:sshTunnel.HasExited) { throw "SSH tunnel exited: $($script:sshTunnelError.GetAwaiter().GetResult())" }
        $tcp = [Net.Sockets.TcpClient]::new()
        try { $tcp.Connect('127.0.0.1', 10085); return } catch { Start-Sleep -Milliseconds 100 } finally { $tcp.Dispose() }
    }
    throw 'SSH tunnel did not become ready.'
}

function Get-WslPath([string]$windowsPath) {
    $output = & wsl.exe --distribution Debian --exec wslpath -a $windowsPath
    Assert-CommandSucceeded "Convert path to WSL: $windowsPath"
    return (($output -join [Environment]::NewLine) -replace [string][char]0, '').Trim()
}

function Ensure-XrayBinary([string]$platform, [string]$archiveName, [string]$archiveHash, [string]$binaryName) {
    $archivePath = Join-Path $work $archiveName
    $binaryDirectory = Join-Path $work $platform
    $binaryPath = Join-Path $binaryDirectory $binaryName
    if (-not (Test-Path -LiteralPath $archivePath)) {
        $partialPath = Join-Path $work "$archiveName.$runId.partial"
        $url = "https://github.com/XTLS/Xray-core/releases/download/v$xrayVersion/$archiveName"
        Invoke-WebRequest -Uri $url -OutFile $partialPath
        $downloadHash = (Get-FileHash -LiteralPath $partialPath -Algorithm SHA256).Hash
        if ($downloadHash -ne $archiveHash) {
            throw "Xray $platform archive SHA-256 mismatch: $downloadHash"
        }
        Move-Item -LiteralPath $partialPath -Destination $archivePath
    }
    $existingHash = (Get-FileHash -LiteralPath $archivePath -Algorithm SHA256).Hash
    if ($existingHash -ne $archiveHash) {
        throw "Existing Xray $platform archive has unexpected SHA-256: $existingHash"
    }
    if (-not (Test-Path -LiteralPath $binaryPath)) {
        $staging = Join-Path $work "extract-$platform-$runId"
        New-Item -ItemType Directory -Path $staging | Out-Null
        Expand-Archive -LiteralPath $archivePath -DestinationPath $staging
        $extractedBinary = Join-Path $staging $binaryName
        if (-not (Test-Path -LiteralPath $extractedBinary)) {
            throw "Pinned Xray archive did not contain $binaryName"
        }
        if (-not (Test-Path -LiteralPath $binaryDirectory)) {
            New-Item -ItemType Directory -Path $binaryDirectory | Out-Null
        }
        Move-Item -LiteralPath $extractedBinary -Destination $binaryPath
        Remove-Item -LiteralPath $staging -Recurse -Force
    }
    return (Resolve-Path -LiteralPath $binaryPath).Path
}

function Write-Utf8NoBom([string]$path, [string]$contents) {
    Set-Content -LiteralPath $path -Value $contents -Encoding utf8NoBOM
}

function Get-Counter([string]$name, [switch]$MissingIsZero) {
    $output = & $probePath "-server=127.0.0.1:10085" "-name=$name" 2>&1
    $exitCode = $LASTEXITCODE
    if ($exitCode -ne 0) {
        $message = $output -join [Environment]::NewLine
        if ($MissingIsZero -and $message -match 'xray stat not found:') {
            return 0L
        }
        throw "Stats probe failed for '$name': $message"
    }
    $match = [regex]::Match(($output -join [Environment]::NewLine), '(?m)=([0-9]+)\s*$')
    if (-not $match.Success) {
        throw "Stats probe returned an invalid value for '$name': $($output -join ' ')"
    }
    return [long]$match.Groups[1].Value
}

function Read-CoreSnapshot([int]$processId, [string]$stage) {
    $script = @'
set -eu
pid=@@PID@@
test -r "/proc/$pid/status"
printf 'stage=@@STAGE@@ '
awk '/^VmRSS:/ { printf "rss_kib=%s ", $2 } /^VmHWM:/ { printf "hwm_kib=%s\n", $2 }' "/proc/$pid/status"
awk '/^MemTotal:/ { printf "guest_total_kib=%s ", $2 } /^MemAvailable:/ { printf "guest_available_kib=%s\n", $2 }' /proc/meminfo
if [ -r /sys/fs/cgroup/memory.current ]; then printf 'system_cgroup_bytes='; cat /sys/fs/cgroup/memory.current; fi
if [ -r /sys/fs/cgroup/memory.swap.current ]; then printf 'system_swap_bytes='; cat /sys/fs/cgroup/memory.swap.current; fi
'@
    $script = $script.Replace('@@PID@@', "$processId").Replace('@@STAGE@@', $stage)
    $output = Invoke-LinuxScript $script
    return (($output -join ' ') -replace [string][char]0, '').Trim()
}

function Start-TestCore {
    $script = @'
set -eu
run_dir='@@RUN_DIR@@'
config="$run_dir/server.json"
nohup "$run_dir/xray" run -config "$config" >"$run_dir/xray.log" 2>&1 < /dev/null &
pid=$!
printf '%s\n' "$pid" >"$run_dir/server.pid"
i=0
while [ "$i" -lt 50 ]; do
  if ! kill -0 "$pid" 2>/dev/null; then cat "$run_dir/xray.log" >&2; exit 1; fi
  if is_listening 10085 && is_listening 18443 && is_listening 18444; then break; fi
  i=$((i + 1))
  sleep 0.1
done
if [ "$i" -ge 50 ]; then cat "$run_dir/xray.log" >&2; exit 1; fi
args=$(process_args "$pid")
case "$args" in *"$run_dir/xray run -config $config"*) ;; *) echo "Unexpected Xray command: $args" >&2; exit 1 ;; esac
printf 'pid=%s start=%s\n' "$pid" "$(awk '{ print $22 }' /proc/$pid/stat)"
'@
    $script = $script.Replace('@@RUN_DIR@@', $linuxRunDirectory)
    $output = Invoke-LinuxScript $script
    $text = (($output -join [Environment]::NewLine) -replace [string][char]0, '').Trim()
    $match = [regex]::Match($text, '(?m)^pid=([0-9]+) start=(.+)$')
    if (-not $match.Success) {
        throw "Could not identify the running Xray process: $text"
    }
    return [pscustomobject]@{ Pid = [int]$match.Groups[1].Value; Start = $match.Groups[2].Value }
}

function Stop-TestCore([int]$processId) {
    $script = @'
set -eu
run_dir='@@RUN_DIR@@'
pid='@@PID@@'
if kill -0 "$pid" 2>/dev/null; then
  args=$(process_args "$pid")
  case "$args" in *"$run_dir/xray run -config $run_dir/server.json"*) kill "$pid" ;; *) echo "Refusing to stop unexpected process: $args" >&2; exit 1 ;; esac
  i=0
  while kill -0 "$pid" 2>/dev/null && [ "$i" -lt 50 ]; do i=$((i + 1)); sleep 0.1; done
  if kill -0 "$pid" 2>/dev/null; then kill -KILL "$pid"; fi
fi
i=0
while { is_listening 10085 || is_listening 18443 || is_listening 18444; } && [ "$i" -lt 50 ]; do i=$((i + 1)); sleep 0.1; done
if is_listening 10085 || is_listening 18443 || is_listening 18444; then echo 'Xray listeners did not stop' >&2; exit 1; fi
'@
    $script = $script.Replace('@@RUN_DIR@@', $linuxRunDirectory).Replace('@@PID@@', "$processId")
    Invoke-LinuxScript $script | Out-Null
}

function Start-TestTarget([string]$targetBinaryPath, [string]$guestIp) {
    $linuxTarget = Get-LinuxAssetPath $targetBinaryPath
    $script = @'
set -eu
run_dir='@@RUN_DIR@@'
guest_ip='@@GUEST_IP@@'
if [ '@@TARGET_BINARY@@' != "$run_dir/http-target" ]; then cp '@@TARGET_BINARY@@' "$run_dir/http-target"; fi
chmod 700 "$run_dir/http-target"
nohup "$run_dir/http-target" -listen "$guest_ip:18080" >"$run_dir/target.log" 2>&1 < /dev/null &
pid=$!
printf '%s\n' "$pid" >"$run_dir/target.pid"
i=0
while [ "$i" -lt 50 ]; do
  if ! kill -0 "$pid" 2>/dev/null; then cat "$run_dir/target.log" >&2; exit 1; fi
  if is_listening 18080; then break; fi
  i=$((i + 1)); sleep 0.1
done
if [ "$i" -ge 50 ]; then cat "$run_dir/target.log" >&2; exit 1; fi
printf '%s\n' "$pid"
'@
    $script = $script.Replace('@@RUN_DIR@@', $linuxRunDirectory).Replace('@@GUEST_IP@@', $guestIp).Replace('@@TARGET_BINARY@@', $linuxTarget)
    $output = Invoke-LinuxScript $script
    $text = (($output -join [Environment]::NewLine) -replace [string][char]0, '').Trim()
    if ($text -notmatch '(?m)^\d+$') {
        throw "Could not identify the test target process: $text"
    }
    return [int]$Matches[0]
}

function Invoke-NodeBurst([string]$node, [int]$serverPort, [string]$guestIp, [string]$uuid, [string]$publicKey, [string]$shortId) {
    $clientConfig = @{
        log = @{ loglevel = $(if ($DebugXray) { 'debug' } else { 'warning' }) }
        inbounds = @(@{
            listen = '127.0.0.1'
            port = 10808
            protocol = 'socks'
            settings = @{ auth = 'noauth'; udp = $false }
        })
        outbounds = @(@{
            protocol = 'vless'
            tag = 'proxy'
            settings = @{ vnext = @(@{ address = $guestIp; port = $serverPort; users = @(@{ id = $uuid; encryption = 'none' }) }) }
            streamSettings = @{
                network = 'raw'
                security = 'reality'
                realitySettings = @{ serverName = 'www.cloudflare.com'; fingerprint = 'chrome'; password = $publicKey; shortId = $shortId; spiderX = '/' }
            }
        })
    }
    $clientPath = Join-Path $work "client-$node.json"
    Write-Utf8NoBom $clientPath ($clientConfig | ConvertTo-Json -Depth 16)
    $testOutput = & $windowsXray run -test -config $clientPath 2>&1
    if ($LASTEXITCODE -ne 0) {
        throw ('Xray client config test failed for {0}: {1}' -f $node, ($testOutput -join ' '))
    }

    $stdout = Join-Path $work "client-$node.stdout.log"
    $stderr = Join-Path $work "client-$node.stderr.log"
    $clientArgs = 'run -config "{0}"' -f $clientPath
    $client = Start-Process -FilePath $windowsXray -ArgumentList $clientArgs -WorkingDirectory $work -WindowStyle Hidden -PassThru -RedirectStandardOutput $stdout -RedirectStandardError $stderr
    try {
        $ready = $false
        for ($attempt = 0; $attempt -lt 50; $attempt++) {
            if ($client.HasExited) {
                throw "Xray client for $node exited early: $(Get-Content $stderr -Raw -ErrorAction SilentlyContinue)"
            }
            $tcp = [Net.Sockets.TcpClient]::new()
            try {
                $tcp.Connect('127.0.0.1', 10808)
                $ready = $true
                break
            }
            catch {
                Start-Sleep -Milliseconds 100
            }
            finally {
                $tcp.Dispose()
            }
        }
        if (-not $ready) {
            throw "Xray client for $node did not open its local SOCKS listener."
        }

        $curlProcesses = [Collections.Generic.List[Diagnostics.Process]]::new()
        for ($request = 0; $request -lt 4; $request++) {
            $requestOut = Join-Path $work "curl-$node-$request.stdout.log"
            $requestErr = Join-Path $work "curl-$node-$request.stderr.log"
            $arguments = '--fail --silent --show-error --max-time 30 --noproxy "" --proxy socks5h://127.0.0.1:10808 --output NUL --write-out "http=%{http_code} bytes=%{size_download}" "http://' + $guestIp + ':18080/"'
            $curl = Start-Process -FilePath "$env:SystemRoot\System32\curl.exe" -ArgumentList $arguments -WorkingDirectory $work -WindowStyle Hidden -PassThru -RedirectStandardOutput $requestOut -RedirectStandardError $requestErr
            $curlProcesses.Add($curl)
        }
        for ($request = 0; $request -lt $curlProcesses.Count; $request++) {
            $curlProcesses[$request].WaitForExit()
            if ($curlProcesses[$request].ExitCode -ne 0) {
                $requestErr = Join-Path $work "curl-$node-$request.stderr.log"
                throw "Proxy request for $node failed: $(Get-Content $requestErr -Raw -ErrorAction SilentlyContinue)"
            }
            $requestOut = Join-Path $work "curl-$node-$request.stdout.log"
            $result = Get-Content -LiteralPath $requestOut -Raw
            if ($result -notmatch '^http=200 bytes=18$') {
                throw "Proxy request for $node returned '$result', want http=200 bytes=18"
            }
        }
        Write-Host ('{0}: four parallel HTTP 200 responses, 18 bytes each' -f $node)
    }
    finally {
        if (-not $client.HasExited) {
            Stop-Process -Id $client.Id -Force
            $client.WaitForExit()
        }
    }
}

try {
    $windowsXray = Ensure-XrayBinary 'windows' 'Xray-windows-64.zip' $windowsArchiveHash 'xray.exe'
    $linuxXray = Ensure-XrayBinary 'linux' 'Xray-linux-64.zip' $linuxArchiveHash 'xray'
    $versionOutput = & $windowsXray version 2>&1
    Assert-CommandSucceeded 'Windows Xray version check'
    if (($versionOutput -join ' ') -notmatch "Xray $xrayVersion ") {
        throw "Unexpected Windows Xray version: $($versionOutput -join ' ')"
    }

    if ($sshMode) {
        $identityCheck = @'
[ "$(hostname)" = '@@HOSTNAME@@' ] || { echo 'SSH hostname mismatch' >&2; exit 18; }
ip -4 -o addr show scope global | grep -F '@@IPV4@@/' >/dev/null || { echo 'SSH internal IPv4 mismatch' >&2; exit 18; }
command -v nc >/dev/null || { echo 'SSH smoke requires existing nc' >&2; exit 18; }
printf 'verified_host=%s internal_ipv4=@@IPV4@@ kernel=' "$(hostname)"; uname -r
awk '/^MemTotal:|^SwapTotal:/ { print }' /proc/meminfo
df -k /
for file in /proc/[0-9]*/comm; do if [ -r "$file" ] && [ "$(cat "$file")" = xray ]; then echo 'An existing Xray Core is running' >&2; exit 19; fi; done
'@
        $identityCheck = $identityCheck.Replace('@@HOSTNAME@@', $ExpectedHostname).Replace('@@IPV4@@', $ExpectedIPv4)
        (Invoke-LinuxScript $identityCheck) | ForEach-Object { Write-Host $_ }
        $guestIp = '127.0.0.1'
    } else {
    $distros = ((& wsl.exe --list --quiet) -join [Environment]::NewLine) -replace [string][char]0, ''
    Assert-CommandSucceeded 'List WSL distributions'
    if ($distros -notmatch '(?im)^\s*Debian\s*$') {
        throw 'The smoke runner requires the existing Debian WSL distribution.'
    }
    $runningDistros = ((& wsl.exe --list --running --quiet 2>$null) -join [Environment]::NewLine) -replace [string][char]0, ''
    $debianWasRunning = $runningDistros -match '(?im)^\s*Debian\s*$'
    $startedDebianByUs = -not $debianWasRunning
    & wsl.exe --distribution Debian --exec /bin/true
    Assert-CommandSucceeded 'Start Debian WSL'

    $guestIpText = Invoke-LinuxScript 'hostname -I | cut -d " " -f 1'
    $guestIp = (($guestIpText -join ' ') -replace [string][char]0, '').Trim()
    $guestAddress = [Net.IPAddress]::Parse($guestIp)
    if ($guestAddress.AddressFamily -ne [Net.Sockets.AddressFamily]::InterNetwork) {
        throw "Expected an IPv4 WSL address, got $guestIp"
    }
    }
    if ($Require128MiB) {
        $memoryCheck = @'
total=$(awk '/^MemTotal:/ { print $2 }' /proc/meminfo)
[ "$total" -le 131072 ] && [ "$total" -ge 120000 ] || { echo "Expected 128MiB whole-system RAM, found $total KiB" >&2; exit 20; }
if [ -r /sys/fs/cgroup/memory.max ]; then [ "$(cat /sys/fs/cgroup/memory.max)" = 134217728 ]; grep -Fx '0::/' /proc/1/cgroup >/dev/null; fi
'@
        Invoke-LinuxScript $memoryCheck | Out-Null
    }
    $portCheckScript = @'
for port in 10085 18080 18443 18444; do if is_listening "$port"; then echo "A D01 Linux port is already occupied: $port" >&2; exit 17; fi; done
'@
    $portCheck = Invoke-LinuxScript $portCheckScript
    if (($portCheck -join '').Trim()) {
        throw ($portCheck -join ' ')
    }
    $tcp = [Net.Sockets.TcpListener]::new([Net.IPAddress]::Loopback, 10808)
    try { $tcp.Start() } finally { $tcp.Stop() }
    if ($sshMode) {
        foreach ($port in @(10085,18443,18444)) {
            $tcp = [Net.Sockets.TcpListener]::new([Net.IPAddress]::Loopback, $port)
            try { $tcp.Start() } finally { $tcp.Stop() }
        }
    }

    $env:GOOS = 'linux'
    $env:GOARCH = 'amd64'
    $env:CGO_ENABLED = '0'
    $targetBinary = Join-Path $work 'http-target-linux-amd64'
    & go build -o $targetBinary ./bench/http-target
    Assert-CommandSucceeded 'Build Linux HTTP test target'
    $env:GOOS = 'windows'
    $env:GOARCH = 'amd64'
    $env:CGO_ENABLED = '0'
    $probePath = Join-Path $work 'stats-probe.exe'
    & go build -o $probePath ./cmd/stats-probe
    Assert-CommandSucceeded 'Build Go StatsService probe'
    if ($sshMode) {
        $relayPath = Join-Path $work 'ssh-relay.exe'
        & go build -o $relayPath ./bench/ssh-relay
        Assert-CommandSucceeded 'Build local SSH exec relay'
    }
    foreach ($name in @('GOOS', 'GOARCH', 'CGO_ENABLED')) {
        $value = $originalEnvironment[$name]
        if ($null -eq $value) { Remove-Item "Env:$name" -ErrorAction SilentlyContinue } else { Set-Item "Env:$name" $value }
    }

    $keyOutput = & $windowsXray x25519 2>&1
    Assert-CommandSucceeded 'Generate temporary REALITY key'
    $keyText = $keyOutput -join [Environment]::NewLine
    $privateMatch = [regex]::Match($keyText, '(?m)^PrivateKey:\s*(\S+)\s*$')
    $publicMatch = [regex]::Match($keyText, '(?m)^Password \(PublicKey\):\s*(\S+)\s*$')
    if (-not $privateMatch.Success -or -not $publicMatch.Success) {
        throw "Could not parse REALITY key output: $keyText"
    }
    $privateKey = $privateMatch.Groups[1].Value
    $publicKey = $publicMatch.Groups[1].Value
    $randomBytes = [byte[]]::new(8)
    [Security.Cryptography.RandomNumberGenerator]::Create().GetBytes($randomBytes)
    $shortId = [BitConverter]::ToString($randomBytes).Replace('-', '').ToLowerInvariant()
    $uuid = [Guid]::NewGuid().ToString()
    $serverTemplatePath = Join-Path $PSScriptRoot 'fixtures\d01-server.json.template'
    $serverConfig = Get-Content -LiteralPath $serverTemplatePath -Raw
    $serverConfig = $serverConfig.Replace('{{UUID}}', $uuid)
    $serverConfig = $serverConfig.Replace('{{REALITY_PRIVATE_KEY}}', $privateKey)
    $serverConfig = $serverConfig.Replace('{{SHORT_ID}}', $shortId)
    $serverConfig = $serverConfig.Replace('{{TARGET_IPV4_CIDR}}', "$guestIp/32")
    if ($sshMode) { $serverConfig = $serverConfig.Replace('"listen": "0.0.0.0"', '"listen": "127.0.0.1"') }
    if ($DebugXray) { $serverConfig = $serverConfig.Replace('"loglevel": "warning"', '"loglevel": "debug"') }
    if ($serverConfig.Contains('{{')) { throw 'D01 server template contains an unresolved placeholder.' }
    $null = $serverConfig | ConvertFrom-Json
    $serverConfigPath = Join-Path $work 'server.json'
    Write-Utf8NoBom $serverConfigPath $serverConfig
    $serverTestOutput = & $windowsXray run -test -config $serverConfigPath 2>&1
    if ($LASTEXITCODE -ne 0) {
        throw "Windows Xray server config test failed: $($serverTestOutput -join ' ')"
    }

    if ($sshMode) {
        Invoke-LinuxScript "test ! -e '$linuxRunDirectory'; mkdir -m 700 '$linuxRunDirectory'" | Out-Null
        $linuxPrepared = $true
        $linuxArchivePath = Join-Path $work 'Xray-linux-64.zip'
        & scp -O @sshOptions -P $SshPort $linuxArchivePath $serverConfigPath $targetBinary "${SshDestination}:$linuxRunDirectory/"
        Assert-CommandSucceeded 'Upload isolated D01 assets'
        $unpack = "umask 077; printf '%s\n' '$($linuxArchiveHash.ToLowerInvariant())  $linuxRunDirectory/Xray-linux-64.zip' | sha256sum -c -; unzip -p '$linuxRunDirectory/Xray-linux-64.zip' xray > '$linuxRunDirectory/xray'; rm -- '$linuxRunDirectory/Xray-linux-64.zip'"
        Invoke-LinuxScript $unpack | ForEach-Object { Write-Host $_ }
    }
    $linuxXraySource = Get-LinuxAssetPath $linuxXray
    $serverConfigSource = Get-LinuxAssetPath $serverConfigPath
    $stage = @'
set -eu
run_dir='@@RUN_DIR@@'
if [ ! -d "$run_dir" ]; then mkdir -m 700 "$run_dir"; fi
if [ '@@LINUX_XRAY@@' != "$run_dir/xray" ]; then cp '@@LINUX_XRAY@@' "$run_dir/xray"; fi
chmod 700 "$run_dir/xray"
if [ '@@SERVER_CONFIG@@' != "$run_dir/server.json" ]; then cp '@@SERVER_CONFIG@@' "$run_dir/server.json"; fi
for file in /proc/[0-9]*/comm; do
  if [ -r "$file" ]; then
    name=$(cat "$file")
    case "$name" in filepanel|nginx) pid=${file#/proc/}; pid=${pid%/comm}; printf '%s %s %s\n' "$pid" "$(awk '{ print $22 }' /proc/$pid/stat)" "$name" >> "$run_dir/protected-processes" ;; esac
  fi
done
if [ -r /sys/fs/cgroup/memory.current ]; then
  cat /sys/fs/cgroup/memory.events > "$run_dir/memory.events.before"
  cat > "$run_dir/sample-memory.sh" <<'SAMPLER'
while :; do cat /sys/fs/cgroup/memory.current; sleep 0.05; done
SAMPLER
  nohup sh "$run_dir/sample-memory.sh" > "$run_dir/memory.samples" 2>"$run_dir/memory-sampler.log" < /dev/null &
  printf '%s\n' "$!" > "$run_dir/sampler.pid"
fi
"$run_dir/xray" version | grep -F 'Xray @@XRAY_VERSION@@ '
"$run_dir/xray" run -test -config "$run_dir/server.json" >"$run_dir/config-check.log" 2>&1 &
pid=$!
peak=0
while [ -r "/proc/$pid/status" ]; do
  rss=$(awk '/^VmHWM:/ { print $2 }' "/proc/$pid/status" 2>/dev/null || echo 0)
  if [ -n "$rss" ] && [ "$rss" -gt "$peak" ]; then peak="$rss"; fi
  state=$(awk '/^State:/ { print $2 }' "/proc/$pid/status" 2>/dev/null || echo gone)
  [ "$state" = Z ] && break
  sleep 0.005
done
if wait "$pid"; then status=0; else status=$?; fi
cat "$run_dir/config-check.log"
if [ "$status" -ne 0 ]; then exit "$status"; fi
printf 'config_check_peak_rss_kib=%s\n' "$peak"
'@
    $stage = $stage.Replace('@@RUN_DIR@@', $linuxRunDirectory).Replace('@@LINUX_XRAY@@', $linuxXraySource).Replace('@@SERVER_CONFIG@@', $serverConfigSource).Replace('@@XRAY_VERSION@@', $xrayVersion)
    $stageOutput = Invoke-LinuxScript $stage
    Write-Host 'Linux Core version and config check:'
    $stageOutput | ForEach-Object { Write-Host $_ }

    $linuxTargetPid = Start-TestTarget $targetBinary $guestIp
    $coreBeforeRestart = Start-TestCore
    $linuxServerPid = $coreBeforeRestart.Pid
    if ($sshMode) { Start-SshTunnel }
    Write-Host "core_instance=pid-$($coreBeforeRestart.Pid)-$($coreBeforeRestart.Start)"
    Write-Host (Read-CoreSnapshot $linuxServerPid 'startup')
    Start-Sleep -Seconds 5
    Write-Host (Read-CoreSnapshot $linuxServerPid 'idle-5s')

    $emailA = 'u0001-n01'
    $emailB = 'u0001-n02'
    $upA = "user>>>$emailA>>>traffic>>>uplink"
    $downA = "user>>>$emailA>>>traffic>>>downlink"
    $upB = "user>>>$emailB>>>traffic>>>uplink"
    $downB = "user>>>$emailB>>>traffic>>>downlink"

    Invoke-NodeBurst 'node-a' 18443 $guestIp $uuid $publicKey $shortId
    Write-Host (Read-CoreSnapshot $linuxServerPid 'low-concurrency-node-a')
    $aUp = Get-Counter $upA
    $aDown = Get-Counter $downA
    if ($aUp -le 0 -or $aDown -le 0) { throw "Node A counters are not positive: $aUp/$aDown" }

    Invoke-NodeBurst 'node-b' 18444 $guestIp $uuid $publicKey $shortId
    Write-Host (Read-CoreSnapshot $linuxServerPid 'low-concurrency-node-b')
    $bUp = Get-Counter $upB
    $bDown = Get-Counter $downB
    if ($bUp -le 0 -or $bDown -le 0) { throw "Node B counters are not positive: $bUp/$bDown" }
    if ((Get-Counter $upA) -ne $aUp -or (Get-Counter $downA) -ne $aDown) {
        throw 'Node B traffic changed the Node A counters.'
    }

    Invoke-NodeBurst 'node-a' 18443 $guestIp $uuid $publicKey $shortId
    $aUpAgain = Get-Counter $upA
    $aDownAgain = Get-Counter $downA
    if ($aUpAgain -le $aUp -or $aDownAgain -le $aDown) { throw 'Node A traffic did not increase its counters.' }
    if ((Get-Counter $upB) -ne $bUp -or (Get-Counter $downB) -ne $bDown) {
        throw 'Node A traffic changed the Node B counters.'
    }

    Invoke-NodeBurst 'node-b' 18444 $guestIp $uuid $publicKey $shortId
    $bUpAgain = Get-Counter $upB
    $bDownAgain = Get-Counter $downB
    if ($bUpAgain -le $bUp -or $bDownAgain -le $bDown) { throw 'Node B traffic did not increase its counters.' }
    if ((Get-Counter $upA) -ne $aUpAgain -or (Get-Counter $downA) -ne $aDownAgain) {
        throw 'Node B traffic changed the Node A counters.'
    }
    Write-Host "same_uuid_across_nodes=true node_a=$aUpAgain/$aDownAgain node_b=$bUpAgain/$bDownAgain"
    Write-Host "repeated_read_node_a=$((Get-Counter $upA))/$((Get-Counter $downA)); node_b=$((Get-Counter $upB))/$((Get-Counter $downB))"

    Stop-TestCore $linuxServerPid
    $coreAfterRestart = Start-TestCore
    $linuxServerPid = $coreAfterRestart.Pid
    if ($coreAfterRestart.Pid -eq $coreBeforeRestart.Pid -and $coreAfterRestart.Start -eq $coreBeforeRestart.Start) {
        throw 'Xray process identity did not change across restart.'
    }
    $restartAUp = Get-Counter $upA -MissingIsZero
    $restartADown = Get-Counter $downA -MissingIsZero
    $restartBUp = Get-Counter $upB -MissingIsZero
    $restartBDown = Get-Counter $downB -MissingIsZero
    if ($restartAUp -ne 0 -or $restartADown -ne 0 -or $restartBUp -ne 0 -or $restartBDown -ne 0) {
        throw "Counters survived a full Core restart unexpectedly: node-a=$restartAUp/$restartADown node-b=$restartBUp/$restartBDown"
    }
    Write-Host "core_restart=pid-$($coreBeforeRestart.Pid)-$($coreBeforeRestart.Start) -> pid-$($coreAfterRestart.Pid)-$($coreAfterRestart.Start); counters=0/0,0/0"
    $resourceCheck = @'
run_dir='@@RUN_DIR@@'
if [ -f "$run_dir/memory.events.before" ]; then
  cat /sys/fs/cgroup/memory.events > "$run_dir/memory.events.after"
  awk 'NR==FNR { before[$1]=$2; next } ($1=="oom" || $1=="oom_kill" || $1=="oom_group_kill") { printf "%s_before=%s after=%s\n", $1, before[$1], $2; if ($2!=before[$1]) bad=1 } END { exit bad }' "$run_dir/memory.events.before" "$run_dir/memory.events.after"
  awk 'BEGIN { peak=0 } $1>peak { peak=$1 } END { printf "sampled_system_peak_bytes=%s samples=%s\n", peak, NR; if (NR<2) exit 1 }' "$run_dir/memory.samples"
  printf 'swap_current_bytes='; cat /sys/fs/cgroup/memory.swap.current
fi
if [ -f "$run_dir/protected-processes" ]; then
  while read -r pid start name; do [ -r "/proc/$pid/stat" ] && [ "$(awk '{ print $22 }' /proc/$pid/stat)" = "$start" ] || { echo "Existing $name process changed: $pid" >&2; exit 21; }; done < "$run_dir/protected-processes"
  printf 'existing_filepanel_nginx_processes_unchanged=true\n'
fi
'@
    (Invoke-LinuxScript ($resourceCheck.Replace('@@RUN_DIR@@', $linuxRunDirectory))) | ForEach-Object { Write-Host $_ }
    $smokePassed = $true
    Write-Host 'D01 REALITY + separate cumulative StatsService counters smoke: PASS'
}
finally {
    if ($null -ne $sshTunnel) {
        if (-not $sshTunnel.HasExited) { $sshTunnel.Kill(); $sshTunnel.WaitForExit() }
        $sshTunnelOutput.GetAwaiter().GetResult() | Out-Null
        Write-Utf8NoBom (Join-Path $work "ssh-tunnel-$runId.log") $sshTunnelError.GetAwaiter().GetResult()
        $sshTunnel.Dispose()
    }
    if ($linuxPrepared -or $debianWasRunning -or $startedDebianByUs) {
        if (-not $smokePassed) {
            $diagnostic = @'
run_dir='@@RUN_DIR@@'
for file in xray.log target.log config-check.log; do if [ -f "$run_dir/$file" ]; then printf '\n--- %s ---\n' "$file"; tail -n 120 "$run_dir/$file"; fi; done
if [ -r /sys/fs/cgroup/memory.events ]; then cat /sys/fs/cgroup/memory.events; fi
'@
            $diagnosticOutput = Invoke-LinuxScript ($diagnostic.Replace('@@RUN_DIR@@', $linuxRunDirectory))
            $diagnosticPath = Join-Path $work "d01-$runId-remote.log"
            Write-Utf8NoBom $diagnosticPath ($diagnosticOutput -join "`n")
            Write-Warning "Remote diagnostic saved at $diagnosticPath"
        }
        $cleanup = @'
set -eu
run_dir='@@RUN_DIR@@'
case "$run_dir" in /tmp/panel-agent-d01-????????????????????????????????) ;; *) echo 'Unsafe cleanup directory' >&2; exit 22 ;; esac
[ ! -L "$run_dir" ] || { echo 'Refusing symlink cleanup' >&2; exit 22; }
if [ -d "$run_dir" ]; then
  for file in server.pid target.pid sampler.pid; do
    if [ -f "$run_dir/$file" ]; then
      pid=$(cat "$run_dir/$file")
      case "$pid" in ''|*[!0-9]*) echo "Invalid PID in $run_dir/$file" >&2; exit 1 ;; esac
      if kill -0 "$pid" 2>/dev/null; then
        args=$(process_args "$pid")
        case "$file:$args" in
          "server.pid:$run_dir/xray run -config $run_dir/server.json"*) kill "$pid" ;;
          "target.pid:$run_dir/http-target -listen "*) kill "$pid" ;;
          "sampler.pid:sh $run_dir/sample-memory.sh"*) kill "$pid" ;;
          *) echo "Refusing to stop unexpected process: $args" >&2; exit 1 ;;
        esac
      fi
    fi
  done
  sleep 0.2
  rm -rf -- "$run_dir"
fi
'@
        $cleanup = $cleanup.Replace('@@RUN_DIR@@', $linuxRunDirectory)
        Invoke-LinuxScript $cleanup | Out-Null
    }
    if ($startedDebianByUs) {
        & wsl.exe --terminate Debian 2>$null
    }
    foreach ($name in $environmentNames) {
        $value = $originalEnvironment[$name]
        if ($null -eq $value) { Remove-Item "Env:$name" -ErrorAction SilentlyContinue } else { Set-Item "Env:$name" $value }
    }
}
