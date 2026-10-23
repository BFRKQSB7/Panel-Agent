$ErrorActionPreference = 'Stop'

$workspaceRoot = [System.IO.Path]::GetFullPath((Join-Path $PSScriptRoot '..'))
$workRoot = [System.IO.Path]::GetFullPath((Join-Path $workspaceRoot 'bench\.work'))
New-Item -ItemType Directory -Force -Path $workRoot | Out-Null
$runDir = [System.IO.Path]::GetFullPath((Join-Path $workRoot ("d02-smoke-" + [guid]::NewGuid().ToString('N'))))
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

function New-ChildProcess {
    param(
        [string[]]$Arguments,
        [switch]$RedirectInput,
        [System.Collections.IDictionary]$Environment = $null
    )

    $startInfo = [System.Diagnostics.ProcessStartInfo]::new()
    $startInfo.FileName = $script:binaryPath
    $startInfo.WorkingDirectory = $script:workspaceRoot
    $startInfo.UseShellExecute = $false
    $startInfo.CreateNoWindow = $true
    $startInfo.WindowStyle = [System.Diagnostics.ProcessWindowStyle]::Hidden
    $startInfo.RedirectStandardInput = [bool]$RedirectInput
    $startInfo.RedirectStandardOutput = $true
    $startInfo.RedirectStandardError = $true
    foreach ($name in @('PANEL_DATA_DIR', 'PANEL_LISTEN_ADDR', 'PANEL_TLS_CERT', 'PANEL_TLS_KEY', 'PANEL_TRUST_PROXY', 'PANEL_COOKIE_SECURE')) {
        [void]$startInfo.Environment.Remove($name)
    }
    if ($null -ne $Environment) {
        foreach ($name in $Environment.Keys) { $startInfo.Environment[[string]$name] = [string]$Environment[$name] }
    }
    foreach ($argument in $Arguments) {
        $startInfo.ArgumentList.Add($argument)
    }

    $process = [System.Diagnostics.Process]::new()
    $process.StartInfo = $startInfo
    if (-not $process.Start()) {
        throw 'Could not start the control process.'
    }
    $entry = [pscustomobject]@{
        Process = $process
        Stdout = $process.StandardOutput.ReadToEndAsync()
        Stderr = $process.StandardError.ReadToEndAsync()
    }
    $script:processes.Add($entry)
    return $entry
}

function Wait-ChildProcess {
    param([object]$Entry, [int]$TimeoutMilliseconds = 30000)

    if (-not $Entry.Process.WaitForExit($TimeoutMilliseconds)) {
        throw 'The control process did not exit before the timeout.'
    }
    $Entry.Process.WaitForExit()
    return [pscustomobject]@{
        ExitCode = $Entry.Process.ExitCode
        Stdout = $Entry.Stdout.GetAwaiter().GetResult()
        Stderr = $Entry.Stderr.GetAwaiter().GetResult()
    }
}

function Stop-ChildProcess {
    param([object]$Entry)

    if (-not $Entry.Process.HasExited) {
        $Entry.Process.Kill($true)
        if (-not $Entry.Process.WaitForExit(10000)) {
            throw 'Could not stop the control process.'
        }
    }
    if (-not $Entry.Stdout.IsCompleted) { $Entry.Stdout.GetAwaiter().GetResult() | Out-Null }
    if (-not $Entry.Stderr.IsCompleted) { $Entry.Stderr.GetAwaiter().GetResult() | Out-Null }
}

function Test-ListeningPort {
    param([int]$Port)

    $client = [System.Net.Sockets.TcpClient]::new()
    try {
        $pending = $client.BeginConnect([System.Net.IPAddress]::Loopback, $Port, $null, $null)
        if (-not $pending.AsyncWaitHandle.WaitOne(300)) { return $false }
        $client.EndConnect($pending)
        return $true
    } catch {
        return $false
    } finally {
        $client.Dispose()
    }
}

