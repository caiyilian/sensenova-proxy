# ============================================================
# clash-node-helper.ps1
# Clash Verge (mihomo) 节点测试与自动切换工具
# 通过 mihomo 的命名管道 external-controller-pipe 控制代理节点
#
# 用法:
#   # 列出所有节点组及节点
#   ./clash-node-helper.ps1 -Action list
#
#   # 测试某个节点组的全部节点延迟(逐个临时切换后测, 按延迟排序)
#   ./clash-node-helper.ps1 -Action test -Group "节点选择"
#
#   # 直接切换到指定节点
#   ./clash-node-helper.ps1 -Action switch -Group "节点选择" -Node "HK-01"
#
#   # 自动修复: 找到第一个健康节点并切换, 然后用代理(7890)验证目标可达
#   ./clash-node-helper.ps1 -Action recover [-Url https://github.com] [-Group "节点选择"]
#
# 注意: 当前节点的 secret 从 clash-verge.yaml 读取, 只在进程内使用, 不会打印。
# ============================================================
param(
  [ValidateSet("list","test","switch","recover")][string]$Action = "list",
  [string]$Group,
  [string]$Node,
  [string]$Url = "https://www.gstatic.com/generate_204",
  [int]$TimeoutMs = 3000,
  [string]$PipeName = "verge-mihomo"
)

$cfgDir  = Join-Path $env:APPDATA "io.github.clash-verge-rev.clash-verge-rev"
$cfgFile = Join-Path $cfgDir "clash-verge.yaml"
if (-not (Test-Path -LiteralPath $cfgFile)) {
  Write-Error "config not found: $cfgFile"; exit 1
}
$secret = ((Select-String -LiteralPath $cfgFile -Pattern "^secret" | Select-Object -First 1).Line -split ":",2)[1].Trim()

function ConvertFrom-Chunked([string]$chunkedText) {
  $sb = New-Object System.Text.StringBuilder
  $i = 0; $len = $chunkedText.Length
  while ($i -lt $len) {
    $eol = $chunkedText.IndexOf("`n", $i)
    if ($eol -lt 0) { break }
    $line = $chunkedText.Substring($i, $eol - $i).Trim()
    $i = $eol + 1
    $semi = $line.IndexOf(';'); if ($semi -ge 0) { $line = $line.Substring(0, $semi) }
    $size = [Convert]::ToInt32($line, 16)
    if ($size -eq 0) { break }
    [void]$sb.Append($chunkedText.Substring($i, $size))
    $i += $size
    while ($i -lt $len -and ($chunkedText[$i] -eq "`r" -or $chunkedText[$i] -eq "`n")) { $i++ }
  }
  return $sb.ToString()
}

function Invoke-PipeHttp {
  param([string]$Method, [string]$PathAndQuery, [string]$Body)
  $pipe = New-Object System.IO.Pipes.NamedPipeClientStream(".", $PipeName,
          [System.IO.Pipes.PipeDirection]::InOut, [System.IO.Pipes.PipeOptions]::None)
  $pipe.Connect(5000)
  # build request as ASCII bytes (no BOM preamble)
  $sb = New-Object System.Text.StringBuilder
  [void]$sb.Append("$Method $PathAndQuery HTTP/1.1`r`n")
  [void]$sb.Append("Host: localhost`r`n")
  [void]$sb.Append("Connection: close`r`n")
  [void]$sb.Append("Authorization: Bearer $secret`r`n")
  if ($Body) {
    [void]$sb.Append("Content-Type: application/json`r`n")
    [void]$sb.Append("Content-Length: $(([Text.Encoding]::UTF8.GetBytes($Body)).Length)`r`n")
  }
  [void]$sb.Append("`r`n")
  $reqText = $sb.ToString()
  if ($Body) { $reqText += $Body }
  $reqBytes = [Text.Encoding]::UTF8.GetBytes($reqText)
  $pipe.Write($reqBytes, 0, $reqBytes.Length)
  # read response
  $sr = New-Object System.IO.StreamReader($pipe, [System.Text.Encoding]::UTF8)
  $raw = $sr.ReadToEnd()
  $pipe.Close()
  $splitIdx = $raw.IndexOf("`r`n`r`n")
  if ($splitIdx -lt 0) { return [PSCustomObject]@{ Status = "000"; Body = $raw } }
  $headerText = $raw.Substring(0, $splitIdx)
  $bodyRaw    = $raw.Substring($splitIdx + 4)
  $statusCode = ([regex]::Match($headerText, '^HTTP/1\.1\s+(\d+)')).Groups[1].Value
  if ($headerText -match 'Transfer-Encoding:\s*chunked') {
    # mihomo pipe chunked encoding is buggy; extract JSON between { and }
    $bs = $bodyRaw.IndexOf('{'); $be = $bodyRaw.LastIndexOf('}')
    if ($bs -ge 0 -and $be -gt $bs) { $body = $bodyRaw.Substring($bs, $be - $bs + 1) }
    else { $body = $bodyRaw }
  } else {
    $body = $bodyRaw
  }
  return [PSCustomObject]@{ Status = $statusCode; Body = $body }
}

