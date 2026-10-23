#requires -Version 7.0
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

function Assert-CommandSucceeded([string]$operation) {
    if ($LASTEXITCODE -ne 0) {
        throw "$operation failed with exit code $LASTEXITCODE"
    }
}

function Invoke-WslScript([string]$script) {
    $encoded = [Convert]::ToBase64String([Text.Encoding]::UTF8.GetBytes($script))
    $output = & wsl.exe --distribution Debian --exec sh -lc "printf '%s' '$encoded' | base64 -d | sh"
    Assert-CommandSucceeded 'WSL helper'
    return ,$output
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
'@
    $script = $script.Replace('@@PID@@', "$processId").Replace('@@STAGE@@', $stage)
    $output = Invoke-WslScript $script
    return (($output -join ' ') -replace [string][char]0, '').Trim()
}

function Start-TestCore {
    $script = @'
set -eu
run_dir='@@RUN_DIR@@'
config="$run_dir/server.json"
is_listening() {
  ss -H -ltn | awk -v port=":$1" '$4 ~ (port "$") { found=1 } END { exit !found }'
}
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
args=$(ps -o args= -p "$pid")
case "$args" in *"$run_dir/xray run -config $config"*) ;; *) echo "Unexpected Xray command: $args" >&2; exit 1 ;; esac
printf 'pid=%s start=%s\n' "$pid" "$(ps -o lstart= -p "$pid" | xargs)"
'@
    $script = $script.Replace('@@RUN_DIR@@', $linuxRunDirectory)
    $output = Invoke-WslScript $script
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
is_listening() {
  ss -H -ltn | awk -v port=":$1" '$4 ~ (port "$") { found=1 } END { exit !found }'
}
if kill -0 "$pid" 2>/dev/null; then
  args=$(ps -o args= -p "$pid")
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
    Invoke-WslScript $script | Out-Null
}

function Start-TestTarget([string]$targetBinaryPath, [string]$guestIp) {
    $linuxTarget = Get-WslPath $targetBinaryPath
    $script = @'
set -eu
run_dir='@@RUN_DIR@@'
guest_ip='@@GUEST_IP@@'
cp '@@TARGET_BINARY@@' "$run_dir/http-target"
chmod 700 "$run_dir/http-target"
nohup "$run_dir/http-target" -listen "$guest_ip:18080" >"$run_dir/target.log" 2>&1 < /dev/null &
pid=$!
printf '%s\n' "$pid" >"$run_dir/target.pid"
i=0
while [ "$i" -lt 50 ]; do
  if ! kill -0 "$pid" 2>/dev/null; then cat "$run_dir/target.log" >&2; exit 1; fi
  if ss -H -ltn | grep -Fq "$guest_ip:18080"; then break; fi
  i=$((i + 1)); sleep 0.1
done
if [ "$i" -ge 50 ]; then cat "$run_dir/target.log" >&2; exit 1; fi
printf '%s\n' "$pid"
'@
    $script = $script.Replace('@@RUN_DIR@@', $linuxRunDirectory).Replace('@@GUEST_IP@@', $guestIp).Replace('@@TARGET_BINARY@@', $linuxTarget)
    $output = Invoke-WslScript $script
    $text = (($output -join [Environment]::NewLine) -replace [string][char]0, '').Trim()
    if ($text -notmatch '(?m)^\d+$') {
        throw "Could not identify the test target process: $text"
    }
    return [int]$Matches[0]
}

