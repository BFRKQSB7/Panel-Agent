#requires -Version 7.0
param(
    [Parameter(Mandatory)][string]$SshDestination,
    [int]$SshPort = 45000,
    [Parameter(Mandatory)][string]$SshIdentityFile,
    [Parameter(Mandatory)][string]$SshKnownHostsFile,
    [Parameter(Mandatory)][string]$ExpectedHostname,
    [Parameter(Mandatory)][string]$ExpectedIPv4,
    [string]$PreparedRemoteDirectory = ''
)
$ErrorActionPreference = 'Stop'
$repository = [IO.Path]::GetFullPath((Join-Path $PSScriptRoot '..'))
$work = Join-Path $PSScriptRoot '.work'
$runId = [guid]::NewGuid().ToString('N')
$local = Join-Path $work "d04-$runId"
New-Item -ItemType Directory -Force -Path $local | Out-Null
$remote = if ($PreparedRemoteDirectory) { $PreparedRemoteDirectory } else { "/tmp/panel-agent-d04-$runId" }
if ($remote -notmatch '^/tmp/panel-agent-d04-[a-f0-9]{32}$' -or $ExpectedHostname -notmatch '^[A-Za-z0-9_.-]+$') { throw 'Invalid isolated directory or hostname.' }
[void][Net.IPAddress]::Parse($ExpectedIPv4)
$sshOptions = @('-F','NUL','-i',(Resolve-Path -LiteralPath $SshIdentityFile).Path,'-o','IdentitiesOnly=yes','-o','BatchMode=yes','-o','StrictHostKeyChecking=yes','-o',"UserKnownHostsFile=$((Resolve-Path -LiteralPath $SshKnownHostsFile).Path)",'-o','ConnectTimeout=15')
$processes = [Collections.Generic.List[object]]::new()
$http = $null
$prepared = $false
$result = [ordered]@{ task='D04'; host=$ExpectedHostname; passed=$false; checks=[ordered]@{} }

