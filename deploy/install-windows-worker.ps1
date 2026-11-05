[CmdletBinding()]
param(
    [Parameter(Mandatory = $true)][string]$PublicKeyPath,
    [Parameter(Mandatory = $true)][string]$RepoRoot,
    [Parameter(Mandatory = $true)][string]$WorktreeRoot,
    [Parameter(Mandatory = $true)][string]$StateRoot,
    [Parameter(Mandatory = $true)][string]$T3Path,
    [Parameter(Mandatory = $true)][string]$CodexPath,
    [Parameter(Mandatory = $true)][ValidateRange(1024, 65535)][int]$T3Port,
    [string]$InstallPath = "$HOME\.local\bin\codex-remote.exe",
    [string]$PolicyPath = "$HOME\.codex\state\codex-remote-runner\windows-worker.json",
    [string]$GoPath,
    [string]$GitPath,
    [string]$OpenSSHMSIPath,
    [string]$TailscaleIPv4Range = '100.64.0.0/10'
)

$ErrorActionPreference = 'Stop'
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

$policy = [ordered]@{
    repo_root = $RepoRoot.Replace('\', '/')
    worktree_root = $WorktreeRoot.Replace('\', '/')
    state_root = $StateRoot.Replace('\', '/')
    t3_bin = $T3Path.Replace('\', '/')
    codex_bin = $CodexPath.Replace('\', '/')
    t3_port = $T3Port
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
    & icacls.exe $directory '/inheritance:r' '/grant:r' '*S-1-5-18:(OI)(CI)F' '/grant' '*S-1-5-32-544:(OI)(CI)F' '/grant' '*S-1-5-11:(OI)(CI)RX' | Out-Null
    if ($LASTEXITCODE -ne 0) { throw "could not secure OpenSSH directory: $directory" }
}
if (!(Test-Path -LiteralPath $sshdConfig)) {
    $defaultConfig = Join-Path $openSshRoot 'sshd_config_default'
    if (!(Test-Path -LiteralPath $defaultConfig)) { throw 'OpenSSH default configuration is missing' }
    Copy-Item -LiteralPath $defaultConfig -Destination $sshdConfig
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