function Send-PanelRequest {
    param(
        [string]$Method,
        [string]$Uri,
        [string]$Body = $null,
        [string]$ContentType = $null,
        [string]$Origin = $null,
        [string]$Csrf = $null
    )

    $request = [System.Net.Http.HttpRequestMessage]::new([System.Net.Http.HttpMethod]::new($Method), $Uri)
    if (-not [string]::IsNullOrEmpty($Body)) {
        $request.Content = [System.Net.Http.StringContent]::new($Body, [System.Text.Encoding]::UTF8, $ContentType)
    }
    if ($Origin) { [void]$request.Headers.TryAddWithoutValidation('Origin', $Origin) }
    if ($Csrf) { [void]$request.Headers.TryAddWithoutValidation('X-CSRF-Token', $Csrf) }
    try {
        $response = $script:httpClient.SendAsync($request).GetAwaiter().GetResult()
        try {
            $responseBody = $response.Content.ReadAsStringAsync().GetAwaiter().GetResult()
            $setCookieHeaders = @()
            if ($response.Headers.Contains('Set-Cookie')) { $setCookieHeaders = @($response.Headers.GetValues('Set-Cookie')) }
            return [pscustomobject]@{ Status = [int]$response.StatusCode; Body = $responseBody; SetCookies = $setCookieHeaders }
        } finally {
            $response.Dispose()
        }
    } finally {
        $request.Dispose()
    }
}

function Get-LoginToken {
    param([string]$BaseUrl)

    $response = Send-PanelRequest -Method GET -Uri "$BaseUrl/login"
    if ($response.Status -ne 200) { throw "Login page returned HTTP $($response.Status)." }
    $match = [regex]::Match($response.Body, 'name="csrf" value="([^"]+)"')
    if (-not $match.Success) { throw 'Login page did not include a CSRF token.' }
    return $match.Groups[1].Value
}

function Get-CsrfCookie {
    param([string]$BaseUrl)

    $cookie = $script:cookieJar.GetCookies([uri]$BaseUrl)['panel_csrf']
    if ($null -eq $cookie) { throw 'CSRF cookie was missing.' }
    return $cookie.Value
}

function Encode-Form {
    param([System.Collections.IDictionary]$Values)

    $items = foreach ($key in $Values.Keys) {
        ([System.Net.WebUtility]::UrlEncode([string]$key) + '=' + [System.Net.WebUtility]::UrlEncode([string]$Values[$key]))
    }
    return $items -join '&'
}

function Wait-PanelReady {
    param([string]$BaseUrl, [object]$Entry)

    $deadline = [DateTime]::UtcNow.AddSeconds(15)
    $lastProbe = 'no probe was sent'
    while ([DateTime]::UtcNow -lt $deadline) {
        if ($Entry.Process.HasExited) {
            $result = Wait-ChildProcess -Entry $Entry
            throw "Panel exited before becoming ready: $($result.Stderr)"
        }
        try {
            $response = Send-PanelRequest -Method GET -Uri "$BaseUrl/healthz"
            $lastProbe = "HTTP $($response.Status): $($response.Body)"
            if ($response.Status -eq 200) { return }
        } catch {
            $lastProbe = $_.Exception.Message
            Start-Sleep -Milliseconds 150
        }
    }
    throw "Panel did not become ready. Last probe: $lastProbe"
}

