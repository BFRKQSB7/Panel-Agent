# Loaded by the existing isolated Iceland runner; all secrets stay in .work.
function Test-Subscriptions {
    $issue=Domain-API GET '/api/admin/user-tokens/action'
    $batch=@{kind='both';action_token=$issue.action_token;items=@(@{user_id=$normalUser.user_id;spec=@{}},@{user_id='missing-user';spec=@{}})}
    $delivery=Domain-API POST '/api/admin/user-tokens/batch' $batch
    $dash=($delivery.results|Where-Object{$_.kind -eq 'dashboard' -and $_.token}).token
    $sub=($delivery.results|Where-Object{$_.kind -eq 'subscription' -and $_.token}).token
    if(-not $dash -or -not $sub -or @($delivery.results|Where-Object error).Count -ne 2){throw 'Token partial delivery failed.'}
    $retry=Domain-API POST '/api/admin/user-tokens/batch' $batch
    if(@($retry.results|Where-Object token).Count -ne 0 -or @($retry.results|Where-Object already_applied).Count -ne 2){throw 'Token retry repeated a successful item.'}
    $page=Request GET "/u/$dash"
    if($page.Status -ne 200 -or -not $page.NoStore -or $page.ReferrerPolicy -ne 'no-referrer' -or $page.Body.Contains($sub)){throw 'Dashboard privacy failed.'}
    $uri=Request GET "/s/$sub/uri"
    $mh=Request GET "/s/$sub/mihomo"
    $sb=Request GET "/s/$sub/sing-box"
    if($uri.Status -ne 200 -or $mh.Status -ne 200 -or $sb.Status -ne 422 -or $sb.Body -notmatch 'unsupported_core_client_combination'){throw 'Subscription format support did not match the accepted contract.'}
    $lines=@($uri.Body.Trim().Split("`n")|Where-Object{$_})
    if($lines.Count -ne 2){throw 'URI authorization did not deduplicate overlapping groups.'}
    # Parse the actual URI export independently and adapt its fields to native Xray.
    $ins=@();$outs=@();$routes=@();$i=0
    foreach($line in $lines){
        $u=[uri]$line.Trim();$q=@{}
        foreach($part in $u.Query.TrimStart('?').Split('&')){$pair=$part.Split('=',2);$q[[Net.WebUtility]::UrlDecode($pair[0])]=[Net.WebUtility]::UrlDecode($pair[1])}
        if($u.Scheme -ne 'vless' -or $u.UserInfo -ne $normalUser.credential_uuid -or $u.Port -notin @($nodes[0].public_port,$nodes[1].public_port) -or $q.security -ne 'reality' -or $q.encryption -ne 'none' -or $q.type -ne 'tcp'){throw 'URI credential/public endpoint mismatch.'}
        $tag="export-$i"
        $ins+=@{listen='127.0.0.1';port=10808+$i;tag=$tag;protocol='socks';settings=@{auth='noauth';udp=$true}}
        $outs+=@{tag=$tag;protocol='vless';settings=@{vnext=@(@{address=$u.Host;port=$u.Port;users=@(@{id=$u.UserInfo;encryption=$q.encryption})})};streamSettings=@{network='raw';security=$q.security;realitySettings=@{serverName=$q.sni;fingerprint=$q.fp;password=$q.pbk;shortId=$q.sid}}}
        $routes+=@{type='field';inboundTag=@($tag);outboundTag=$tag};$i++
    }
    $script:clientDocument=@{log=@{loglevel='none'};inbounds=$ins;outbounds=$outs;routing=@{rules=$routes}}
    Reload-Client
    Proxy-Get 10808;Proxy-Get 10809
    $result.checks.exported_uri_two_authorized_nodes='PASS'
    Stop-Local $script:client
    $profile=$mh.Body|ConvertFrom-Json -AsHashtable
    if($profile.proxies.Count -ne 2 -or @($profile.proxies.name|Select-Object -Unique).Count -ne 2 -or $profile.proxies.uuid -contains $testUser.credential_uuid){throw 'Mihomo export permission mismatch.'}
    $dir=Join-Path $local 'mihomo-export';New-Item -ItemType Directory -Path $dir|Out-Null
    $path=Join-Path $dir 'config.json';[IO.File]::WriteAllText($path,$mh.Body)
    $binary=Join-Path $work 'clients/mihomo-1.19.33/mihomo-windows-amd64-compatible.exe'
    if(-not (Test-Path -LiteralPath $binary)){throw 'Verified Mihomo 1.19.33 cache is missing.'}
    if((Get-FileHash -LiteralPath $binary -Algorithm SHA256).Hash -ne '926BFEA2CE48A247D6A7CE4CF641980E416202D5E40FE4EE5C949089ACE0E197'){throw 'Mihomo executable does not match the verified 1.19.33 archive.'}
    $check=Start-Local $binary @('-t','-d',$dir,'-f',$path)
    if(-not $check.Process.WaitForExit(45000) -or $check.Process.ExitCode -ne 0){throw 'Native exported Mihomo check failed.'}
    # Only logging changes for observing the native routing decisions; all proxy,
    # rule-provider, and routing fields remain identical to the checked export.
    $profile['log-level']='info';[IO.File]::WriteAllText($path,($profile|ConvertTo-Json -Depth 20))
    $native=Start-Local $binary @('-d',$dir,'-f',$path)
    $target=Start-Local "$local/http-target.exe" @('--listen','127.0.0.1:18082')
    $deadline=[DateTime]::UtcNow.AddSeconds(45)
    while([DateTime]::UtcNow -lt $deadline){
        if($native.Process.HasExited){throw 'Mihomo exited before ready.'}
        if((Test-Path "$dir/rules/geosite-cn.mrs") -and (Test-Path "$dir/rules/geoip-cn.mrs")){break}
        Start-Sleep -Milliseconds 200
    }
    Proxy-Get 7890
    $lan=& curl.exe --fail --silent --show-error --max-time 10 --noproxy '' --proxy socks5h://127.0.0.1:7890 http://127.0.0.1:18082/
    if($LASTEXITCODE -ne 0 -or ($lan -join '') -ne 'Panel-Agent-D01-OK'){throw 'Client LAN DIRECT failed.'}
    # These external requests exercise routing; route decisions are checked even
    # if an unrelated destination TLS/network policy prevents the HTTP response.
    & curl.exe --silent --max-time 8 --noproxy '' --proxy socks5h://127.0.0.1:7890 https://www.baidu.com/ > $null 2>&1
    & curl.exe --silent --max-time 8 --noproxy '' --proxy socks5h://127.0.0.1:7890 https://223.5.5.5/ > $null 2>&1
    $result.checks.exported_mihomo_native_check_and_reality='PASS'
    $action=[regex]::Match($page.Body,'name="action_token" value="([^"]+)"').Groups[1].Value
    $reset=Domain-API POST "/u/$dash/subscription/reset" @{action_token=$action;spec=@{}}
    if(-not $reset.token -or (Request GET "/s/$sub/uri").Status -ne 404 -or (Request GET "/u/$dash").Status -ne 200){throw 'Independent URL reset failed.'}
    $current=Domain-API GET "/api/admin/users/$($normalUser.user_id)"
    if($current.user.credential_uuid -ne $normalUser.credential_uuid){throw 'URL reset rotated the connection credential.'}
    Proxy-Get 7890
    $result.checks.url_reset_preserves_cached_credential='PASS'
    [void](Domain-API POST "/api/admin/users/$($normalUser.user_id)/rotate")
    [void](Sync-Agent)
    Proxy-Get 7890 -Denied
    Stop-Local $native
    $log=$native.Out.GetAwaiter().GetResult()+$native.Err.GetAwaiter().GetResult()
    if($log -notmatch '127\.0\.0\.1:18082.*DIRECT' -or $log -notmatch 'www\.baidu\.com:443.*RuleSet\(cn-domain\).*DIRECT' -or $log -notmatch '223\.5\.5\.5:443.*RuleSet\(cn-ip\).*DIRECT' -or $log -notmatch 'example\.com:443.*PROXY'){throw 'Native route decisions did not match LAN/CN DIRECT and other PROXY.'}
    $result.checks.native_lan_cn_domain_cn_ip_and_other_routes='PASS'
    $fresh=Request GET "/s/$($reset.token)/mihomo"
    $updated=$fresh.Body|ConvertFrom-Json -AsHashtable
    if($updated.proxies.uuid -contains $normalUser.credential_uuid){throw 'Refreshed subscription retained the rotated credential.'}
    [IO.File]::WriteAllText($path,$fresh.Body)
    $native=Start-Local $binary @('-d',$dir,'-f',$path)
    Start-Sleep -Milliseconds 700
    Proxy-Get 7890
    $result.checks.credential_rotation_denies_cache_and_refresh_recovers='PASS'
    $panelLog=(Remote "cat '$remote/panel.log'").Text
    foreach($secret in @($dash,$sub,$reset.token)){if($panelLog.Contains($secret)){throw 'Sensitive URL leaked to server access log.'}}
    $result.checks.private_url_log_redaction='PASS'
    $result.checks.partial_token_retry='PASS'
    $result.checks.sing_box_reality_explicitly_disabled='PASS'
    $result.compatibility=[ordered]@{server='Xray 26.10.21';uri_adapter_client='Xray 26.10.21';mihomo='1.19.33 with support-x25519mlkem768';sing_box='1.14.3 Reality excluded after native failure';transport='SSH exec relay to isolated loopback listeners; no public NAT claim'}
    $result.rule_snapshots=@(Get-ChildItem "$dir/rules" -Filter '*.mrs'|ForEach-Object{@{file=$_.Name;sha256=(Get-FileHash $_.FullName -Algorithm SHA256).Hash.ToLowerInvariant()}})
    $result.passed=$true
}
