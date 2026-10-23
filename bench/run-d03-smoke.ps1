$ErrorActionPreference = 'Stop'

$workspaceRoot = [System.IO.Path]::GetFullPath((Join-Path $PSScriptRoot '..'))
$workRoot = [System.IO.Path]::GetFullPath((Join-Path $workspaceRoot 'bench\.work'))
New-Item -ItemType Directory -Force -Path $workRoot | Out-Null
$runDir = [System.IO.Path]::GetFullPath((Join-Path $workRoot ("d03-smoke-" + [guid]::NewGuid().ToString('N'))))
if (-not $runDir.StartsWith($workRoot + [System.IO.Path]::DirectorySeparatorChar, [System.StringComparison]::OrdinalIgnoreCase)) {
    throw "Smoke directory escaped the workspace: $runDir"
}
New-Item -ItemType Directory -Path $runDir | Out-Null

$previousGoPath = $env:GOPATH
$previousGoCache = $env:GOCACHE
$previousGoModCache = $env:GOMODCACHE
$env:GOPATH = Join-Path $workRoot 'gopath'
$env:GOCACHE = Join-Path $workRoot 'gocache'
$env:GOMODCACHE = Join-Path $workRoot 'gomodcache'
$processes = [System.Collections.Generic.List[object]]::new()
$success = $false
$httpClient = $null

function New-ControlProcess {
    param([string]$Binary, [string[]]$Arguments, [switch]$RedirectInput, [System.Collections.IDictionary]$Environment = $null)

    $startInfo = [System.Diagnostics.ProcessStartInfo]::new()
    $startInfo.FileName = $Binary
    $startInfo.WorkingDirectory = $script:workspaceRoot
    $startInfo.UseShellExecute = $false
    $startInfo.CreateNoWindow = $true
    $startInfo.WindowStyle = [System.Diagnostics.ProcessWindowStyle]::Hidden
    $startInfo.RedirectStandardInput = [bool]$RedirectInput
    $startInfo.RedirectStandardOutput = $true
    $startInfo.RedirectStandardError = $true
    foreach ($name in @('PANEL_DATA_DIR', 'PANEL_LISTEN_ADDR', 'PANEL_TLS_CERT', 'PANEL_TLS_KEY', 'PANEL_TRUST_PROXY', 'PANEL_COOKIE_SECURE', 'PANEL_AGENT_DATA_DIR')) {
        [void]$startInfo.Environment.Remove($name)
    }
    if ($null -ne $Environment) {
        foreach ($name in $Environment.Keys) { $startInfo.Environment[[string]$name] = [string]$Environment[$name] }
    }
    foreach ($argument in $Arguments) { $startInfo.ArgumentList.Add($argument) }

    $process = [System.Diagnostics.Process]::new()
    $process.StartInfo = $startInfo
    if (-not $process.Start()) { throw "Could not start $Binary." }
    $entry = [pscustomobject]@{
        Process = $process
        Stdout = $process.StandardOutput.ReadToEndAsync()
        Stderr = $process.StandardError.ReadToEndAsync()
    }
    $script:processes.Add($entry)
    return $entry
}

function Wait-ControlProcess {
    param([object]$Entry, [int]$TimeoutMilliseconds = 30000)

    if (-not $Entry.Process.WaitForExit($TimeoutMilliseconds)) { throw 'The process did not exit before the timeout.' }
    $Entry.Process.WaitForExit()
    return [pscustomobject]@{
        ExitCode = $Entry.Process.ExitCode
        Stdout = $Entry.Stdout.GetAwaiter().GetResult()
        Stderr = $Entry.Stderr.GetAwaiter().GetResult()
    }
}

function Stop-ControlProcess {
    param([object]$Entry)

    if ($null -eq $Entry) { return }
    if (-not $Entry.Process.HasExited) {
        $Entry.Process.Kill($true)
        if (-not $Entry.Process.WaitForExit(10000)) { throw 'Could not stop a smoke process.' }
    }
    if (-not $Entry.Stdout.IsCompleted) { $Entry.Stdout.GetAwaiter().GetResult() | Out-Null }
    if (-not $Entry.Stderr.IsCompleted) { $Entry.Stderr.GetAwaiter().GetResult() | Out-Null }
}

