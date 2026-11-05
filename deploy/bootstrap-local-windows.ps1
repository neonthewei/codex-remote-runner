[CmdletBinding()]
param(
    [Parameter(Mandatory = $true)][string]$LocalRepoRoot,
    [Parameter(Mandatory = $true)][ValidatePattern('^[A-Za-z0-9][A-Za-z0-9_-]{0,95}$')][string]$TaskId,
    [string]$Repository,
    [string]$Base = 'main',
    [string]$Profile = 'local-codex',
    [ValidateRange(1024, 65535)][int]$LocalPort = 3784,
    [ValidateRange(1024, 65535)][int]$TailscaleHttpsPort = 10000,
    [string]$WorktreeRoot,
    [string]$ConfigPath,
    [string]$StatePath,
    [string]$BinaryPath,
    [string]$T3Path,
    [string]$CodexPath,
    [string]$TailscalePath = 'C:\Program Files\Tailscale\tailscale.exe',
    [string]$SkillRoot,
    [switch]$Rebuild
)

$ErrorActionPreference = 'Stop'
$checkoutRoot = Split-Path -Parent $PSScriptRoot

function Resolve-Executable {
    param([string]$ExplicitPath, [string]$CommandName, [string[]]$Candidates)

    if ($ExplicitPath) {
        if (!(Test-Path -LiteralPath $ExplicitPath -PathType Leaf)) {
            throw "required executable is missing: $ExplicitPath"
        }
        return (Resolve-Path -LiteralPath $ExplicitPath).Path
    }

    $command = Get-Command $CommandName -ErrorAction SilentlyContinue | Select-Object -First 1
    if ($command -and $command.Source -and (Test-Path -LiteralPath $command.Source -PathType Leaf)) {
        return $command.Source
    }

    $match = Get-ChildItem -Path $Candidates -File -ErrorAction SilentlyContinue |
        Sort-Object LastWriteTimeUtc -Descending |
        Select-Object -First 1
    if ($match) {
        return $match.FullName
    }
    throw "could not locate $CommandName; install it or pass its explicit path"
}

function Quote-Yaml {
    param([string]$Value)
    return "'" + $Value.Replace("'", "''") + "'"
}

function Invoke-Controller {
    param([Parameter(ValueFromRemainingArguments = $true)][string[]]$Arguments)
    & $BinaryPath --config $ConfigPath --state $StatePath @Arguments
    if ($LASTEXITCODE -ne 0) {
        throw "codex-remote failed: $($Arguments -join ' ')"
    }
}

$gitPath = Resolve-Executable -CommandName 'git.exe' -Candidates @()
$goPath = Resolve-Executable -CommandName 'go.exe' -Candidates @(
    (Join-Path $env:USERPROFILE '.cache\go*\go\bin\go.exe')
)
$TailscalePath = Resolve-Executable -ExplicitPath $TailscalePath -CommandName 'tailscale.exe' -Candidates @()
$T3Path = Resolve-Executable -ExplicitPath $T3Path -CommandName 't3.exe' -Candidates @(
    (Join-Path $env:USERPROFILE '.t3\runtime\versions\*\t3.exe')
)
$CodexPath = Resolve-Executable -ExplicitPath $CodexPath -CommandName 'codex.exe' -Candidates @(
    (Join-Path $env:LOCALAPPDATA 'OpenAI\Codex\bin\*\codex.exe')
)

$goVersionText = (& $goPath version) -join ' '
if ($goVersionText -notmatch 'go(\d+\.\d+(?:\.\d+)?)') {
    throw "could not read Go version: $goVersionText"
}
if ([version]$Matches[1] -lt [version]'1.26.10') {
    throw "Go 1.26.10 or newer is required; found $($Matches[1])"
}

$LocalRepoRoot = (Resolve-Path -LiteralPath $LocalRepoRoot).Path
& $gitPath -C $LocalRepoRoot rev-parse --show-toplevel | Out-Null
if ($LASTEXITCODE -ne 0) {
    throw "local repository is not a Git checkout: $LocalRepoRoot"
}

if (!$Repository) {
    $origin = (& $gitPath -C $LocalRepoRoot remote get-url origin) -join ''
    if ($LASTEXITCODE -ne 0 -or $origin -notmatch '(?:github\.com[/:])([^/]+/[^/]+?)(?:\.git)?$') {
        throw 'could not derive OWNER/REPO from origin; pass -Repository explicitly'
    }
    $Repository = $Matches[1]
}

$privateRoot = Join-Path $env:USERPROFILE '.codex\state\codex-remote-runner'
if (!$ConfigPath) { $ConfigPath = Join-Path $privateRoot "$TaskId.yaml" }
if (!$StatePath) { $StatePath = Join-Path $privateRoot $TaskId }
if (!$WorktreeRoot) { $WorktreeRoot = Join-Path $env:USERPROFILE ".codex\worktrees\remote-$TaskId" }
if (!$BinaryPath) { $BinaryPath = Join-Path $env:USERPROFILE '.local\bin\codex-remote.exe' }

$ConfigPath = [System.IO.Path]::GetFullPath($ConfigPath)
$StatePath = [System.IO.Path]::GetFullPath($StatePath)
$WorktreeRoot = [System.IO.Path]::GetFullPath($WorktreeRoot)
$BinaryPath = [System.IO.Path]::GetFullPath($BinaryPath)