function Get-ProxiesJson {
  $r = Invoke-PipeHttp GET "/proxies"
  if ($r.Status -ne "200") { Write-Error "GET /proxies failed: $($r.Body)"; exit 1 }
  return ($r.Body | ConvertFrom-Json)
}

function Url-Encode([string]$s) {
  return [Uri]::EscapeDataString($s)
}

# ---------- list ----------
if ($Action -eq "list") {
  $p = Get-ProxiesJson
  $rows = @()
  foreach ($prop in $p.proxies.PSObject.Properties) {
    $v = $prop.Value
    if ($v.type -in @("Selector","URLTest","Fallback")) {
      $rows += [PSCustomObject]@{ Type=$v.type; Group=$prop.Name; Current=$v.now; Nodes=@($v.all).Count }
    }
  }
  $rows | Format-Table -AutoSize
  exit 0
}

# ---------- switch ----------
if ($Action -eq "switch") {
  if (-not $Group -or -not $Node) { Write-Error "switch 需要 -Group 和 -Node"; exit 1 }
  $body = @{ name = $Node } | ConvertTo-Json -Compress
  $r = Invoke-PipeHttp PUT ("/proxies/" + (Url-Encode $Group)) $body
  if ($r.Status -eq "204") { Write-Host "OK 已切换: $Group -> $Node" }
  else { Write-Host "切换结果: HTTP $($r.Status)  body=$($r.Body)" }
  exit 0
}

# ---------- test: 测组内全部节点延迟 ----------
if ($Action -eq "test") {
  if (-not $Group) { Write-Error "test 需要 -Group"; exit 1 }
  $p = Get-ProxiesJson
  $members = @($p.proxies.PSObject.Properties[$Group].Value.all | Where-Object {
    $_ -notin @("DIRECT","REJECT","PASS","COMPATIBLE","自动选择","故障转移") -and
    $_ -notmatch "剩余流量|距离下次重置|套餐到期"
  })
  if (-not $members -or $members.Count -eq 0) { Write-Error "group 未找到或为空: $Group"; exit 1 }
  Write-Host "测试 $Group ($($members.Count) 个节点) -> $Url (timeout=$TimeoutMs ms)..."
  $prev = $p.proxies.PSObject.Properties[$Group].Value.now
  $results = @()
  foreach ($m in $members) {
    # 临时切换到该节点
    $sw = Invoke-PipeHttp PUT ("/proxies/" + (Url-Encode $Group)) (@{name=$m} | ConvertTo-Json -Compress)
    if ($sw.Status -ne "204") { $results += [PSCustomObject]@{ Node=$m; Delay="switch fail"; OK=$false }; continue }
    $q = "/proxies/" + (Url-Encode $Group) + "/delay?url=" + (Url-Encode $Url) + "&timeout=$TimeoutMs"
    $r = Invoke-PipeHttp GET $q
    $delay = ""
    $ok = $false
    if ($r.Status -eq "200") { try { $delay = ([regex]::Match($r.Body,'"delay":(\d+)')).Groups[1].Value; $ok = $true } catch {} }
    if (-not $ok) { $delay = "timeout/fail ($($r.Status))" }
    $results += [PSCustomObject]@{ Node=$m; Delay=$delay; OK=$ok }
  }
  # 恢复原节点
  [void](Invoke-PipeHttp PUT ("/proxies/" + (Url-Encode $Group)) (@{name=$prev} | ConvertTo-Json -Compress))
  $results | Sort-Object @{Expression={ if($_.OK){[int]$_.Delay} else {999999} }} | Format-Table -AutoSize
  $good = @($results | Where-Object { $_.OK })
  Write-Host "健康节点: $($good.Count)/$($results.Count). 最快: $($good[0].Node) ($($good[0].Delay)ms)" -ForegroundColor Cyan
  exit 0
}