function Invoke-NodeBurst([string]$node, [int]$serverPort, [string]$guestIp, [string]$uuid, [string]$publicKey, [string]$shortId) {
    $clientConfig = @{
        log = @{ loglevel = 'warning' }
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
            $arguments = '--fail --silent --show-error --max-time 30 --proxy socks5h://127.0.0.1:10808 --output NUL --write-out "http=%{http_code} bytes=%{size_download}" "http://' + $guestIp + ':18080/"'
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

    $guestIpText = Invoke-WslScript 'hostname -I | cut -d " " -f 1'
    $guestIp = (($guestIpText -join ' ') -replace [string][char]0, '').Trim()
    $guestAddress = [Net.IPAddress]::Parse($guestIp)
    if ($guestAddress.AddressFamily -ne [Net.Sockets.AddressFamily]::InterNetwork) {
        throw "Expected an IPv4 WSL address, got $guestIp"
    }
    $portCheckScript = @'
if ss -H -ltn | awk '{print $4}' | grep -Eq ':(10085|18080|18443|18444)$'; then echo 'A D01 WSL port is already occupied'; exit 17; fi
'@
    $portCheck = Invoke-WslScript $portCheckScript
    if (($portCheck -join '').Trim()) {
        throw ($portCheck -join ' ')
    }
    $tcp = [Net.Sockets.TcpListener]::new([Net.IPAddress]::Loopback, 10808)
    try { $tcp.Start() } finally { $tcp.Stop() }

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
    $serverConfig = $serverConfig.Replace('{{WSL_IPV4_CIDR}}', "$guestIp/32")
    if ($serverConfig.Contains('{{')) { throw 'D01 server template contains an unresolved placeholder.' }
    $null = $serverConfig | ConvertFrom-Json
    $serverConfigPath = Join-Path $work 'server.json'
    Write-Utf8NoBom $serverConfigPath $serverConfig
    $serverTestOutput = & $windowsXray run -test -config $serverConfigPath 2>&1
    if ($LASTEXITCODE -ne 0) {
        throw "Windows Xray server config test failed: $($serverTestOutput -join ' ')"
    }

    $linuxXrayOnWsl = Get-WslPath $linuxXray
    $serverConfigOnWsl = Get-WslPath $serverConfigPath
    $targetBinaryOnWsl = Get-WslPath $targetBinary
    $stage = @'
set -eu
run_dir='@@RUN_DIR@@'
mkdir -m 700 "$run_dir"
cp '@@LINUX_XRAY@@' "$run_dir/xray"
chmod 700 "$run_dir/xray"
cp '@@SERVER_CONFIG@@' "$run_dir/server.json"
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
    $stage = $stage.Replace('@@RUN_DIR@@', $linuxRunDirectory).Replace('@@LINUX_XRAY@@', $linuxXrayOnWsl).Replace('@@SERVER_CONFIG@@', $serverConfigOnWsl).Replace('@@XRAY_VERSION@@', $xrayVersion)
    $stageOutput = Invoke-WslScript $stage
    Write-Host 'Linux Core version and config check:'
    $stageOutput | ForEach-Object { Write-Host $_ }

    $linuxTargetPid = Start-TestTarget $targetBinary $guestIp
    $coreBeforeRestart = Start-TestCore
    $linuxServerPid = $coreBeforeRestart.Pid
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
    Write-Host "same_uuid=$uuid node_a=$aUpAgain/$aDownAgain node_b=$bUpAgain/$bDownAgain"
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
    Write-Host 'D01 REALITY + separate cumulative StatsService counters smoke: PASS'
}
finally {
    if ($debianWasRunning -or $startedDebianByUs) {
        $cleanup = @'
set -eu
run_dir='@@RUN_DIR@@'
if [ -d "$run_dir" ]; then
  for file in server.pid target.pid; do
    if [ -f "$run_dir/$file" ]; then
      pid=$(cat "$run_dir/$file")
      case "$pid" in ''|*[!0-9]*) echo "Invalid PID in $run_dir/$file" >&2; exit 1 ;; esac
      if kill -0 "$pid" 2>/dev/null; then
        args=$(ps -o args= -p "$pid")
        case "$file:$args" in
          "server.pid:$run_dir/xray run -config $run_dir/server.json"*) kill "$pid" ;;
          "target.pid:$run_dir/http-target -listen "*) kill "$pid" ;;
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
        try { Invoke-WslScript $cleanup | Out-Null } catch { Write-Warning "WSL process cleanup needs attention: $_" }
    }
    if ($startedDebianByUs) {
        & wsl.exe --terminate Debian 2>$null
    }
    foreach ($name in $environmentNames) {
        $value = $originalEnvironment[$name]
        if ($null -eq $value) { Remove-Item "Env:$name" -ErrorAction SilentlyContinue } else { Set-Item "Env:$name" $value }
    }
}