foreach ($directory in @(
    (Split-Path -Parent $ConfigPath),
    $StatePath,
    $WorktreeRoot,
    (Split-Path -Parent $BinaryPath)
)) {
    New-Item -ItemType Directory -Force -Path $directory | Out-Null
}

if (!(Test-Path -LiteralPath $BinaryPath -PathType Leaf) -or $Rebuild) {
    Push-Location $checkoutRoot
    try {
        & $goPath build -o $BinaryPath .
        if ($LASTEXITCODE -ne 0) { throw 'controller build failed' }
    } finally {
        Pop-Location
    }
}

$tailscaleStatus = (& $TailscalePath status --json | ConvertFrom-Json)
if (!$tailscaleStatus.Self.Online) {
    throw 'Tailscale is not online for this Windows user'
}
$tailnetHost = $tailscaleStatus.Self.DNSName.TrimEnd('.')
$publicUrl = "https://$tailnetHost`:$TailscaleHttpsPort"
$proxyTarget = "http://127.0.0.1:$LocalPort"
$serveStatus = (& $TailscalePath serve status --json | ConvertFrom-Json)
$serveKey = "$tailnetHost`:$TailscaleHttpsPort"
$serveEntry = $serveStatus.Web.PSObject.Properties[$serveKey]
if ($serveEntry) {
    $rootHandler = $serveEntry.Value.Handlers.PSObject.Properties['/']
    $existingTarget = if ($rootHandler) { $rootHandler.Value.Proxy } else { $null }
    if ($existingTarget -ne $proxyTarget) {
        throw "Tailscale HTTPS port $TailscaleHttpsPort is already assigned to another service"
    }
}

if (!(Test-Path -LiteralPath $ConfigPath)) {
    $config = @(
        'host: local',
        'backend: t3',
        "user: $(Quote-Yaml $Profile)",
        'provider: codex',
        "repo: $(Quote-Yaml $Repository)",
        "base: $(Quote-Yaml $Base)",
        "t3_public_url: $(Quote-Yaml $publicUrl)",
        "local_repo_root: $(Quote-Yaml $LocalRepoRoot)",
        "local_worktree_root: $(Quote-Yaml $WorktreeRoot)",
        "local_t3_bin: $(Quote-Yaml $T3Path)",
        "local_codex_bin: $(Quote-Yaml $CodexPath)",
        "local_t3_port: $LocalPort"
    ) -join [Environment]::NewLine
    $temporaryConfig = "$ConfigPath.new"
    [System.IO.File]::WriteAllText($temporaryConfig, $config + [Environment]::NewLine)
    Move-Item -LiteralPath $temporaryConfig -Destination $ConfigPath
}

$statusText = (& $BinaryPath --config $ConfigPath --state $StatePath status $TaskId 2>&1) -join [Environment]::NewLine
$statusExit = $LASTEXITCODE
if ($statusExit -ne 0) {
    $statusError = $null
    try { $statusError = ($statusText | ConvertFrom-Json).error } catch {}
    if ($statusError -ne 'unknown task') {
        throw "could not inspect existing task: $statusText"
    }
    Invoke-Controller create --task $TaskId
    Invoke-Controller prepare $TaskId
} else {
    $status = $statusText | ConvertFrom-Json
    if ($status.task.host -ne 'local' -or $status.task.t3_port -ne $LocalPort) {
        throw 'existing task does not match the requested local host and port'
    }
    if ([System.IO.Path]::GetFullPath($status.task.source_root) -ne $LocalRepoRoot) {
        throw 'existing task belongs to a different source repository'
    }
}

& (Join-Path $PSScriptRoot 'install-local-task.ps1') `
    -BinaryPath $BinaryPath `
    -ConfigPath $ConfigPath `
    -StatePath $StatePath `
    -TaskId $TaskId `
    -LocalPort $LocalPort `
    -TailscaleHttpsPort $TailscaleHttpsPort `
    -TailscalePath $TailscalePath

if ($LASTEXITCODE -ne 0) { throw 'local task service installation failed' }

$finalStatus = & $BinaryPath --config $ConfigPath --state $StatePath status $TaskId
if ($LASTEXITCODE -ne 0) { throw 'local task did not pass its final status check' }

$skillPath = $null
if ($SkillRoot) {
    $SkillRoot = [System.IO.Path]::GetFullPath($SkillRoot)
    New-Item -ItemType Directory -Force -Path $SkillRoot | Out-Null
    $skillPath = Join-Path $SkillRoot 'codex-remote'
    $skillSource = Join-Path $checkoutRoot 'skills\codex-remote'
    if (Test-Path -LiteralPath $skillPath) {
        $existingSkill = Get-Item -LiteralPath $skillPath -Force
        $target = @($existingSkill.Target) | Select-Object -First 1
        if (!$target -or [System.IO.Path]::GetFullPath($target) -ne [System.IO.Path]::GetFullPath($skillSource)) {
            throw "an unrelated skill installation already exists: $skillPath"
        }
    } else {
        New-Item -ItemType Junction -Path $skillPath -Target $skillSource | Out-Null
    }
}

[pscustomobject]@{
    task = $TaskId
    config = $ConfigPath
    state = $StatePath
    binary = $BinaryPath
    skill = $skillPath
    tailnet = $publicUrl
    pairing = 'Run pair with a private output file; never paste its contents into chat.'
    status = ($finalStatus -join [Environment]::NewLine | ConvertFrom-Json)
}
