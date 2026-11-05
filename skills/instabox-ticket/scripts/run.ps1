param(
    [Parameter(ValueFromRemainingArguments = $true)][string[]]$ControllerArguments
)

$ErrorActionPreference = 'Stop'
$root = [System.IO.Path]::GetFullPath((Join-Path $PSScriptRoot '..\..\..'))
$candidates = @(
    $env:IB_TICKET_BIN,
    (Join-Path $env:USERPROFILE '.local\bin\ib-ticket.exe'),
    (Join-Path $root 'bin\ib-ticket.exe')
) | Where-Object { $_ }
$binary = $candidates | Where-Object { Test-Path -LiteralPath $_ -PathType Leaf } | Select-Object -First 1
if (!$binary) {
    throw 'Build or install the trusted controller before using this skill.'
}

& $binary @ControllerArguments
exit $LASTEXITCODE
