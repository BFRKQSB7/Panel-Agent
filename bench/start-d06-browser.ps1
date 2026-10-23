# Local, disposable browser fixture. Its PID and data path are returned for cleanup.
$ErrorActionPreference='Stop'
$root=[IO.Path]::GetFullPath((Join-Path $PSScriptRoot '..'))
$work=Join-Path $PSScriptRoot '.work'
$dir=Join-Path $work ('d06-browser-'+[guid]::NewGuid().ToString('N'))
New-Item -ItemType Directory -Path $dir | Out-Null
$env:GOPATH=Join-Path $work 'gopath'; $env:GOCACHE=Join-Path $work 'gocache'; $env:GOMODCACHE=Join-Path $work 'gomodcache'
Push-Location $root
try { & go build -o "$dir/control.exe" ./cmd/control; if($LASTEXITCODE -ne 0){throw 'Build failed.'} } finally { Pop-Location }
$password='d06-local-browser-only-password'
$password,$password | & "$dir/control.exe" admin init --data-dir "$dir/data" --password-stdin
if($LASTEXITCODE -ne 0){throw 'Fixture initialization failed.'}
$p=Start-Process -FilePath "$dir/control.exe" -ArgumentList @('serve','--data-dir',"$dir/data",'--listen','127.0.0.1:38086') -WindowStyle Hidden -PassThru -RedirectStandardOutput "$dir/stdout.log" -RedirectStandardError "$dir/stderr.log"
[pscustomobject]@{pid=$p.Id;directory=$dir;url='http://127.0.0.1:38086'}|ConvertTo-Json|Set-Content "$work/d06-browser-active.json"
Get-Content "$work/d06-browser-active.json"