function Remote([string]$Code, [switch]$AllowFailure) {
    $script = ("set -eu`n" + $Code).Replace("`r`n","`n")
    $encoded = [Convert]::ToBase64String([Text.Encoding]::UTF8.GetBytes($script))
    $output = & ssh @sshOptions -p $SshPort $SshDestination "printf '%s' '$encoded' | base64 -d | sh" 2>&1
    $code = $LASTEXITCODE
    if ($code -ne 0 -and -not $AllowFailure) { throw "Isolated remote command failed ($code): $($output -join ' ')" }
    return [pscustomobject]@{ Code=$code; Text=($output -join "`n") }
}
function Upload([string[]]$Paths) {
    & scp -O @sshOptions -P $SshPort @Paths "${SshDestination}:$remote/"
    if ($LASTEXITCODE -ne 0) { throw 'Upload failed.' }
}
function Start-Local([string]$Binary, [string[]]$Arguments) {
    $info = [Diagnostics.ProcessStartInfo]::new()
    $info.FileName=$Binary; $info.UseShellExecute=$false; $info.CreateNoWindow=$true
    $info.WindowStyle=[Diagnostics.ProcessWindowStyle]::Hidden
    $info.RedirectStandardOutput=$true; $info.RedirectStandardError=$true
    foreach ($argument in $Arguments) { $info.ArgumentList.Add($argument) }
    $p=[Diagnostics.Process]::new(); $p.StartInfo=$info
    if (-not $p.Start()) { throw 'Local helper failed to start.' }
    $entry=[pscustomobject]@{ Process=$p; Out=$p.StandardOutput.ReadToEndAsync(); Err=$p.StandardError.ReadToEndAsync() }
    $processes.Add($entry)
    return $entry
}
function Stop-Local($Entry) {
    if ($null -ne $Entry -and -not $Entry.Process.HasExited) { $Entry.Process.Kill($true); [void]$Entry.Process.WaitForExit(10000) }
}
function Request([string]$Method, [string]$Path, [string]$Body='', [string]$Csrf='') {
    $req=[Net.Http.HttpRequestMessage]::new([Net.Http.HttpMethod]::new($Method),"https://127.0.0.1:28443$Path")
	$req.Headers.ConnectionClose=$true
    if ($Body) { $req.Content=[Net.Http.StringContent]::new($Body,[Text.Encoding]::UTF8,'application/x-www-form-urlencoded') }
    if ($Method -ne 'GET') { [void]$req.Headers.TryAddWithoutValidation('Origin','https://127.0.0.1:28443') }
    if ($Csrf) { [void]$req.Headers.TryAddWithoutValidation('X-CSRF-Token',$Csrf) }
    try {
        $res=$http.SendAsync($req).GetAwaiter().GetResult()
        try { return [pscustomobject]@{ Status=[int]$res.StatusCode; Body=$res.Content.ReadAsStringAsync().GetAwaiter().GetResult(); Location=[string]$res.Headers.Location } }
        finally { $res.Dispose() }
    } finally { $req.Dispose() }
}
function Form($Values) {
    return (($Values.Keys | ForEach-Object { [Net.WebUtility]::UrlEncode($_)+'='+[Net.WebUtility]::UrlEncode([string]$Values[$_]) }) -join '&')
}
function Csrf([string]$Path) {
    $page=Request GET $Path
    $match=[regex]::Match($page.Body,'name="csrf" value="([^"]+)"')
    if ($page.Status -ne 200 -or -not $match.Success) { throw 'Missing CSRF page.' }
    return $match.Groups[1].Value
}
function Start-Panel {
    [void](Remote "cd '$remote'; PANEL_TLS_CERT='$remote/panel-cert.pem' PANEL_TLS_KEY='$remote/panel-key.pem' nohup '$remote/d04-control' serve --data-dir '$remote/panel' --listen 127.0.0.1:28443 > panel.log 2>&1 < /dev/null & echo `$! > '$remote/panel.pid'")
    $deadline=[DateTime]::UtcNow.AddSeconds(20)
    while ([DateTime]::UtcNow -lt $deadline) { try { if ((Request GET '/healthz').Status -eq 200) { return } } catch {}; Start-Sleep -Milliseconds 200 }
    throw 'Panel readiness failed.'
}
function Login-Admin {
    $loginCsrf=Csrf '/login'
    if ((Request POST '/login' (Form @{csrf=$loginCsrf;password=$password})).Status -ne 303) { throw 'Admin login failed.' }
    $script:adminCsrf=Csrf '/admin/machines'
}
function Stop-Owned([string]$PidFile, [string]$Expected) {
    [void](Remote "pid=`$(cat '$remote/$PidFile'); test -r /proc/`$pid/cmdline; tr '\000' ' ' < /proc/`$pid/cmdline | grep -F -- '$Expected' >/dev/null; kill `$pid")
}
function Core-State { return ((Remote "cat '$remote/agent/core/state.json'").Text | ConvertFrom-Json) }
function Sync-Agent([switch]$AllowFailure) {
    return Remote "'$remote/d04-agent' sync --data-dir '$remote/agent' --ca-cert '$remote/panel-cert.pem' --xray-path '$remote/xray' --development-loopback-test" -AllowFailure:$AllowFailure
}
function Node-Form($Node, [string]$Name=$Node.name, [int]$ListenPort=$Node.listen_port, [bool]$Enabled=$true) {
    $values=@{ csrf=$script:adminCsrf; name=$Name; public_host=$Node.public_host; public_port=$Node.public_port; listen_host=$Node.listen_host; listen_port=$ListenPort; server_name=$Node.server_name; target=$Node.target; development='true' }
    if ($Enabled) { $values.enabled='true' }
    $response=Request POST "/admin/machines/$script:machineID/nodes/$($Node.node_id)" (Form $values)
    if ($response.Status -ne 303 -or $response.Location -ne "/admin/machines/$script:machineID/nodes") { throw "Node form edit returned $($response.Status) or redirected outside its Node page." }
}
function Proxy-Get([int]$Port) {
    $body=& curl.exe --fail --silent --show-error --max-time 15 --noproxy '' --proxy "socks5h://127.0.0.1:$Port" http://127.0.0.1:18080/
    if ($LASTEXITCODE -ne 0 -or ($body -join '') -ne 'Panel-Agent-D01-OK') { throw "Reality request through SOCKS $Port failed." }
}
function Wait-Probe([string]$Path, [bool]$Closed) {
    $deadline=[DateTime]::UtcNow.AddSeconds(15)
    while ([DateTime]::UtcNow -lt $deadline) {
        if (Test-Path -LiteralPath $Path) {
            $o=Get-Content -LiteralPath $Path -Raw | ConvertFrom-Json
            if ($Closed -and $o.tcp_closed -and $o.udp_closed) { return $o }
            if (-not $Closed -and $o.tcp_packets -gt 2 -and $o.udp_packets -gt 2 -and -not $o.tcp_closed -and -not $o.udp_closed) { return $o }
        }
        Start-Sleep -Milliseconds 150
    }
    throw "Persistent session observation did not reach expected state (closed=$Closed)."
}

