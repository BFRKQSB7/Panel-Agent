#requires -Version 7.0
param(
    [Parameter(Mandatory)][string]$SshDestination,
    [int]$SshPort=45000,
    [Parameter(Mandatory)][string]$SshIdentityFile,
    [Parameter(Mandatory)][string]$SshKnownHostsFile,
    [Parameter(Mandatory)][string]$ExpectedHostname,
    [Parameter(Mandatory)][string]$ExpectedIPv4
)
& (Join-Path $PSScriptRoot 'run-d04-smoke.ps1') @PSBoundParameters -LedgerOnly
