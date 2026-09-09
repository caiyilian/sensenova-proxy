[CmdletBinding()]
param(
  [string]$TaskName = 'Agnes Resilient Gateway'
)

$ErrorActionPreference = 'Stop'

$repoRoot = (Resolve-Path -LiteralPath (Join-Path $PSScriptRoot '..')).Path
$entryPath = Join-Path $repoRoot 'agnes-proxy.js'
$nodePath = (Get-Command node -ErrorAction Stop).Source
$apiKey = [Environment]::GetEnvironmentVariable('AGNES_API_KEY', 'Process')
if (-not $apiKey) {
  $apiKey = [Environment]::GetEnvironmentVariable('AGNES_API_KEY', 'User')
}

if (-not (Test-Path -LiteralPath $entryPath -PathType Leaf)) {
  throw "Gateway entry point not found: $entryPath"
}
if (-not $apiKey) {
  throw 'AGNES_API_KEY is not set in the process or user environment.'
}

$identity = [Security.Principal.WindowsIdentity]::GetCurrent().Name
$action = New-ScheduledTaskAction `
  -Execute $nodePath `
  -Argument ('"{0}"' -f $entryPath) `
  -WorkingDirectory $repoRoot
$trigger = New-ScheduledTaskTrigger -AtLogOn -User $identity
$principal = New-ScheduledTaskPrincipal `
  -UserId $identity `
  -LogonType Interactive `
  -RunLevel Limited
$settings = New-ScheduledTaskSettingsSet `
  -AllowStartIfOnBatteries `
  -DontStopIfGoingOnBatteries `
  -ExecutionTimeLimit ([TimeSpan]::Zero) `
  -Hidden `
  -MultipleInstances IgnoreNew `
  -RestartCount 3 `
  -RestartInterval (New-TimeSpan -Minutes 1)

$existing = Get-ScheduledTask -TaskName $TaskName -ErrorAction SilentlyContinue
if ($existing -and $existing.State -eq 'Running') {
  Stop-ScheduledTask -TaskName $TaskName
  for ($attempt = 0; $attempt -lt 50; $attempt += 1) {
    if ((Get-ScheduledTask -TaskName $TaskName).State -ne 'Running') { break }
    Start-Sleep -Milliseconds 100
  }

  if ((Get-ScheduledTask -TaskName $TaskName).State -eq 'Running') {
    $gatewayProcesses = Get-CimInstance Win32_Process -Filter "Name = 'node.exe'" |
      Where-Object {
        $_.CommandLine -and
        $_.CommandLine.IndexOf($entryPath, [StringComparison]::OrdinalIgnoreCase) -ge 0
      }
    foreach ($gatewayProcess in $gatewayProcesses) {
      Stop-Process -Id $gatewayProcess.ProcessId -Force
    }
  }
}

Register-ScheduledTask `
  -TaskName $TaskName `
  -Action $action `
  -Trigger $trigger `
  -Principal $principal `
  -Settings $settings `
  -Description 'Local OpenAI-compatible Agnes gateway with direct/Clash failover and node recovery.' `
  -Force | Out-Null

Start-ScheduledTask -TaskName $TaskName

$health = $null
for ($attempt = 0; $attempt -lt 40; $attempt += 1) {
  Start-Sleep -Milliseconds 250
  try {
    $health = Invoke-RestMethod -Uri 'http://127.0.0.1:18788/health' -TimeoutSec 2
    break
  } catch {
    # The gateway may still be starting.
  }
}

if (-not $health) {
  throw 'Scheduled task was registered, but the Agnes gateway health check did not become ready.'
}

[pscustomobject]@{
  TaskName = $TaskName
  State = (Get-ScheduledTask -TaskName $TaskName).State
  Endpoint = 'http://127.0.0.1:18788/v1'
  PreferredRoute = $health.routes.preferred
}
