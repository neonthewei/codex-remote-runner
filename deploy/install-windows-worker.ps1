[CmdletBinding()]
param(
    [Parameter(Mandatory = $true)][string]$PublicKeyPath,
    [Parameter(Mandatory = $true)][string]$RepoRoot,
    [Parameter(Mandatory = $true)][string]$WorktreeRoot,
    [Parameter(Mandatory = $true)][string]$StateRoot,
    [Parameter(Mandatory = $true)][string]$T3Path,
    [Parameter(Mandatory = $true)][string]$CodexPath,
    [ValidateRange(0, 65535)][int]$T3Port = 0,
    [ValidateRange(0, 65535)][int]$T3PortStart = 0,
    [ValidateRange(0, 65535)][int]$T3PortEnd = 0,
    [string]$InstallPath = "$HOME\.local\bin\codex-remote.exe",
    [string]$PolicyPath = "$HOME\.codex\state\codex-remote-runner\windows-worker.json",
    [string]$GoPath,
    [string]$GitPath,
    [string]$SetupExecutablePath,
    [string[]]$SetupArguments = @(),
    [string[]]$SetupPathEntries = @(),
    [string]$WorkspaceExecutablePath,
    [string[]]$WorkspaceArguments = @(),
    [string[]]$WorkspacePathEntries = @(),
    [string]$BrowserExecutablePath,
    [string]$OpenSSHMSIPath,
    [string]$TailscaleIPv4Range = '100.64.0.0/10'
)

$ErrorActionPreference = 'Stop'
$usingLegacyPort = $T3PortStart -eq 0 -and $T3PortEnd -eq 0
if ($usingLegacyPort) {
    if ($T3Port -lt 1024 -or $T3Port -gt 65535) { throw 'Provide -T3Port or a valid -T3PortStart/-T3PortEnd range.' }
    $T3PortStart = $T3Port
    $T3PortEnd = $T3Port
} elseif ($T3PortStart -lt 1024 -or $T3PortEnd -gt 65535 -or $T3PortStart -gt $T3PortEnd) {
    throw 'T3 port range must be between 1024 and 65535.'
}
$principal = [Security.Principal.WindowsPrincipal]::new([Security.Principal.WindowsIdentity]::GetCurrent())
if (!$principal.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)) {
    throw 'Run this installer from an elevated PowerShell session.'
}

$publicKey = (Get-Content -LiteralPath $PublicKeyPath -Raw).Trim()
if ($publicKey -notmatch '^ssh-ed25519 [A-Za-z0-9+/=]+(?: .*)?$') {
    throw 'Provide one dedicated ssh-ed25519 public key.'
}
$keyParts = $publicKey -split '\s+'
$keyIdentity = "$($keyParts[0]) $($keyParts[1])"

foreach ($path in @($RepoRoot, $T3Path, $CodexPath)) {
    if (!(Test-Path -LiteralPath $path)) { throw "required Windows worker path is missing: $path" }
}
$RepoRoot = (Resolve-Path -LiteralPath $RepoRoot).Path
$T3Path = (Resolve-Path -LiteralPath $T3Path).Path
$CodexPath = (Resolve-Path -LiteralPath $CodexPath).Path
$WorktreeRoot = [System.IO.Path]::GetFullPath($WorktreeRoot)
$StateRoot = [System.IO.Path]::GetFullPath($StateRoot)
$PolicyPath = [System.IO.Path]::GetFullPath($PolicyPath)

if ($GitPath) {
    if (!(Test-Path -LiteralPath $GitPath -PathType Leaf)) { throw "Git executable is missing: $GitPath" }
    $GitPath = (Resolve-Path -LiteralPath $GitPath).Path
} else {
    $git = Get-Command git.exe -ErrorAction SilentlyContinue | Select-Object -First 1
    if (!$git) { throw 'Git for Windows is required by the worker.' }
    $GitPath = $git.Source
}

