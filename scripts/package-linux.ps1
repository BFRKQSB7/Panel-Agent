#requires -Version 7.0
param(
    [Parameter(Mandatory)][string]$OutputDirectory,
    [Parameter(Mandatory)][string]$XrayAmd64Package,
    [Parameter(Mandatory)][string]$XrayArm64Package,
    [string]$HysteriaAmd64Binary,
    [string]$HysteriaArm64Binary,
    [string]$HysteriaLicense,
    [string]$WslDistribution = 'Debian'
)
$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest
$repository = [IO.Path]::GetFullPath((Join-Path $PSScriptRoot '..'))
$output = [IO.Path]::GetFullPath($OutputDirectory)
$withHysteria = [bool]($HysteriaAmd64Binary -or $HysteriaArm64Binary -or $HysteriaLicense)
if ($withHysteria -and (-not $HysteriaAmd64Binary -or -not $HysteriaArm64Binary -or -not $HysteriaLicense)) { throw 'Hysteria 打包需要同时提供两个架构的官方二进制与许可证。' }
if ($withHysteria -and (Get-FileHash -Algorithm SHA256 -LiteralPath $HysteriaLicense).Hash.ToLowerInvariant() -ne 'b279cfdac4db4b077f0660b5d8156d50a8bc7bd410036dc356499af43c4e84f5') { throw '官方 Hysteria 许可证校验失败。' }
if (Test-Path -LiteralPath $output) { throw '输出目录必须不存在，避免覆盖已有产物。' }
$versionMatch = [regex]::Match([IO.File]::ReadAllText((Join-Path $repository 'internal/protocol/messages.go')), 'const AgentVersion = "([0-9.]+)"')
if (-not $versionMatch.Success) { throw '未找到程序版本。' }
$version = $versionMatch.Groups[1].Value
$saved = @{}
foreach ($name in @('GOOS','GOARCH','CGO_ENABLED','GOTOOLCHAIN')) { $saved[$name] = [Environment]::GetEnvironmentVariable($name) }
function WslPath([string]$Path) {
    $mapped = (& wsl.exe -d $WslDistribution -- wslpath -a -u $Path.Replace('\','/') | Out-String).Trim()
    if ($LASTEXITCODE -ne 0 -or -not $mapped.StartsWith('/')) { throw "无法转换 WSL 路径：$Path" }
    return $mapped
}
Push-Location $repository
try {
    $env:GOTOOLCHAIN = 'go1.27.1'
    $goVersion = (& go version | Out-String).Trim()
    if ($LASTEXITCODE -ne 0 -or $goVersion -notmatch '^go version go1\.27\.1 ') { throw '要求 Go 1.27.1。' }
    $revision = (& git rev-parse HEAD | Out-String).Trim()
    $epoch = (& git show -s --format=%ct HEAD | Out-String).Trim()
    New-Item -ItemType Directory -Path $output | Out-Null
    $archives = @()
    foreach ($arch in @('amd64','arm64')) {
        $env:GOOS = 'linux'; $env:GOARCH = $arch; $env:CGO_ENABLED = '0'
        $bundle = Join-Path $output "panel-agent-$version-linux-$arch"
        New-Item -ItemType Directory -Path $bundle | Out-Null
        foreach ($program in @(@{Name='control';Path='./cmd/control'}, @{Name='control-agent';Path='./cmd/agent'}, @{Name='panel-agent-installer';Path='./cmd/installer'})) {
            & go build -trimpath -ldflags='-s -w' -o (Join-Path $bundle $program.Name) $program.Path
            if ($LASTEXITCODE -ne 0) { throw "构建失败：$arch/$($program.Name)" }
        }
        $zipPath = (Resolve-Path -LiteralPath $(if ($arch -eq 'amd64') { $XrayAmd64Package } else { $XrayArm64Package })).Path
        $expected = if ($arch -eq 'amd64') { '0eec3ed4163c273d8c8bf821f7d219e257921cf584aa3fb970715e458e631978' } else { 'd1304946c486d98e6b886e9b931eaaaebb23835b30e217638037b7f2721d4e9c' }
        if ((Get-FileHash -Algorithm SHA256 -LiteralPath $zipPath).Hash.ToLowerInvariant() -ne $expected) { throw "官方 Xray 26.10.21 包校验失败：$arch" }
        $licenseRoot = Join-Path $bundle 'LICENSES'
        New-Item -ItemType Directory -Path $licenseRoot | Out-Null
        $zip = [IO.Compression.ZipFile]::OpenRead($zipPath)
        try {
            foreach ($entryName in @('xray','LICENSE')) {
                $entry = $zip.GetEntry($entryName)
                if ($null -eq $entry) { throw "官方包缺少 $entryName" }
                $destination = if ($entryName -eq 'xray') { Join-Path $bundle 'xray' } else { Join-Path $licenseRoot 'Xray-26.10.21-LICENSE' }
                $inputStream = $entry.Open(); $outputStream = [IO.File]::Create($destination)
                try { $inputStream.CopyTo($outputStream) } finally { $inputStream.Dispose(); $outputStream.Dispose() }
            }
        } finally { $zip.Dispose() }
        $hysteriaManifest = $null
        if ($withHysteria) {
            $hySource = if ($arch -eq 'amd64') { $HysteriaAmd64Binary } else { $HysteriaArm64Binary }
            $hyExpected = if ($arch -eq 'amd64') { '907ba8c9693edb104b20582681fb7dc15639d5b64a9cbb616a7b539190a86691' } else { 'a68a61a84452ca250ce0368202521965ca9cc9d801a404f1dc9008ac6cf677a7' }
            if ((Get-FileHash -Algorithm SHA256 -LiteralPath $hySource).Hash.ToLowerInvariant() -ne $hyExpected) { throw "官方 Hysteria v2.13.0 校验失败：$arch" }
            Copy-Item -LiteralPath $hySource -Destination (Join-Path $bundle 'hysteria')
            Copy-Item -LiteralPath $HysteriaLicense -Destination (Join-Path $licenseRoot 'Hysteria-2.13.0-LICENSE')
            $extensionDir = Join-Path $bundle 'protocol-extensions'
            New-Item -ItemType Directory -Path $extensionDir | Out-Null
            $extensionZip = [IO.Compression.ZipFile]::Open((Join-Path $extensionDir 'official-hysteria2.zip'),[IO.Compression.ZipArchiveMode]::Create)
            try {
                [IO.Compression.ZipFileExtensions]::CreateEntryFromFile($extensionZip,(Join-Path $repository 'examples/extensions/official-hysteria2/package.json'),'package.json') | Out-Null
            } finally { $extensionZip.Dispose() }
            $hysteriaManifest = [ordered]@{version='2.13.0';binary_sha256=$hyExpected;source='https://github.com/HyNetworks/hysteria/releases/tag/app/v2.13.0'}
        }
        $modules = @(& go list -deps -f '{{with .Module}}{{.Path}}|{{.Version}}|{{.Dir}}{{end}}' ./cmd/control ./cmd/agent ./cmd/installer | Where-Object { $_ } | Sort-Object -Unique)
        if ($LASTEXITCODE -ne 0) { throw '依赖枚举失败。' }
        $licenseIndex = @()
        foreach ($module in $modules) {
            $parts = $module -split '\|'
            if ($parts.Count -ne 3 -or -not $parts[1]) { continue }
            $files = @(Get-ChildItem -LiteralPath $parts[2] -File | Where-Object Name -Match '^(LICENSE|COPYING|NOTICE)(\..*|-.*)?$')
            if ($files.Count -eq 0) { throw "缺少依赖许可证：$($parts[0])" }
            $folder = ($parts[0] + '@' + $parts[1]) -replace '[^A-Za-z0-9_.-]', '_'
            $destination = Join-Path $licenseRoot $folder
            New-Item -ItemType Directory -Path $destination | Out-Null
            foreach ($file in $files) { Copy-Item -LiteralPath $file.FullName -Destination $destination }
            $licenseIndex += [ordered]@{module=$parts[0];version=$parts[1];files=@($files | ForEach-Object { "LICENSES/$folder/$($_.Name)" })}
        }
        $goRoot = (& go env GOROOT | Out-String).Trim()
        Copy-Item -LiteralPath (Join-Path $goRoot 'LICENSE') -Destination (Join-Path $licenseRoot 'Go-LICENSE')
        foreach ($name in @('install.sh','uninstall.sh','cleanup.sh')) {
            $contents = [IO.File]::ReadAllText((Join-Path $PSScriptRoot $name)).Replace("`r`n","`n")
            [IO.File]::WriteAllText((Join-Path $bundle $name),$contents,[Text.UTF8Encoding]::new($false))
        }
        Copy-Item -LiteralPath (Join-Path $repository 'docs') -Destination (Join-Path $bundle 'docs') -Recurse
        Copy-Item -LiteralPath (Join-Path $repository 'README.md') -Destination (Join-Path $bundle 'README.md')
        $manifest = [ordered]@{version=$version;platform="linux/$arch";source_revision=$revision;go=$goVersion;status='unaccepted_local_candidate';project_license='not_declared';xray=[ordered]@{version='26.10.21';package_sha256=$expected;source='https://github.com/XTLS/Xray-core/releases/tag/v26.10.21'};dependencies=$licenseIndex}
        if ($withHysteria) { $manifest['hysteria'] = $hysteriaManifest }
        [IO.File]::WriteAllText((Join-Path $bundle 'manifest.json'),($manifest | ConvertTo-Json -Depth 8),[Text.UTF8Encoding]::new($false))
        $checksums = @(Get-ChildItem -LiteralPath $bundle -File -Recurse | Sort-Object FullName | ForEach-Object { (Get-FileHash -Algorithm SHA256 -LiteralPath $_.FullName).Hash.ToLowerInvariant() + '  ' + [IO.Path]::GetRelativePath($bundle,$_.FullName).Replace('\','/') })
        [IO.File]::WriteAllText((Join-Path $bundle 'SHA256SUMS'),($checksums -join "`n") + "`n",[Text.UTF8Encoding]::new($false))
        $archive = $bundle + '.tar.gz'
        & wsl.exe -d $WslDistribution -- sh (WslPath (Join-Path $PSScriptRoot 'archive-linux.sh')) (WslPath $bundle) (WslPath $archive) $epoch
        if ($LASTEXITCODE -ne 0) { throw "归档失败：$arch" }
        $archives += (Get-FileHash -Algorithm SHA256 -LiteralPath $archive).Hash.ToLowerInvariant() + '  ' + [IO.Path]::GetFileName($archive)
        Write-Output "本地候选包已生成：$archive"
    }
    [IO.File]::WriteAllText((Join-Path $output 'SHA256SUMS'),($archives -join "`n") + "`n",[Text.UTF8Encoding]::new($false))
} finally {
    foreach ($name in $saved.Keys) { [Environment]::SetEnvironmentVariable($name,$saved[$name]) }
    Pop-Location
}
