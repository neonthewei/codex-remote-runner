param(
    [Parameter(Mandatory = $true)][ValidatePattern('^[A-Za-z0-9][A-Za-z0-9_-]{0,95}$')][string]$TaskId
)

$ErrorActionPreference = 'Stop'
$taskName = "InstaboxTicketRunner-$TaskId"
$task = Get-ScheduledTask -TaskName $taskName -ErrorAction SilentlyContinue
if ($task) {
    Stop-ScheduledTask -TaskName $taskName -ErrorAction SilentlyContinue
    Unregister-ScheduledTask -TaskName $taskName -Confirm:$false
}

[pscustomobject]@{
    task = $taskName
    removed = $true
    state_preserved = $true
    tailscale_listener_preserved = $true
}
