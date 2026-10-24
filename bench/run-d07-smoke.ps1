#requires -Version 7.0
param(
    [Parameter(Mandatory)][string]$SshDestination,
    [int]$SshPort=45000,
    [Parameter(Mandatory)][string]$SshIdentityFile,
    [Parameter(Mandatory)][string]$SshKnownHostsFile,
    [Parameter(Mandatory)][string]$ExpectedHostname,
    [Parameter(Mandatory)][string]$ExpectedIPv4
)
$ErrorActionPreference='Stop'
$root=[IO.Path]::GetFullPath((Join-Path $PSScriptRoot '..'))
$work=Join-Path $PSScriptRoot '.work'
$runID=[guid]::NewGuid().ToString('N')
$local=Join-Path $work "d07-$runID"
$remote="/tmp/panel-agent-d07-$runID"
if($ExpectedHostname -notmatch '^[A-Za-z0-9_.-]+$' -or $ExpectedIPv4 -notmatch '^\d+\.\d+\.\d+\.\d+$'){throw 'Invalid identity expectation.'}
[void][Net.IPAddress]::Parse($ExpectedIPv4)
New-Item -ItemType Directory -Path $local | Out-Null
$options=@('-F','NUL','-i',(Resolve-Path -LiteralPath $SshIdentityFile).Path,'-o','IdentitiesOnly=yes','-o','BatchMode=yes','-o','StrictHostKeyChecking=yes','-o',"UserKnownHostsFile=$((Resolve-Path -LiteralPath $SshKnownHostsFile).Path)",'-o','ConnectTimeout=15')
function Remote([string]$Code){
    $encoded=[Convert]::ToBase64String([Text.Encoding]::UTF8.GetBytes(("set -eu`n"+$Code).Replace("`r`n","`n")))
    $output=& ssh @options -p $SshPort $SshDestination "printf '%s' '$encoded' | base64 -d | sh" 2>&1
    if($LASTEXITCODE -ne 0){throw "Remote check failed: $($output -join ' ')"}
    return ($output -join "`n")
}
$prepared=$false
$result=[ordered]@{task='D07';passed=$false;host=$ExpectedHostname;checked_at=[DateTime]::UtcNow.ToString('o');checks=[ordered]@{}}
$previousEnv=@{};foreach($name in @('GOPATH','GOCACHE','GOMODCACHE','GOOS','GOARCH','CGO_ENABLED')){$previousEnv[$name]=[Environment]::GetEnvironmentVariable($name,'Process')}
Push-Location $root
try {
    $env:GOPATH=Join-Path $work 'gopath';$env:GOCACHE=Join-Path $work 'gocache';$env:GOMODCACHE=Join-Path $work 'gomodcache';$env:GOOS='windows';$env:GOARCH='amd64';$env:CGO_ENABLED='0'
    & go build -trimpath -ldflags '-s -w' -o "$local/fixture.exe" ./bench/d07-smoke
    if($LASTEXITCODE -ne 0){throw 'Windows build failed.'}
    $windows=(& "$local/fixture.exe") -join "`n"
    if($LASTEXITCODE -ne 0){throw 'Windows fixture failed.'}
    [IO.File]::WriteAllText("$local/windows.json",$windows+"`n",[Text.UTF8Encoding]::new($false))
    $env:GOOS='linux'
    & go build -trimpath -ldflags '-s -w' -o "$local/fixture-linux" ./bench/d07-smoke
    if($LASTEXITCODE -ne 0){throw 'Linux build failed.'}
    $hash=(Get-FileHash "$local/fixture-linux" -Algorithm SHA256).Hash.ToLowerInvariant()
    $identity=Remote "test `"`$(hostname)`" = '$ExpectedHostname'; ip -4 -o addr show | grep -F ' $ExpectedIPv4/' >/dev/null; test `"`$(uname -m)`" = x86_64; cat /etc/alpine-release; uname -r"
    $result.checks.identity=$identity
    $prepared=$true
    [void](Remote "umask 077; mkdir '$remote'; printf '%s' '$runID' > '$remote/.owner'; ss -lnt | tail -n +2 | awk '{print `$4}' | sort > '$remote/listeners.before'; ps -o pid,args | grep -E '[n]ginx|[f]ilepanel|[f]ile-panel' > '$remote/services.before'")
    & scp -O @options -P $SshPort "$local/fixture-linux" "${SshDestination}:$remote/fixture-linux"
    if($LASTEXITCODE -ne 0){throw 'Fixture upload failed.'}
    $linux=Remote "cd '$remote'; test `"`$(cat .owner)`" = '$runID'; test `"`$(sha256sum fixture-linux | cut -d ' ' -f 1)`" = '$hash'; chmod 700 fixture-linux; timeout 30 ./fixture-linux"
    [IO.File]::WriteAllText("$local/linux.json",$linux+"`n",[Text.UTF8Encoding]::new($false))
    if($windows -cne $linux){throw 'Windows and Linux outputs differ.'}
    $result.checks.identical_windows_linux=$true
    $result.checks.fixture_sha256=(Get-FileHash "$local/windows.json" -Algorithm SHA256).Hash.ToLowerInvariant()
    $result.checks.linux_binary_sha256=$hash
    $result.checks.tzdata=($linux|ConvertFrom-Json).tzdata
    [void](Remote "ss -lnt | tail -n +2 | awk '{print `$4}' | sort > '$remote/listeners.after'; cmp '$remote/listeners.before' '$remote/listeners.after'; while read -r pid args; do test -d /proc/`$pid; done < '$remote/services.before'")
    $result.checks.original_listeners_and_services=$true
    $result.passed=$true
} finally {
    if($prepared){
        try { [void](Remote "test `"`$(hostname)`" = '$ExpectedHostname'; test `"`$(cat '$remote/.owner')`" = '$runID'; rm -rf -- '$remote'; test ! -e '$remote'");$result.checks.cleanup='PASS' }
        catch {$result.passed=$false;$result.checks.cleanup=$_.Exception.Message}
    }
    Pop-Location
    foreach($name in $previousEnv.Keys){[Environment]::SetEnvironmentVariable($name,$previousEnv[$name],'Process')}
    $result|ConvertTo-Json -Depth 8|Set-Content -LiteralPath "$local/result.json" -Encoding utf8
    Get-Content -LiteralPath "$local/result.json"
    Write-Output "Evidence directory: $local"
    if($result.checks.cleanup -ne 'PASS'){throw 'D07 isolated cleanup was not verified.'}
}