function Send-PanelRequest {
    param([string]$Method, [string]$Uri, [string]$Body = $null, [string]$ContentType = $null, [string]$Origin = $null, [string]$Bearer = $null)

    $request = [System.Net.Http.HttpRequestMessage]::new([System.Net.Http.HttpMethod]::new($Method), $Uri)
    if (-not [string]::IsNullOrEmpty($Body)) {
        $request.Content = [System.Net.Http.StringContent]::new($Body, [System.Text.Encoding]::UTF8, $ContentType)
    }
    if ($Origin) { [void]$request.Headers.TryAddWithoutValidation('Origin', $Origin) }
    if ($Bearer) { [void]$request.Headers.TryAddWithoutValidation('Authorization', "Bearer $Bearer") }
    try {
        $response = $script:httpClient.SendAsync($request).GetAwaiter().GetResult()
        try {
            return [pscustomobject]@{
                Status = [int]$response.StatusCode
                Body = $response.Content.ReadAsStringAsync().GetAwaiter().GetResult()
            }
        } finally { $response.Dispose() }
    } finally { $request.Dispose() }
}

function Encode-Form {
    param([System.Collections.IDictionary]$Values)
    $items = foreach ($key in $Values.Keys) {
        [System.Net.WebUtility]::UrlEncode([string]$key) + '=' + [System.Net.WebUtility]::UrlEncode([string]$Values[$key])
    }
    return $items -join '&'
}

function Get-CsrfFromPage {
    param([string]$Uri)
    $response = Send-PanelRequest -Method GET -Uri $Uri
    if ($response.Status -ne 200) { throw "Page returned HTTP $($response.Status): $Uri" }
    $match = [regex]::Match($response.Body, 'name="csrf" value="([^"]+)"')
    if (-not $match.Success) { throw "Page had no CSRF token: $Uri" }
    return $match.Groups[1].Value
}

function Login-Admin {
    param([string]$BaseUrl, [string]$Password)
    $loginToken = Get-CsrfFromPage "$BaseUrl/login"
    $loginForm = Encode-Form @{ csrf = $loginToken; password = $Password }
    $response = Send-PanelRequest -Method POST -Uri "$BaseUrl/login" -Body $loginForm -ContentType 'application/x-www-form-urlencoded' -Origin $BaseUrl
    if ($response.Status -ne 303) { throw "Admin login returned HTTP $($response.Status)." }
}

function Create-JoinToken {
    param([string]$BaseUrl, [string]$Csrf, [string]$TargetMachineID = '')
    $body = Encode-Form @{ csrf = $Csrf; target_machine_id = $TargetMachineID }
    $response = Send-PanelRequest -Method POST -Uri "$BaseUrl/admin/join-tokens" -Body $body -ContentType 'application/x-www-form-urlencoded' -Origin $BaseUrl
    if ($response.Status -ne 200) { throw "Join Token request returned HTTP $($response.Status)." }
    $match = [regex]::Match($response.Body, '<code>([A-Za-z0-9_-]{43})</code>')
    if (-not $match.Success) { throw 'Admin page did not show a one-time Join Token.' }
    return $match.Groups[1].Value
}

function Start-Panel {
    param([string]$DataDir, [string]$ListenAddress, [string]$CertPath, [string]$KeyPath)
    return New-ControlProcess -Binary $script:controlBinary -Arguments @('serve', '--data-dir', $DataDir, '--listen', $ListenAddress) -Environment @{
        PANEL_TLS_CERT = $CertPath
        PANEL_TLS_KEY = $KeyPath
    }
}