if (!$SetupExecutablePath -and ($SetupArguments.Count -gt 0 -or $SetupPathEntries.Count -gt 0)) {
    throw 'Setup arguments and PATH entries require SetupExecutablePath.'
}
$resolvedSetupPaths = @()
if ($SetupExecutablePath) {
    if (!(Test-Path -LiteralPath $SetupExecutablePath -PathType Leaf)) { throw "Setup executable is missing: $SetupExecutablePath" }
    $SetupExecutablePath = (Resolve-Path -LiteralPath $SetupExecutablePath).Path
    foreach ($pathEntry in $SetupPathEntries) {
        if (!(Test-Path -LiteralPath $pathEntry -PathType Container)) { throw "Setup PATH entry is missing: $pathEntry" }
        $resolvedSetupPaths += (Resolve-Path -LiteralPath $pathEntry).Path.Replace('\', '/')
    }
}

if (!$WorkspaceExecutablePath -and ($WorkspaceArguments.Count -gt 0 -or $WorkspacePathEntries.Count -gt 0)) {
    throw 'Workspace arguments and PATH entries require WorkspaceExecutablePath.'
}
$resolvedWorkspacePaths = @()
if ($WorkspaceExecutablePath) {
    if (!(Test-Path -LiteralPath $WorkspaceExecutablePath -PathType Leaf)) { throw "Workspace executable is missing: $WorkspaceExecutablePath" }
    $WorkspaceExecutablePath = (Resolve-Path -LiteralPath $WorkspaceExecutablePath).Path
    foreach ($pathEntry in $WorkspacePathEntries) {
        if (!(Test-Path -LiteralPath $pathEntry -PathType Container)) { throw "Workspace PATH entry is missing: $pathEntry" }
        $resolvedWorkspacePaths += (Resolve-Path -LiteralPath $pathEntry).Path.Replace('\', '/')
    }
}
if ($BrowserExecutablePath) {
    if (!(Test-Path -LiteralPath $BrowserExecutablePath -PathType Leaf)) { throw "Browser executable is missing: $BrowserExecutablePath" }
    $BrowserExecutablePath = (Resolve-Path -LiteralPath $BrowserExecutablePath).Path
}

$capability = Get-WindowsCapability -Online -Name 'OpenSSH.Server~~~~0.0.1.0'
if ($capability.State -ne 'Installed') {
    Add-WindowsCapability -Online -Name 'OpenSSH.Server~~~~0.0.1.0' | Out-Null
}
if ($OpenSSHMSIPath) {
    $OpenSSHMSIPath = (Resolve-Path -LiteralPath $OpenSSHMSIPath).Path
    $signature = Get-AuthenticodeSignature -FilePath $OpenSSHMSIPath
    if ($signature.Status -ne 'Valid' -or $signature.SignerCertificate.Subject -notmatch '(^|, )O=Microsoft Corporation(,|$)') {
        throw 'OpenSSH MSI must have a valid Microsoft Corporation signature'
    }
    $msi = Start-Process -FilePath 'msiexec.exe' -ArgumentList @('/i', $OpenSSHMSIPath, 'ADDLOCAL=Server', '/qn', '/norestart') -WindowStyle Hidden -Wait -PassThru
    if ($msi.ExitCode -ne 0 -and $msi.ExitCode -ne 3010) { throw "OpenSSH MSI failed with exit code $($msi.ExitCode)" }
}

$checkoutRoot = Split-Path -Parent $PSScriptRoot
if ($GoPath) {
    if (!(Test-Path -LiteralPath $GoPath -PathType Leaf)) { throw "Go executable is missing: $GoPath" }
} else {
    $go = Get-Command go.exe -ErrorAction SilentlyContinue | Select-Object -First 1
    if ($go) {
        $GoPath = $go.Source
    } else {
        $GoPath = Get-ChildItem -Path (Join-Path $HOME '.cache\go*\go\bin\go.exe') -File -ErrorAction SilentlyContinue | Sort-Object LastWriteTimeUtc -Descending | Select-Object -First 1 -ExpandProperty FullName
    }
}
if (!$GoPath) { throw 'Go 1.26.10 or newer is required to build the worker.' }
$goVersionText = (& $GoPath version) -join ' '
if ($goVersionText -notmatch 'go(\d+\.\d+(?:\.\d+)?)' -or [version]$Matches[1] -lt [version]'1.26.10') {
    throw "Go 1.26.10 or newer is required; found $goVersionText"
}
New-Item -ItemType Directory -Force -Path (Split-Path -Parent $InstallPath) | Out-Null
New-Item -ItemType Directory -Force -Path $WorktreeRoot | Out-Null
New-Item -ItemType Directory -Force -Path $StateRoot | Out-Null
New-Item -ItemType Directory -Force -Path (Split-Path -Parent $PolicyPath) | Out-Null
Push-Location $checkoutRoot
try {
    & $GoPath build -o $InstallPath .
    if ($LASTEXITCODE -ne 0) { throw 'codex-remote worker build failed' }
} finally {
    Pop-Location
}

$setupExecutablePolicyPath = if ($SetupExecutablePath) { $SetupExecutablePath.Replace('\', '/') } else { '' }
$workspaceExecutablePolicyPath = if ($WorkspaceExecutablePath) { $WorkspaceExecutablePath.Replace('\', '/') } else { '' }
$browserExecutablePolicyPath = if ($BrowserExecutablePath) { $BrowserExecutablePath.Replace('\', '/') } else { '' }
$policy = [ordered]@{
    repo_root = $RepoRoot.Replace('\', '/')
    worktree_root = $WorktreeRoot.Replace('\', '/')
    state_root = $StateRoot.Replace('\', '/')
    t3_bin = $T3Path.Replace('\', '/')
    codex_bin = $CodexPath.Replace('\', '/')
    setup_bin = $setupExecutablePolicyPath
    setup_args = @($SetupArguments)
    setup_paths = @($resolvedSetupPaths)
    workspace_bin = $workspaceExecutablePolicyPath
    workspace_args = @($WorkspaceArguments)
    workspace_paths = @($resolvedWorkspacePaths)
    browser_bin = $browserExecutablePolicyPath
    t3_port = if ($usingLegacyPort) { $T3Port } else { 0 }
    t3_port_start = $T3PortStart
    t3_port_end = $T3PortEnd
} | ConvertTo-Json
[System.IO.File]::WriteAllText($PolicyPath, $policy + [Environment]::NewLine)
$identity = [System.Security.Principal.WindowsIdentity]::GetCurrent().Name
& icacls.exe $PolicyPath '/inheritance:r' '/grant:r' "$identity`:F" '/grant' 'SYSTEM:F' | Out-Null
if ($LASTEXITCODE -ne 0) { throw 'could not secure the Windows worker policy' }

$isAdministrator = $principal.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)
if ($isAdministrator) {
    $authorizedKeys = Join-Path $env:ProgramData 'ssh\administrators_authorized_keys'
} else {
    $authorizedKeys = Join-Path $HOME '.ssh\authorized_keys'
}
New-Item -ItemType Directory -Force -Path (Split-Path -Parent $authorizedKeys) | Out-Null
$existingKeys = if (Test-Path -LiteralPath $authorizedKeys) { Get-Content -LiteralPath $authorizedKeys } else { @() }
$gitDirectory = (Split-Path -Parent $GitPath).Replace("'", "''")
$workerScript = "`$env:Path = '$gitDirectory;' + `$env:Path; & '" + $InstallPath.Replace("'", "''") + "' worker --policy '" + $PolicyPath.Replace("'", "''") + "'"
$encodedWorker = [Convert]::ToBase64String([Text.Encoding]::Unicode.GetBytes($workerScript))
$forcedKey = "command=`"powershell.exe -NoProfile -NonInteractive -EncodedCommand $encodedWorker`",no-agent-forwarding,no-port-forwarding,no-pty,no-X11-forwarding $publicKey"
$escapedIdentity = [regex]::Escape($keyIdentity)
$existingKeys = @($existingKeys | Where-Object { $_ -notmatch "(?:^|\s)$escapedIdentity(?:\s|$)" })
[System.IO.File]::WriteAllLines($authorizedKeys, @($existingKeys + $forcedKey), [System.Text.Encoding]::ASCII)

if ($isAdministrator) {
    & icacls.exe $authorizedKeys '/inheritance:r' '/grant:r' '*S-1-5-32-544:F' '/grant' 'SYSTEM:F' | Out-Null
} else {
    $identity = [System.Security.Principal.WindowsIdentity]::GetCurrent().Name
    & icacls.exe $authorizedKeys '/inheritance:r' '/grant:r' "$identity`:F" '/grant' 'SYSTEM:F' | Out-Null
}
if ($LASTEXITCODE -ne 0) { throw 'could not secure authorized_keys permissions' }

$openSshRoot = @(
    (Join-Path $env:ProgramFiles 'OpenSSH'),
    (Join-Path $env:ProgramFiles 'OpenSSH-Win64'),
    (Join-Path $env:WINDIR 'System32\OpenSSH')
) | Where-Object { Test-Path -LiteralPath (Join-Path $_ 'sshd.exe') } | Select-Object -First 1
if (!$openSshRoot) { throw 'OpenSSH Server executable is missing' }
$sshd = Join-Path $openSshRoot 'sshd.exe'
$sshKeygen = Join-Path $openSshRoot 'ssh-keygen.exe'
$sshdDirectory = Join-Path $env:ProgramData 'ssh'
$sshdLogs = Join-Path $sshdDirectory 'logs'
$sshdConfig = Join-Path $sshdDirectory 'sshd_config'
New-Item -ItemType Directory -Force -Path $sshdDirectory | Out-Null
New-Item -ItemType Directory -Force -Path $sshdLogs | Out-Null
foreach ($directory in @($sshdDirectory, $sshdLogs)) {
    & icacls.exe $directory '/inheritance:r' '/grant:r' '*S-1-5-18:(OI)(CI)F' '/grant' '*S-1-5-32-544:(OI)(CI)F' | Out-Null
    if ($LASTEXITCODE -ne 0) { throw "could not secure OpenSSH directory: $directory" }
}
if (!(Test-Path -LiteralPath $sshdConfig)) {
    $defaultConfig = Join-Path $openSshRoot 'sshd_config_default'
    if (!(Test-Path -LiteralPath $defaultConfig)) { throw 'OpenSSH default configuration is missing' }
    Copy-Item -LiteralPath $defaultConfig -Destination $sshdConfig
}
Get-ChildItem -LiteralPath $sshdDirectory -Filter 'ssh_host_*_key' -File -ErrorAction SilentlyContinue | ForEach-Object {
    & icacls.exe $_.FullName '/inheritance:r' '/grant:r' '*S-1-5-18:F' '/grant' '*S-1-5-32-544:F' | Out-Null
    if ($LASTEXITCODE -ne 0) { throw "could not secure OpenSSH host key: $($_.FullName)" }
}
& $sshKeygen -A
if ($LASTEXITCODE -ne 0) { throw 'OpenSSH host-key initialization failed' }
$systemSid = [System.Security.Principal.SecurityIdentifier]::new('S-1-5-18')
$administratorsSid = [System.Security.Principal.SecurityIdentifier]::new('S-1-5-32-544')
foreach ($hostKey in Get-ChildItem -LiteralPath $sshdDirectory -Filter 'ssh_host_*_key' -File) {
    $hostKeyAcl = [System.Security.AccessControl.FileSecurity]::new()
    $hostKeyAcl.SetAccessRuleProtection($true, $false)
    $hostKeyAcl.SetOwner($systemSid)
    $hostKeyAcl.AddAccessRule([System.Security.AccessControl.FileSystemAccessRule]::new($systemSid, [System.Security.AccessControl.FileSystemRights]::FullControl, [System.Security.AccessControl.AccessControlType]::Allow))
    $hostKeyAcl.AddAccessRule([System.Security.AccessControl.FileSystemAccessRule]::new($administratorsSid, [System.Security.AccessControl.FileSystemRights]::FullControl, [System.Security.AccessControl.AccessControlType]::Allow))
    Set-Acl -LiteralPath $hostKey.FullName -AclObject $hostKeyAcl
}
$backup = "$sshdConfig.codex-remote.bak"
if (!(Test-Path -LiteralPath $backup)) {
    Copy-Item -LiteralPath $sshdConfig -Destination $backup
}
$config = Get-Content -LiteralPath $sshdConfig
$config = $config | Where-Object { $_ -notmatch '^\s*#?\s*(PasswordAuthentication|PubkeyAuthentication|KbdInteractiveAuthentication|AuthenticationMethods)\s+' }
$matchIndex = -1
for ($index = 0; $index -lt $config.Count; $index++) {
    if ($config[$index] -match '^\s*Match\s+') {
        $matchIndex = $index
        break
    }
}
$securityConfig = @('PubkeyAuthentication yes', 'PasswordAuthentication no', 'KbdInteractiveAuthentication no', 'AuthenticationMethods publickey')
if ($matchIndex -ge 0) {
    $beforeMatch = if ($matchIndex -eq 0) { @() } else { @($config[0..($matchIndex - 1)]) }
    $config = $beforeMatch + $securityConfig + @($config[$matchIndex..($config.Count - 1)])
} else {
    $config += $securityConfig
}
[System.IO.File]::WriteAllLines($sshdConfig, $config)

& $sshd -t -f $sshdConfig
if ($LASTEXITCODE -ne 0) {
    Copy-Item -LiteralPath $backup -Destination $sshdConfig -Force
    throw 'OpenSSH configuration validation failed; the original file was restored'
}

Set-Service -Name sshd -StartupType Automatic
Start-Service -Name sshd
Restart-Service -Name sshd

$firewall = Get-NetFirewallRule -Name 'OpenSSH-Server-In-TCP' -ErrorAction SilentlyContinue
if (!$firewall) {
    $firewall = New-NetFirewallRule -Name 'OpenSSH-Server-In-TCP' -DisplayName 'OpenSSH Server (Tailscale only)' -Enabled True -Direction Inbound -Protocol TCP -Action Allow -LocalPort 22
}
$firewall | Get-NetFirewallAddressFilter | Set-NetFirewallAddressFilter -RemoteAddress $TailscaleIPv4Range
$hostFingerprint = & $sshKeygen -lf (Join-Path $env:ProgramData 'ssh\ssh_host_ed25519_key.pub')

[pscustomobject]@{
    worker = (Resolve-Path -LiteralPath $InstallPath).Path
    policy = $PolicyPath
    sshd = (Get-Service -Name sshd).Status.ToString()
    authorized_keys = $authorizedKeys
    firewall_remote = $TailscaleIPv4Range
    password_authentication = $false
    host_key_fingerprint = ($hostFingerprint -join ' ')
}
