param(
    [Parameter(Mandatory = $true)][string]$BinaryPath,
    [Parameter(Mandatory = $true)][string]$ConfigPath,
    [Parameter(Mandatory = $true)][string]$StatePath,
    [Parameter(Mandatory = $true)][ValidatePattern('^[A-Za-z0-9][A-Za-z0-9_-]{0,95}$')][string]$TaskId,
    [Parameter(Mandatory = $true)][ValidateRange(1024, 65535)][int]$LocalPort,
    [Parameter(Mandatory = $true)][ValidateRange(1024, 65535)][int]$TailscaleHttpsPort,
    [string]$TailscalePath = 'C:\Program Files\Tailscale\tailscale.exe'
)

$ErrorActionPreference = 'Stop'
$root = Split-Path -Parent $PSScriptRoot
$runner = Join-Path $PSScriptRoot 'run-local-task.ps1'
$taskName = "InstaboxTicketRunner-$TaskId"
$logPath = Join-Path $StatePath "service-$TaskId.log"
$pwsh = Join-Path $PSHOME 'pwsh.exe'

foreach ($path in @($BinaryPath, $ConfigPath, $runner, $TailscalePath, $pwsh)) {
    if (!(Test-Path -LiteralPath $path -PathType Leaf)) {
        throw "required file is missing: $path"
    }
    if ($path.Contains('"')) {
        throw 'paths containing quotes are not supported'
    }
}

New-Item -ItemType Directory -Force -Path $StatePath | Out-Null
& $BinaryPath --config $ConfigPath --state $StatePath status $TaskId | Out-Null
if ($LASTEXITCODE -ne 0) {
    throw 'local task is not prepared'
}

$arguments = '-NoLogo -NoProfile -NonInteractive -ExecutionPolicy Bypass -File "{0}" -BinaryPath "{1}" -ConfigPath "{2}" -StatePath "{3}" -TaskId "{4}" -LogPath "{5}"' -f $runner, $BinaryPath, $ConfigPath, $StatePath, $TaskId, $logPath
$action = New-ScheduledTaskAction -Execute $pwsh -Argument $arguments -WorkingDirectory $root
$identity = [System.Security.Principal.WindowsIdentity]::GetCurrent().Name
$principal = New-ScheduledTaskPrincipal -UserId $identity -LogonType Interactive -RunLevel Limited
$trigger = New-ScheduledTaskTrigger -AtLogOn -User $identity
$settings = New-ScheduledTaskSettingsSet -AllowStartIfOnBatteries -DontStopIfGoingOnBatteries -StartWhenAvailable -RestartCount 999 -RestartInterval (New-TimeSpan -Minutes 1) -ExecutionTimeLimit ([TimeSpan]::Zero) -MultipleInstances IgnoreNew

Register-ScheduledTask -TaskName $taskName -Action $action -Principal $principal -Trigger $trigger -Settings $settings -Description "Persistent local T3/Codex worker for $TaskId" -Force | Out-Null
Start-ScheduledTask -TaskName $taskName

$ready = $false
for ($attempt = 0; $attempt -lt 60; $attempt++) {
    try {
        $response = Invoke-WebRequest -UseBasicParsing -Uri "http://127.0.0.1:$LocalPort/.well-known/t3/environment" -TimeoutSec 2
        if ($response.StatusCode -ge 200 -and $response.StatusCode -lt 300) {
            $ready = $true
            break
        }
    } catch {
    }
    Start-Sleep -Seconds 1
}
if (!$ready) {
    throw "scheduled task started but local T3 did not become ready; inspect $logPath"
}

& $TailscalePath serve --bg --yes "--https=$TailscaleHttpsPort" "http://127.0.0.1:$LocalPort" | Out-Null
if ($LASTEXITCODE -ne 0) {
    throw 'Tailscale Serve configuration failed'
}
$tailscaleStatus = (& $TailscalePath status --json | ConvertFrom-Json)
$tailnetHost = $tailscaleStatus.Self.DNSName.TrimEnd('.')

[pscustomobject]@{
    task = $taskName
    state = (Get-ScheduledTask -TaskName $taskName).State.ToString()
    local = "http://127.0.0.1:$LocalPort"
    tailnet = "https://$tailnetHost`:$TailscaleHttpsPort"
    log = $logPath
}