function Wait-PanelReady {
    param([string]$BaseUrl, [object]$Entry)
    $deadline = [DateTime]::UtcNow.AddSeconds(15)
    while ([DateTime]::UtcNow -lt $deadline) {
        if ($Entry.Process.HasExited) {
            $result = Wait-ControlProcess -Entry $Entry
            throw "Panel exited during startup: $($result.Stderr)"
        }
        try {
            $response = Send-PanelRequest -Method GET -Uri "$BaseUrl/healthz"
            if ($response.Status -eq 200) { return }
        } catch { Start-Sleep -Milliseconds 150 }
    }
    throw 'Panel did not become ready.'
}

function Get-MachineHeartbeat {
    param([string]$BaseUrl)
    $response = Send-PanelRequest -Method GET -Uri "$BaseUrl/admin/machines"
    if ($response.Status -ne 200) { throw "Machine page returned HTTP $($response.Status)." }
    $match = [regex]::Match($response.Body, '最近心跳：([^；<]+)；收到 revision')
    if (-not $match.Success) { throw 'Machine page did not show the last heartbeat.' }
    return $match.Groups[1].Value
}

try {
    $script:controlBinary = Join-Path $runDir 'control.exe'
    $agentBinary = Join-Path $runDir 'control-agent.exe'
    & go build -o $script:controlBinary ./cmd/control
    if ($LASTEXITCODE -ne 0) { throw "Panel build failed with exit code $LASTEXITCODE." }
    & go build -o $agentBinary ./cmd/agent
    if ($LASTEXITCODE -ne 0) { throw "Agent build failed with exit code $LASTEXITCODE." }

    $dataDir = Join-Path $runDir 'panel-data'
    $agentDir = Join-Path $runDir 'agent-data'
    $password = [guid]::NewGuid().ToString('N') + 'Aa7!'
    $init = New-ControlProcess -Binary $script:controlBinary -Arguments @('admin', 'init', '--data-dir', $dataDir, '--password-stdin') -RedirectInput
    $init.Process.StandardInput.WriteLine($password)
    $init.Process.StandardInput.WriteLine($password)
    $init.Process.StandardInput.Close()
    $initResult = Wait-ControlProcess -Entry $init
    if ($initResult.ExitCode -ne 0 -or $initResult.Stdout -notmatch '管理员已初始化') { throw "Panel initialization failed: $($initResult.Stderr)" }

    $certPath = Join-Path $runDir 'panel-cert.pem'
    $keyPath = Join-Path $runDir 'panel-key.pem'
    $rsa = [System.Security.Cryptography.RSA]::Create(2048)
    $certRequest = [System.Security.Cryptography.X509Certificates.CertificateRequest]::new(
        'CN=127.0.0.1', $rsa, [System.Security.Cryptography.HashAlgorithmName]::SHA256,
        [System.Security.Cryptography.RSASignaturePadding]::Pkcs1
    )
    $san = [System.Security.Cryptography.X509Certificates.SubjectAlternativeNameBuilder]::new()
    $san.AddIpAddress([System.Net.IPAddress]::Loopback)
    $certRequest.CertificateExtensions.Add($san.Build())
    $certificate = $certRequest.CreateSelfSigned([DateTimeOffset]::UtcNow.AddMinutes(-5), [DateTimeOffset]::UtcNow.AddHours(1))
    [System.IO.File]::WriteAllText($certPath, $certificate.ExportCertificatePem(), [System.Text.UTF8Encoding]::new($false))
    [System.IO.File]::WriteAllText($keyPath, $rsa.ExportPkcs8PrivateKeyPem(), [System.Text.UTF8Encoding]::new($false))
    $certificate.Dispose()
    $rsa.Dispose()

    $listener = [System.Net.Sockets.TcpListener]::new([System.Net.IPAddress]::Loopback, 0)
    $listener.Start()
    $port = ([System.Net.IPEndPoint]$listener.LocalEndpoint).Port
    $listener.Stop()
    $listenAddress = "127.0.0.1:$port"
    $baseUrl = "https://$listenAddress"

    $cookieJar = [System.Net.CookieContainer]::new()
    $httpHandler = [System.Net.Http.HttpClientHandler]::new()
    $httpHandler.CookieContainer = $cookieJar
    $httpHandler.AllowAutoRedirect = $false
    $httpHandler.ServerCertificateCustomValidationCallback = [System.Net.Http.HttpClientHandler]::DangerousAcceptAnyServerCertificateValidator
    $script:httpClient = [System.Net.Http.HttpClient]::new($httpHandler)
    $script:httpClient.Timeout = [TimeSpan]::FromSeconds(5)

    $panel = Start-Panel -DataDir $dataDir -ListenAddress $listenAddress -CertPath $certPath -KeyPath $keyPath
    Wait-PanelReady -BaseUrl $baseUrl -Entry $panel
    Login-Admin -BaseUrl $baseUrl -Password $password

    $csrf = Get-CsrfFromPage "$baseUrl/admin/machines"
    $joinToken = Create-JoinToken -BaseUrl $baseUrl -Csrf $csrf
    $pair = New-ControlProcess -Binary $agentBinary -Arguments @('pair', '--panel-url', $baseUrl, '--data-dir', $agentDir, '--name', 'D03 smoke agent', '--ca-cert', $certPath, '--join-token-stdin') -RedirectInput
    $pair.Process.StandardInput.WriteLine($joinToken)
    $pair.Process.StandardInput.Close()
    $pairResult = Wait-ControlProcess -Entry $pair
    if ($pairResult.ExitCode -ne 0 -or $pairResult.Stdout -notmatch 'Agent 已配对') { throw "Agent pairing failed: $($pairResult.Stderr)" }

    $identityPath = Join-Path $agentDir 'agent.identity.json'
    $identity = Get-Content -LiteralPath $identityPath -Raw | ConvertFrom-Json
    $machineID = [string]$identity.machine_id
    $oldSecret = [string]$identity.machine_secret
    if (-not $machineID -or -not $identity.authority_epoch -or $identity.revision -ne 1 -or $identity.desired_state.nodes.Count -ne 0) {
        throw 'Agent did not persist its machine identity and empty full snapshot.'
    }
    $initialHeartbeat = Get-MachineHeartbeat -BaseUrl $baseUrl

    Stop-ControlProcess -Entry $panel
    Start-Sleep -Milliseconds 300
    $agentRun = New-ControlProcess -Binary $agentBinary -Arguments @('run', '--data-dir', $agentDir, '--ca-cert', $certPath)
    Start-Sleep -Milliseconds 800
    $panel = Start-Panel -DataDir $dataDir -ListenAddress $listenAddress -CertPath $certPath -KeyPath $keyPath
    Wait-PanelReady -BaseUrl $baseUrl -Entry $panel
    Login-Admin -BaseUrl $baseUrl -Password $password
    $recovered = $false
    $recoveryDeadline = [DateTime]::UtcNow.AddSeconds(8)
    while ([DateTime]::UtcNow -lt $recoveryDeadline) {
        try {
            if ((Get-MachineHeartbeat -BaseUrl $baseUrl) -ne $initialHeartbeat) { $recovered = $true; break }
        } catch { }
        Start-Sleep -Milliseconds 250
    }
    if (-not $recovered) { throw 'Agent did not recover its control channel after the Panel returned.' }
    Stop-ControlProcess -Entry $agentRun

    $csrf = Get-CsrfFromPage "$baseUrl/admin/machines"
    $rotateForm = Encode-Form @{ csrf = $csrf }
    $response = Send-PanelRequest -Method POST -Uri "$baseUrl/admin/authority/rotate" -Body $rotateForm -ContentType 'application/x-www-form-urlencoded' -Origin $baseUrl
    if ($response.Status -ne 303) { throw "Authority rotation returned HTTP $($response.Status)." }
    $sync = New-ControlProcess -Binary $agentBinary -Arguments @('sync', '--data-dir', $agentDir, '--ca-cert', $certPath)
    $syncResult = Wait-ControlProcess -Entry $sync
    if ($syncResult.ExitCode -ne 0 -or $syncResult.Stdout -notmatch '未报告 applied 成功') { throw "Agent authority sync failed: $($syncResult.Stderr)" }
    $identity = Get-Content -LiteralPath $identityPath -Raw | ConvertFrom-Json
    $machinesPage = Send-PanelRequest -Method GET -Uri "$baseUrl/admin/machines"
    if ($machinesPage.Status -ne 200 -or $machinesPage.Body -notmatch [regex]::Escape([string]$identity.authority_epoch)) {
        throw 'Machine did not persist the new authority after full-snapshot handshake.'
    }

    $csrf = Get-CsrfFromPage "$baseUrl/admin/machines"
    $repairToken = Create-JoinToken -BaseUrl $baseUrl -Csrf $csrf -TargetMachineID $machineID
    $repair = New-ControlProcess -Binary $agentBinary -Arguments @('re-pair', '--data-dir', $agentDir, '--ca-cert', $certPath, '--join-token-stdin') -RedirectInput
    $repair.Process.StandardInput.WriteLine($repairToken)
    $repair.Process.StandardInput.Close()
    $repairResult = Wait-ControlProcess -Entry $repair
    if ($repairResult.ExitCode -ne 0) { throw "Agent re-pair failed: $($repairResult.Stderr)" }
    $repairedIdentity = Get-Content -LiteralPath $identityPath -Raw | ConvertFrom-Json
    if ($repairedIdentity.machine_id -ne $machineID -or $repairedIdentity.machine_secret -eq $oldSecret) {
        throw 'Re-pair did not preserve machine identity and replace its secret.'
    }

    $oldRequestBody = @{ request_id = [guid]::NewGuid().ToString(); authority_epoch = $repairedIdentity.authority_epoch; revision = $repairedIdentity.revision; digest = $repairedIdentity.digest } | ConvertTo-Json -Compress
    $response = Send-PanelRequest -Method POST -Uri "$baseUrl/api/v1/agent/desired-state" -Body $oldRequestBody -ContentType 'application/json' -Bearer $oldSecret
    if ($response.Status -ne 401) { throw "Old machine secret returned HTTP $($response.Status) after re-pair." }

    $csrf = Get-CsrfFromPage "$baseUrl/admin/machines"
    $revokeForm = Encode-Form @{ csrf = $csrf }
    $response = Send-PanelRequest -Method POST -Uri "$baseUrl/admin/machines/$machineID/revoke" -Body $revokeForm -ContentType 'application/x-www-form-urlencoded' -Origin $baseUrl
    if ($response.Status -ne 303) { throw "Machine revocation returned HTTP $($response.Status)." }
    $response = Send-PanelRequest -Method POST -Uri "$baseUrl/api/v1/agent/desired-state" -Body $oldRequestBody -ContentType 'application/json' -Bearer ([string]$repairedIdentity.machine_secret)
    if ($response.Status -ne 401) { throw "Revoked machine secret returned HTTP $($response.Status)." }

    Stop-ControlProcess -Entry $panel
    $panel = $null
    $success = $true
    Write-Output 'D03 smoke 通过：HTTPS 登录签发 Join Token、Agent 注册与空快照、断连退避恢复、authority nonce 握手、同身份重新配对、旧凭据与吊销凭据拒绝。'
} finally {
    foreach ($entry in $processes) {
        Stop-ControlProcess -Entry $entry
        $entry.Process.Dispose()
    }
    if ($null -ne $httpClient) { $httpClient.Dispose() }
    $env:GOPATH = $previousGoPath
    $env:GOCACHE = $previousGoCache
    $env:GOMODCACHE = $previousGoModCache
    if ($success) {
        $resolvedParent = [System.IO.Path]::GetFullPath((Split-Path -Parent $runDir))
        if ($resolvedParent -ne $workRoot) { throw "Refusing to remove unexpected smoke path: $runDir" }
        Remove-Item -LiteralPath $runDir -Recurse -Force
    } else {
        Write-Warning "Smoke artifacts preserved at $runDir"
    }
}