try {
    $script:binaryPath = Join-Path $runDir 'control.exe'
    & go build -o $script:binaryPath ./cmd/control
    if ($LASTEXITCODE -ne 0) { throw "go build failed with exit code $LASTEXITCODE." }

    $dataDir = Join-Path $runDir 'panel-data'
    $listener = [System.Net.Sockets.TcpListener]::new([System.Net.IPAddress]::Loopback, 0)
    $listener.Start()
    $port = ([System.Net.IPEndPoint]$listener.LocalEndpoint).Port
    $listener.Stop()
    $listenAddress = "127.0.0.1:$port"
    $baseUrl = "http://$listenAddress"

    $uninitialized = New-ChildProcess -Arguments @('serve', '--data-dir', $dataDir, '--listen', $listenAddress)
    $uninitializedResult = Wait-ChildProcess -Entry $uninitialized -TimeoutMilliseconds 15000
    if ($uninitializedResult.ExitCode -eq 0 -or $uninitializedResult.Stderr -notmatch '管理员尚未初始化') {
        throw 'Uninitialized Panel did not refuse startup with the expected local-init message.'
    }
    if (Test-ListeningPort -Port $port) { throw 'Uninitialized Panel opened a management listener.' }

    $password = [guid]::NewGuid().ToString('N') + 'Aa7!'
    $init = New-ChildProcess -Arguments @('admin', 'init', '--data-dir', $dataDir, '--password-stdin') -RedirectInput
    $init.Process.StandardInput.WriteLine($password)
    $init.Process.StandardInput.WriteLine($password)
    $init.Process.StandardInput.Close()
    $initResult = Wait-ChildProcess -Entry $init
    if ($initResult.ExitCode -ne 0 -or $initResult.Stdout -notmatch '管理员已初始化') {
        throw "Local administrator initialization failed: $($initResult.Stderr)"
    }

    $script:cookieJar = [System.Net.CookieContainer]::new()
    $handler = [System.Net.Http.HttpClientHandler]::new()
    $handler.CookieContainer = $script:cookieJar
    $handler.AllowAutoRedirect = $false
    $script:httpClient = [System.Net.Http.HttpClient]::new($handler)
    $script:httpClient.Timeout = [TimeSpan]::FromSeconds(5)

    $server = New-ChildProcess -Arguments @('serve', '--data-dir', $dataDir, '--listen', $listenAddress)
    Wait-PanelReady -BaseUrl $baseUrl -Entry $server
    $duplicateServer = New-ChildProcess -Arguments @('serve', '--data-dir', $dataDir, '--listen', $listenAddress)
    $duplicateResult = Wait-ChildProcess -Entry $duplicateServer
    if ($duplicateResult.ExitCode -eq 0 -or $duplicateResult.Stderr -notmatch 'another Panel process may be running') {
        throw 'A second Panel process was not rejected by the data-directory lock.'
    }

    $response = Send-PanelRequest -Method GET -Uri "$baseUrl/api/settings"
    if ($response.Status -ne 401) { throw "Unauthenticated API returned HTTP $($response.Status), expected 401." }

    $token = Get-LoginToken -BaseUrl $baseUrl
    $badForm = Encode-Form @{ csrf = $token; password = 'wrong test password' }
    $response = Send-PanelRequest -Method POST -Uri "$baseUrl/login" -Body $badForm -ContentType 'application/x-www-form-urlencoded' -Origin $baseUrl
    if ($response.Status -ne 401) { throw "Wrong password returned HTTP $($response.Status), expected 401." }

    $token = Get-LoginToken -BaseUrl $baseUrl
    $goodForm = Encode-Form @{ csrf = $token; password = $password }
    $response = Send-PanelRequest -Method POST -Uri "$baseUrl/login" -Body $goodForm -ContentType 'application/x-www-form-urlencoded' -Origin $baseUrl
    if ($response.Status -ne 303) { throw "Valid login returned HTTP $($response.Status), expected 303." }

    $response = Send-PanelRequest -Method PUT -Uri "$baseUrl/api/settings" -Body '{"site_name":"D02 smoke persisted"}' -ContentType 'application/json' -Origin $baseUrl
    if ($response.Status -ne 403) { throw "Settings API without CSRF returned HTTP $($response.Status), expected 403." }

    $csrf = Get-CsrfCookie -BaseUrl $baseUrl
    $response = Send-PanelRequest -Method PUT -Uri "$baseUrl/api/settings" -Body '{"site_name":"D02 smoke persisted"}' -ContentType 'application/json' -Origin 'http://attacker.invalid' -Csrf $csrf
    if ($response.Status -ne 403) { throw "Settings API with a mismatched Origin returned HTTP $($response.Status), expected 403." }

    $response = Send-PanelRequest -Method PUT -Uri "$baseUrl/api/settings" -Body '{"site_name":"D02 smoke persisted"}' -ContentType 'application/json' -Origin $baseUrl -Csrf $csrf
    if ($response.Status -ne 200) { throw "Settings save returned HTTP $($response.Status), expected 200." }

    $response = Send-PanelRequest -Method POST -Uri "$baseUrl/api/init" -Body '{"password":"claim attempt"}' -ContentType 'application/json' -Origin $baseUrl
    if ($response.Status -ne 405) { throw "Unexpected public initialization route returned HTTP $($response.Status)." }

    Stop-ChildProcess -Entry $server
    $server = New-ChildProcess -Arguments @('serve', '--data-dir', $dataDir, '--listen', $listenAddress)
    Wait-PanelReady -BaseUrl $baseUrl -Entry $server

    $response = Send-PanelRequest -Method GET -Uri "$baseUrl/api/settings"
    if ($response.Status -ne 401) { throw "A pre-restart session remained valid (HTTP $($response.Status))." }

    $token = Get-LoginToken -BaseUrl $baseUrl
    $goodForm = Encode-Form @{ csrf = $token; password = $password }
    $response = Send-PanelRequest -Method POST -Uri "$baseUrl/login" -Body $goodForm -ContentType 'application/x-www-form-urlencoded' -Origin $baseUrl
    if ($response.Status -ne 303) { throw "Login after restart returned HTTP $($response.Status)." }
    $response = Send-PanelRequest -Method GET -Uri "$baseUrl/api/settings"
    if ($response.Status -ne 200 -or $response.Body -notmatch 'D02 smoke persisted') {
        throw 'Saved settings did not survive restart.'
    }

    $csrf = Get-CsrfCookie -BaseUrl $baseUrl
    $logoutForm = Encode-Form @{ csrf = $csrf }
    $response = Send-PanelRequest -Method POST -Uri "$baseUrl/logout" -Body $logoutForm -ContentType 'application/x-www-form-urlencoded' -Origin $baseUrl
    if ($response.Status -ne 303) { throw "Logout returned HTTP $($response.Status), expected 303." }
    $response = Send-PanelRequest -Method GET -Uri "$baseUrl/api/settings"
    if ($response.Status -ne 401) { throw "Logout did not invalidate the session (HTTP $($response.Status))." }

    Stop-ChildProcess -Entry $server
    $server = $null

    $certPath = Join-Path $runDir 'localhost-cert.pem'
    $keyPath = Join-Path $runDir 'localhost-key.pem'
    $rsa = [System.Security.Cryptography.RSA]::Create(2048)
    $certificateRequest = [System.Security.Cryptography.X509Certificates.CertificateRequest]::new(
        'CN=127.0.0.1',
        $rsa,
        [System.Security.Cryptography.HashAlgorithmName]::SHA256,
        [System.Security.Cryptography.RSASignaturePadding]::Pkcs1
    )
    $subjectAlternativeNames = [System.Security.Cryptography.X509Certificates.SubjectAlternativeNameBuilder]::new()
    $subjectAlternativeNames.AddIpAddress([System.Net.IPAddress]::Loopback)
    $certificateRequest.CertificateExtensions.Add($subjectAlternativeNames.Build())
    $certificate = $certificateRequest.CreateSelfSigned([DateTimeOffset]::UtcNow.AddMinutes(-5), [DateTimeOffset]::UtcNow.AddHours(1))
    [System.IO.File]::WriteAllText($certPath, $certificate.ExportCertificatePem(), [System.Text.UTF8Encoding]::new($false))
    [System.IO.File]::WriteAllText($keyPath, $rsa.ExportPkcs8PrivateKeyPem(), [System.Text.UTF8Encoding]::new($false))
    $certificate.Dispose()
    $rsa.Dispose()

    $tlsListener = [System.Net.Sockets.TcpListener]::new([System.Net.IPAddress]::Loopback, 0)
    $tlsListener.Start()
    $tlsPort = ([System.Net.IPEndPoint]$tlsListener.LocalEndpoint).Port
    $tlsListener.Stop()
    $tlsAddress = "127.0.0.1:$tlsPort"
    $tlsBaseUrl = "https://$tlsAddress"
    $tlsServer = New-ChildProcess -Arguments @('serve', '--data-dir', $dataDir, '--listen', $tlsAddress) -Environment @{
        PANEL_TLS_CERT = $certPath
        PANEL_TLS_KEY = $keyPath
    }
    $server = $tlsServer
    $oldHttpClient = $script:httpClient
    $oldCookieJar = $script:cookieJar
    $tlsHandler = [System.Net.Http.HttpClientHandler]::new()
    $tlsCookieJar = [System.Net.CookieContainer]::new()
    $tlsHandler.CookieContainer = $tlsCookieJar
    $tlsHandler.AllowAutoRedirect = $false
    $tlsHandler.ServerCertificateCustomValidationCallback = [System.Net.Http.HttpClientHandler]::DangerousAcceptAnyServerCertificateValidator
    $script:httpClient = [System.Net.Http.HttpClient]::new($tlsHandler)
    $script:cookieJar = $tlsCookieJar
    $script:httpClient.Timeout = [TimeSpan]::FromSeconds(5)
    Wait-PanelReady -BaseUrl $tlsBaseUrl -Entry $tlsServer
    $tlsHealth = $script:httpClient.GetAsync("$tlsBaseUrl/healthz").GetAwaiter().GetResult()
    if ([int]$tlsHealth.StatusCode -ne 200 -or -not $tlsHealth.Headers.Contains('Strict-Transport-Security')) {
        $tlsHealth.Dispose()
        throw 'HTTPS health check failed or did not include HSTS.'
    }
    $tlsHealth.Dispose()
    $token = Get-LoginToken -BaseUrl $tlsBaseUrl
    $tlsLoginForm = Encode-Form @{ csrf = $token; password = $password }
    $response = Send-PanelRequest -Method POST -Uri "$tlsBaseUrl/login" -Body $tlsLoginForm -ContentType 'application/x-www-form-urlencoded' -Origin $tlsBaseUrl
    if ($response.Status -ne 303) { throw "HTTPS login returned HTTP $($response.Status)." }
    foreach ($cookieName in @('panel_session', 'panel_csrf')) {
        $cookie = $tlsCookieJar.GetCookies([uri]$tlsBaseUrl)[$cookieName]
        $cookieHeader = $response.SetCookies | Where-Object { $_ -match ('^' + [regex]::Escape($cookieName) + '=') } | Select-Object -First 1
        if ($null -eq $cookie -or -not $cookie.Secure -or -not $cookie.HttpOnly -or $cookieHeader -notmatch '(?i)(?:^|;\s*)SameSite=Strict(?:;|$)') {
            $cookieDetails = if ($null -eq $cookie) { 'missing' } else { "Secure=$($cookie.Secure), HttpOnly=$($cookie.HttpOnly), SameSite=$($cookie.SameSite)" }
            throw "HTTPS cookie '$cookieName' did not have Secure, HttpOnly, and SameSite=Strict attributes ($cookieDetails)."
        }
    }
    Stop-ChildProcess -Entry $tlsServer
    $server = $null
    $script:httpClient.Dispose()
    $script:httpClient = $oldHttpClient
    $script:cookieJar = $oldCookieJar

    $missingKeyDir = Join-Path $runDir 'missing-key-data'
    New-Item -ItemType Directory -Path $missingKeyDir | Out-Null
    $databasePath = Join-Path $dataDir 'panel.db'
    $missingDatabasePath = Join-Path $missingKeyDir 'panel.db'
    Copy-Item -LiteralPath $databasePath -Destination $missingDatabasePath
    $databaseHashBefore = (Get-FileHash -LiteralPath $missingDatabasePath -Algorithm SHA256).Hash
    $missingKey = New-ChildProcess -Arguments @('serve', '--data-dir', $missingKeyDir, '--listen', $listenAddress)
    $missingKeyResult = Wait-ChildProcess -Entry $missingKey
    if ($missingKeyResult.ExitCode -eq 0 -or $missingKeyResult.Stderr -notmatch '数据库已存在，但 master.key 缺失') {
        throw 'Existing database without a master key did not fail closed.'
    }
    $databaseHashAfter = (Get-FileHash -LiteralPath $missingDatabasePath -Algorithm SHA256).Hash
    if ($databaseHashAfter -ne $databaseHashBefore) { throw 'Database changed during missing-key startup refusal.' }
    if (Test-Path -LiteralPath (Join-Path $missingKeyDir 'master.key')) { throw 'A replacement master key was created.' }

    $success = $true
    Write-Output 'D02 smoke 通过：本机初始化、未初始化时不开端口、进程锁、登录错误与成功、认证和 CSRF 校验、设置重启持久化、注销失效、HTTPS 服务、缺失密钥时拒绝启动。'
} finally {
    foreach ($entry in $processes) {
        Stop-ChildProcess -Entry $entry
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
