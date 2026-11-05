param(
    [Parameter(Mandatory = $true)][string]$BinaryPath,
    [Parameter(Mandatory = $true)][string]$ConfigPath,
    [Parameter(Mandatory = $true)][string]$StatePath,
    [Parameter(Mandatory = $true)][string]$TaskId,
    [Parameter(Mandatory = $true)][string]$LogPath
)

$ErrorActionPreference = 'Stop'
$logDirectory = Split-Path -Parent $LogPath
New-Item -ItemType Directory -Force -Path $logDirectory | Out-Null

$timestamp = Get-Date -Format o
Add-Content -LiteralPath $LogPath -Value "$timestamp starting local T3 task $TaskId"

& $BinaryPath --config $ConfigPath --state $StatePath serve-local $TaskId 2>&1 |
    ForEach-Object {
        Add-Content -LiteralPath $LogPath -Value "$(Get-Date -Format o) $_"
    }

exit $LASTEXITCODE