# ---------- recover: 找健康节点并切换, 再用 7890 验证 ----------
if ($Action -eq "recover") {
  Write-Host "获取节点列表..." -ForegroundColor Cyan
  $p = Get-ProxiesJson
  if (-not $Group) {
    $cand = @($p.proxies.PSObject.Properties | Where-Object { $_.Value.type -eq "Selector" -and $_.Name -ne "GLOBAL" } |
             Sort-Object @{Expression={
               $nodes = @($_.Value.all | Where-Object { $_ -notin @("DIRECT","REJECT","PASS","COMPATIBLE") }).Count
               $prio = if($_.Name -match "良心云|选择|Proxy|国外|外网"){0}else{1}
               $prio * 10000 - $nodes
             }} | Select-Object -First 1)
    if (-not $cand) { Write-Error "未找到 Selector 组"; exit 1 }
    $Group = $cand[0].Name
    Write-Host "自动选择节点组: $Group"
  }
  $members = @($p.proxies.PSObject.Properties[$Group].Value.all | Where-Object {
    $_ -notin @("DIRECT","REJECT","PASS","COMPATIBLE","自动选择","故障转移") -and
    $_ -notmatch "剩余流量|距离下次重置|套餐到期"
  })
  $prev = $p.proxies.PSObject.Properties[$Group].Value.now
  Write-Host "从 $($members.Count) 个节点里找健康节点... (目标 $Url, 单节点超时 $TimeoutMs ms)"
  $results = @()
  $healthyTarget = 5
  foreach ($m in $members) {
    $q = "/proxies/" + (Url-Encode $m) + "/delay?url=" + (Url-Encode $Url) + "&timeout=$TimeoutMs"
    $r = Invoke-PipeHttp GET $q
    if ($r.Status -eq "200") {
      $d = ([regex]::Match($r.Body,'"delay":(\d+)')).Groups[1].Value
      $results += [PSCustomObject]@{ Node=$m; Delay=[int]$d }
      Write-Host "  + $m : $d ms" -ForegroundColor Green
      if ($results.Count -ge $healthyTarget) {
        Write-Host "已找到 $healthyTarget 个健康节点，提前选优以缩短恢复时间。"
        break
      }
    } else {
      Write-Host "  - $m : timeout" -ForegroundColor DarkGray
    }
  }
  $best = $results | Sort-Object Delay | Select-Object -First 1
  if (-not $best) { Write-Error "所有节点不可用"; exit 1 }
  Write-Host "选用: $($best.Node) ($($best.Delay) ms)" -ForegroundColor Cyan
  # 切换节点 (不读响应, 直接关管道, 避免挂起)
  $body = @{name=$best.Node} | ConvertTo-Json -Compress
  $req = "PUT /proxies/" + (Url-Encode $Group) + " HTTP/1.1`r`nHost: localhost`r`nConnection: close`r`nAuthorization: Bearer $secret`r`nContent-Type: application/json`r`nContent-Length: $([Text.Encoding]::UTF8.GetBytes($body).Length)`r`n`r`n$body"
  $reqBytes = [Text.Encoding]::UTF8.GetBytes($req)
  $pipe = New-Object System.IO.Pipes.NamedPipeClientStream(".",$PipeName,[System.IO.Pipes.PipeDirection]::Out,[System.IO.Pipes.PipeOptions]::None)
  $pipe.Connect(5000); $pipe.Write($reqBytes,0,$reqBytes.Length); $pipe.Close()
  Start-Sleep -Milliseconds 500
  # 验证
  try {
    $resp = curl.exe -s -x "http://127.0.0.1:7890" --max-time 15 -o NUL -w "%{http_code}" $Url
  } catch {}
  $code = ($resp | Out-String).Trim()
  if ($code -eq "000") {
    Write-Host "警告: 7890 验证失败, 但节点已切换" -ForegroundColor Yellow
    exit 0
  }
  Write-Host "验证通过: 7890 -> $Url -> HTTP $code" -ForegroundColor Green
  if ($prev -ne $best.Node) { Write-Host "原节点: $prev -> 已切换: $($best.Node)" }
  exit 0
}
