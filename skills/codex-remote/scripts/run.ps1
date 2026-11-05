param(
    [Parameter(ValueFromRemainingArguments = $true)][string[]]$ControllerArguments
)

$ErrorActionPreference = 'Stop'
$root = [System.IO.Path]::GetFullPath((Join-Path $PSScriptRoot '..\..\..'))
$candidates = @(
    $env:CODEX_REMOTE_BIN,
    (Join-Path $env:USERPROFILE '.local\bin\codex-remote.exe'),
    (Join-Path $root 'bin\codex-remote.exe')
) | Where-Object { $_ }
$binary = $candidates | Where-Object { Test-Path -LiteralPath $_ -PathType Leaf } | Select-Object -First 1
if (!$binary) {
    throw 'Build or install the trusted controller before using this skill.'
}

& $binary @ControllerArguments
exit $LASTEXITCODE