$envNames=@('GOPATH','GOCACHE','GOMODCACHE','GOOS','GOARCH','CGO_ENABLED')
$oldEnv=@{}
foreach ($name in $envNames) { $oldEnv[$name]=[Environment]::GetEnvironmentVariable($name,'Process') }
try {
    $env:GOPATH=Join-Path $work 'gopath'; $env:GOCACHE=Join-Path $work 'gocache'; $env:GOMODCACHE=Join-Path $work 'gomodcache'
    $env:GOOS='windows'; $env:GOARCH='amd64'; $env:CGO_ENABLED='0'
    Push-Location $repository
    try {
        & go build -o "$local/ssh-relay.exe" ./bench/ssh-relay
        if ($LASTEXITCODE -ne 0) { throw 'Relay build failed.' }
        & go build -o "$local/session-probe.exe" ./bench/session-probe
        if ($LASTEXITCODE -ne 0) { throw 'Session probe build failed.' }
        if (-not $PreparedRemoteDirectory) {
            $env:GOOS='linux'
            & go test -c -ldflags '-s -w' -o "$local/d04-core-tests" ./internal/agent
            if ($LASTEXITCODE -ne 0) { throw 'Linux test build failed.' }
            foreach ($target in @(@('d04-control','./cmd/control'),@('d04-agent','./cmd/agent'),@('d04-http-target','./bench/http-target'))) {
                & go build -ldflags '-s -w' -o "$local/$($target[0])" $target[1]
                if ($LASTEXITCODE -ne 0) { throw 'Linux binary build failed.' }
            }
        }
    } finally { Pop-Location }
    $linuxArchive=Join-Path $work 'Xray-linux-64.zip'
    $windowsArchive=Join-Path $work 'Xray-windows-64.zip'
    if ((Get-FileHash -LiteralPath $linuxArchive -Algorithm SHA256).Hash -ne '0EEC3ED4163C273D8C8BF821F7D219E257921CF584AA3FB970715E458E631978' -or (Get-FileHash -LiteralPath $windowsArchive -Algorithm SHA256).Hash -ne 'FC58C39EE2F2717B4C54D28D374362389E5319B20AE44AEA76EA45A5243EC827') { throw 'Pinned archive hash mismatch; run D01 to populate the verified cache.' }
    $baseline=(Remote "test `"`$(hostname)`" = '$ExpectedHostname'; ip -4 addr show | grep -F '$ExpectedIPv4/' >/dev/null; cat /sys/fs/cgroup/memory.max; cat /sys/fs/cgroup/memory.events; netstat -lnt; ps -o pid,args | awk '/filepanel|nginx/ && !/awk/ {print}'").Text
    [IO.File]::WriteAllText("$local/baseline.txt",$baseline)
    if ($baseline -notmatch '^134217728') { throw 'Expected the authorized 128MiB test container.' }
    if (-not $PreparedRemoteDirectory) {
        [void](Remote "mkdir -m 700 '$remote'")
        & tar.exe -czf "$local/bundle.tar.gz" -C $local d04-core-tests d04-control d04-agent d04-http-target
        if ($LASTEXITCODE -ne 0) { throw 'Benchmark bundle failed.' }
        Upload @("$local/bundle.tar.gz",$linuxArchive)
        [void](Remote "cd '$remote'; tar -xzf bundle.tar.gz")
    }
    [void](Remote "test ! -e '$remote/panel'; printf '%s' '$remote' > '$remote/.owner'; chmod 600 '$remote/.owner'; cd '$remote'; test `"`$(sha256sum Xray-linux-64.zip | cut -d ' ' -f1)`" = 0eec3ed4163c273d8c8bf821f7d219e257921cf584aa3fb970715e458e631978; unzip -q -o Xray-linux-64.zip xray; chmod 700 d04-* xray")
    $prepared=$true
    $monitor=@'
cd '__REMOTE__'
cat > monitor.sh <<'MONITOR'
while test ! -e monitor.stop; do
  printf "mem "; cat /sys/fs/cgroup/memory.current
  for pid in $(pidof xray 2>/dev/null || true); do
    if test -r /proc/$pid/cmdline; then
      args=$(tr "\000" " " < /proc/$pid/cmdline)
      case "$args" in *__REMOTE__/agent/core/*)
        kind=run; case "$args" in *" -test "*) kind=check;; esac
        awk -v kind="$kind" '/^VmHWM:/ {print kind, $2}' /proc/$pid/status 2>/dev/null || true;;
      esac
    fi
  done
  sleep 0.01
done
MONITOR
nohup sh '__REMOTE__/monitor.sh' > resource.tsv 2>/dev/null < /dev/null &
echo $! > monitor.pid
'@
    [void](Remote $monitor.Replace('__REMOTE__',$remote))
    $test=Remote "cd '$remote'; TMPDIR='$remote' PANEL_AGENT_TEST_XRAY='$remote/xray' ./d04-core-tests -test.run '^TestRealCoreApplyAndCrashRecovery$' -test.v -test.timeout 90s"
    [IO.File]::WriteAllText("$local/core-tests.txt",$test.Text)
    $result.checks.real_core_fault_suite='PASS'
    Write-Output 'D04: pinned real Core fault/recovery suite passed.'

    $password=[guid]::NewGuid().ToString('N')+'Aa7!'
    [void](Remote "printf '%s\n%s\n' '$password' '$password' | '$remote/d04-control' admin init --data-dir '$remote/panel' --password-stdin")
    $rsa=[Security.Cryptography.RSA]::Create(2048)
    $certRequest=[Security.Cryptography.X509Certificates.CertificateRequest]::new('CN=127.0.0.1',$rsa,[Security.Cryptography.HashAlgorithmName]::SHA256,[Security.Cryptography.RSASignaturePadding]::Pkcs1)
    $san=[Security.Cryptography.X509Certificates.SubjectAlternativeNameBuilder]::new(); $san.AddIpAddress([Net.IPAddress]::Loopback); $certRequest.CertificateExtensions.Add($san.Build())
    $certificate=$certRequest.CreateSelfSigned([DateTimeOffset]::UtcNow.AddMinutes(-5),[DateTimeOffset]::UtcNow.AddHours(2))
    [IO.File]::WriteAllText("$local/panel-cert.pem",$certificate.ExportCertificatePem())
    [IO.File]::WriteAllText("$local/panel-key.pem",$rsa.ExportPkcs8PrivateKeyPem())
    $certificate.Dispose(); $rsa.Dispose()
    Upload @("$local/panel-cert.pem","$local/panel-key.pem")
    [void](Remote "chmod 600 '$remote/panel-key.pem'; cd '$remote'; nohup '$remote/d04-http-target' --listen 127.0.0.1:18080 --udp-listen 127.0.0.1:18081 > target.log 2>&1 < /dev/null & echo `$! > '$remote/target.pid'")
    $relay=Start-Local "$local/ssh-relay.exe" @('-destination',$SshDestination,'-port',"$SshPort",'-identity',$SshIdentityFile,'-known-hosts',$SshKnownHostsFile,'-ports','10085,18443,18444,18445,28443','-remote-helper',"$remote/d04-http-target")
    $handler=[Net.Http.HttpClientHandler]::new(); $handler.AllowAutoRedirect=$false
    $handler.CookieContainer=[Net.CookieContainer]::new()
    # The only TLS endpoint is this isolated loopback relay; Agent pins the PEM.
    $handler.ServerCertificateCustomValidationCallback=[Net.Http.HttpClientHandler]::DangerousAcceptAnyServerCertificateValidator
    $http=[Net.Http.HttpClient]::new($handler); $http.Timeout=[TimeSpan]::FromSeconds(5)
    Start-Panel
    Login-Admin
    $joinResponse=Request POST '/admin/join-tokens' (Form @{csrf=$adminCsrf})
    $join=[regex]::Match($joinResponse.Body,'<code>([A-Za-z0-9_-]{43})</code>').Groups[1].Value
    if (-not $join) { throw 'Join Token missing.' }
    $pair=Remote "printf '%s\n' '$join' | '$remote/d04-agent' pair --panel-url https://127.0.0.1:28443 --data-dir '$remote/agent' --ca-cert '$remote/panel-cert.pem' --name 'D04 local Agent' --join-token-stdin"
    $script:machineID=[regex]::Match($pair.Text,'[a-f0-9]{8}-[a-f0-9]{4}-4[a-f0-9]{3}-[89ab][a-f0-9]{3}-[a-f0-9]{12}').Value
    if (-not $machineID) { throw 'Machine identity missing.' }
    foreach ($index in 0..2) {
        $form=Form @{csrf=$adminCsrf;name="入口 $index";public_host='127.0.0.1';public_port=(45001+$index);listen_host='127.0.0.1';listen_port=(18443+$index);server_name='www.cloudflare.com';target='www.cloudflare.com:443';enabled='true';development='true'}
        if ((Request POST "/admin/machines/$machineID/nodes" $form).Status -ne 303) { throw 'Creating Node through its form failed.' }
    }
    [void](Sync-Agent)
    $state=Core-State
    $page=Request GET "/admin/machines/$machineID/nodes"
    if ($state.current.revision -ne 4 -or $page.Body -notmatch '已应用：4' -or $page.Body -match 'core_not_implemented') { throw 'Panel did not receive actual applied status.' }
    $nodes=(Request GET "/api/admin/machines/$machineID/nodes").Body | ConvertFrom-Json
    $nodes=@($nodes.nodes | Sort-Object listen_port)
    if ($nodes.Count -ne 3 -or @($nodes.development_uuid | Select-Object -Unique).Count -ne 1) { throw 'Node credential isolation fixture is invalid.' }
    $inbounds=@(); $outbounds=@(); $rules=@()
    foreach ($index in 0..2) {
        $node=$nodes[$index]; $tag="node-$index"
        $inbounds+=@{listen='127.0.0.1';port=(10808+$index);tag=$tag;protocol='socks';settings=@{auth='noauth';udp=$true}}
        $outbounds+=@{tag=$tag;protocol='vless';settings=@{vnext=@(@{address='127.0.0.1';port=$node.listen_port;users=@(@{id=$node.development_uuid;encryption='none'})})};streamSettings=@{network='raw';security='reality';realitySettings=@{serverName=$node.server_name;fingerprint='chrome';password=$node.public_key;shortId=$node.short_id;spiderX='/'}}}
        $rules+=@{type='field';inboundTag=@($tag);outboundTag=$tag}
    }
    $clientConfig=@{log=@{loglevel='none'};inbounds=$inbounds;outbounds=$outbounds;routing=@{rules=$rules}} | ConvertTo-Json -Depth 20
    [IO.File]::WriteAllText("$local/client.json",$clientConfig)
    $client=Start-Local (Join-Path $work 'windows/xray.exe') @('run','-config',"$local/client.json")
    Start-Sleep -Milliseconds 500
    foreach ($port in 10808..10810) { Proxy-Get $port }
    $count=(Remote "for p in /proc/[0-9]*/cmdline; do tr '\000' ' ' 2>/dev/null < `$p | grep -F '$remote/xray run -config $remote/agent/core/' || true; done").Text.Split("`n",[StringSplitOptions]::RemoveEmptyEntries).Count
    if ($count -ne 1) { throw "Expected one managed Core, observed $count." }
    $result.checks.three_nodes_one_core='PASS'
    $firstPID=$state.process.pid; $firstGeneration=$state.core_generation
    [void](Sync-Agent)
    $unchanged=Core-State
    if ($unchanged.process.pid -ne $firstPID -or $unchanged.core_generation -ne $firstGeneration) { throw 'Unchanged snapshot restarted Core.' }
    $result.checks.unchanged_snapshot='PASS'
    Node-Form $nodes[2] '只改显示名称'
    [void](Sync-Agent)
    $metadata=Core-State
    if ($metadata.process.pid -ne $firstPID -or $metadata.core_generation -ne $firstGeneration -or $metadata.current.revision -ne 5) { throw 'Metadata change restarted Core or failed to advance applied scope.' }
    $result.checks.metadata_without_restart='PASS'
    Stop-Owned 'panel.pid' "$remote/d04-control serve"
    $offline=Sync-Agent -AllowFailure
    if ($offline.Code -eq 0) { throw 'Panel-offline sync unexpectedly succeeded.' }
    foreach ($port in 10808..10810) { Proxy-Get $port }
    if ((Core-State).core_generation -ne $firstGeneration) { throw 'Panel outage restarted Current.' }
    $result.checks.panel_offline_service='PASS'
    Start-Panel
    Login-Admin

    # Real Agent termination while the candidate is awaiting stable health.
    Node-Form $nodes[2] '新配置待健康检查' 18446
    $crash=@'
cd '__REMOTE__'
nohup ./d04-agent sync --data-dir '__REMOTE__/agent' --ca-cert '__REMOTE__/panel-cert.pem' --xray-path '__REMOTE__/xray' --development-loopback-test > crash-agent.log 2>&1 < /dev/null &
agent=$!; echo $agent > crash-agent.pid
i=0
while test "$i" -lt 1000; do
  if grep -q '"phase": "checking"' agent/core/state.json; then
    kill -KILL "$agent"
    echo killed_before_health_commit
    exit 0
  fi
  kill -0 "$agent" 2>/dev/null || { echo agent_exited_before_crash; exit 1; }
  i=$((i+1)); sleep 0.005
done
exit 1
'@
    $crashed=Remote $crash.Replace('__REMOTE__',$remote)
    if ($crashed.Text -notmatch 'killed_before_health_commit') { throw 'Agent crash injection did not execute.' }
    Stop-Owned 'panel.pid' "$remote/d04-control serve"
    $recovery=Sync-Agent -AllowFailure
    $recovered=Core-State
    if ($recovery.Code -eq 0 -or $recovered.current.revision -ne 5 -or $recovered.candidate -or $recovered.phase -ne 'stable' -or -not $recovered.lkg) { throw 'Offline recovery adopted an unconfirmed candidate.' }
    foreach ($port in 10808..10810) { Proxy-Get $port }
    $result.checks.agent_killed_before_health_offline_rollback='PASS'
    Start-Panel
    Login-Admin
    Node-Form $nodes[2] '恢复原入口' 18445
    [void](Sync-Agent)

    $probeAFile="$local/session-a.json"; $probeBFile="$local/session-b.json"
    $probeA=Start-Local "$local/session-probe.exe" @('-socks','127.0.0.1:10808','-output',$probeAFile)
    $probeB=Start-Local "$local/session-probe.exe" @('-socks','127.0.0.1:10809','-output',$probeBFile)
    $beforeA=Wait-Probe $probeAFile $false; $beforeB=Wait-Probe $probeBFile $false
    $beforeRevoke=Core-State
    $deleted=Request POST "/admin/machines/$machineID/nodes/$($nodes[0].node_id)/delete" (Form @{csrf=$adminCsrf})
    if ($deleted.Status -ne 303) { throw 'Node delete form failed.' }
    [void](Sync-Agent)
    $afterA=Wait-Probe $probeAFile $true
    # Other Node TCP also closes because revocation is a complete Core restart.
    $deadline=[DateTime]::UtcNow.AddSeconds(10)
    do { $afterB=Get-Content $probeBFile -Raw | ConvertFrom-Json; if ($afterB.tcp_closed) { break }; Start-Sleep -Milliseconds 100 } while ([DateTime]::UtcNow -lt $deadline)
    if (-not $afterB.tcp_closed -or (Core-State).core_generation -le $beforeRevoke.core_generation) { throw 'Revocation did not terminate existing Core sessions.' }
    Proxy-Get 10809; Proxy-Get 10810
    & curl.exe --fail --silent --max-time 5 --noproxy '' --proxy socks5h://127.0.0.1:10808 http://127.0.0.1:18080/ > $null 2>&1
    if ($LASTEXITCODE -eq 0) { throw 'Revoked Node still accepts proxy requests.' }
    $boundaryJSON=(Remote "printf '['; separator=''; for f in '$remote'/agent/core/boundaries/*.json; do printf '%s' `"`$separator`"; cat `"`$f`"; separator=','; done; printf ']'").Text
    $boundaries=@($boundaryJSON | ConvertFrom-Json)
    $revokedCounters=@($boundaries.counters | Where-Object { $_.node_id -eq $nodes[0].node_id -and $_.uplink -gt 0 -and $_.downlink -gt 0 })
    if ($revokedCounters.Count -eq 0 -or @($boundaries | Where-Object { $_.unobserved_tail -and $_.observation_gap }).Count -eq 0) { throw 'Final counters, old Node mapping, or observation gap were not retained.' }
    $result.checks.revocation=[ordered]@{existing_tcp_closed=$afterA.tcp_closed;existing_udp_closed=$afterA.udp_closed;other_node_tcp_interrupted=$afterB.tcp_closed;other_nodes_reconnect=$true;old_mapping_retained=$true;boundary_count=$boundaries.Count;observed_uplink=$revokedCounters[-1].uplink;observed_downlink=$revokedCounters[-1].downlink;unobserved_tail_explicit=$true}
    $result.checks.panel_local_agent='PASS'
    Write-Output 'D04: three Nodes, offline rollback, and existing TCP/UDP revocation passed.'
    $result.passed=$true
} finally {
    $index=0
    foreach ($entry in $processes) {
        Stop-Local $entry
        [IO.File]::WriteAllText("$local/helper-$index.stdout.log",$entry.Out.GetAwaiter().GetResult())
        [IO.File]::WriteAllText("$local/helper-$index.stderr.log",$entry.Err.GetAwaiter().GetResult())
        $index++
    }
    if ($null -ne $http) { $http.Dispose() }
    if ($prepared) {
        try {
            $diagnostic=Remote "cd '$remote'; for f in panel.log target.log crash-agent.log agent/core/state.json; do if test -f `"`$f`"; then printf '\n%s\n' `"`$f`"; cat `"`$f`"; fi; done"
            [IO.File]::WriteAllText("$local/diagnostic.txt",$diagnostic.Text)
            $cleanup=@'
cd '__REMOTE__'
test "$(cat .owner)" = '__REMOTE__'
touch monitor.stop
for f in panel.pid target.pid monitor.pid crash-agent.pid; do
  if test -f "$f"; then
    pid=$(cat "$f")
    if test -r /proc/$pid/cmdline; then
      args=$(tr '\000' ' ' < /proc/$pid/cmdline)
      case "$args" in *__REMOTE__*) kill "$pid" 2>/dev/null || true;; esac
    fi
  fi
done
for file in /proc/[0-9]*/cmdline; do
  args=$(tr '\000' ' ' 2>/dev/null < "$file" || true)
  case "$args" in "__REMOTE__/xray run -config __REMOTE__/agent/core/"*) pid=${file#/proc/}; pid=${pid%/cmdline}; kill "$pid" 2>/dev/null || true;; esac
done
sleep 0.3
awk '$1=="mem"{n++;if($2>m)m=$2} $1=="check"{c++;if($2>cm)cm=$2} $1=="run"{r++;if($2>rm)rm=$2} END{printf "{\"system_samples\":%d,\"system_peak_bytes\":%d,\"check_samples\":%d,\"check_sampled_hwm_kib\":%d,\"run_samples\":%d,\"run_sampled_hwm_kib\":%d}",n,m,c,cm,r,rm}' resource.tsv
'@
            $resources=Remote $cleanup.Replace('__REMOTE__',$remote)
            $result.resources=$resources.Text | ConvertFrom-Json
            $after=Remote "cat /sys/fs/cgroup/memory.events; netstat -lnt; ps -o pid,args | awk '/filepanel|nginx/ && !/awk/ {print}'; df -k /"
            [IO.File]::WriteAllText("$local/after.txt",$after.Text)
            $servicesBefore=@([regex]::Matches($baseline,'(?m)^\s*\d+ .*(?:filepanel|nginx).*') | ForEach-Object { $_.Value.Trim() })
            $servicesAfter=@([regex]::Matches($after.Text,'(?m)^\s*\d+ .*(?:filepanel|nginx).*') | ForEach-Object { $_.Value.Trim() })
            $listenersBefore=@([regex]::Matches($baseline,'(?m)^tcp.*LISTEN\s*$') | ForEach-Object { $_.Value.Trim() })
            $listenersAfter=@([regex]::Matches($after.Text,'(?m)^tcp.*LISTEN\s*$') | ForEach-Object { $_.Value.Trim() })
            if ($after.Text -notmatch 'oom_kill 0' -or @(Compare-Object $servicesBefore $servicesAfter).Count -ne 0 -or @(Compare-Object $listenersBefore $listenersAfter).Count -ne 0) { $result.passed=$false; throw 'Original services, listeners, or OOM counters changed.' }
            # Only this guarded, uniquely named smoke directory is removed.
            $final=Remote "test `"`$(cat '$remote/.owner')`" = '$remote'; rm -rf '$remote'; test ! -e '$remote'; df -k /"
            [IO.File]::WriteAllText("$local/final-disk.txt",$final.Text)
            $result.cleanup='PASS'
        } catch { $result.passed=$false; $result.cleanup_error=$_.Exception.Message; Write-Warning $_.Exception.Message }
    }
    foreach ($name in $envNames) { [Environment]::SetEnvironmentVariable($name,$oldEnv[$name],'Process') }
    $result | ConvertTo-Json -Depth 12 | Set-Content -LiteralPath "$local/result.json" -Encoding utf8
    Write-Output "D04 result: $local/result.json"
}
if (-not $result.passed) { throw 'D04 smoke did not pass.' }
$result | ConvertTo-Json -Depth 12
