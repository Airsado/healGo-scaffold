#!/usr/bin/env pwsh
# Test connection values are process-local; preserve the caller's environment exactly.
[CmdletBinding()]
param(
    [ValidateSet('All', 'Postgres', 'MySQL')]
    [string]$Database = 'All',
    [switch]$IncludeReplication,
    [switch]$Race
)

$ErrorActionPreference = 'Stop'
$PSNativeCommandUseErrorActionPreference = $false
$taskRoot = [IO.Path]::GetFullPath((Join-Path $PSScriptRoot '../..'))
$environmentFile = Join-Path $taskRoot '.local/replication-test/test.env'
if (-not (Test-Path -LiteralPath $environmentFile)) {
    throw 'Test environment is absent. Run scripts/replication/replication.ps1 -Action Start first.'
}

$previous = @{}
$testEnvironment = @{}
foreach ($line in Get-Content -LiteralPath $environmentFile) {
    if ([string]::IsNullOrWhiteSpace($line)) { continue }
    $pair = $line.Split('=', 2)
    if ($pair.Count -ne 2 -or $pair[0] -notmatch '^HEAGO_TEST_[A-Z0-9_]+$') { throw 'Invalid test environment file entry.' }
    $testEnvironment[$pair[0]] = $pair[1]
}
foreach ($name in @($testEnvironment.Keys) + @('HEAGO_TEST_REPLICATION')) {
    $previous[$name] = [Environment]::GetEnvironmentVariable($name, 'Process')
}

function Invoke-GoTests([string]$Label, [string[]]$Arguments) {
    Write-Host $Label
    # Do not print test.env or verbose connection setup. Application database logs redact SQL/errors.
    & go @Arguments
    if ($LASTEXITCODE -ne 0) { throw "$Label failed." }
}

Push-Location -LiteralPath $taskRoot
try {
    foreach ($name in $testEnvironment.Keys) {
        [Environment]::SetEnvironmentVariable($name, $testEnvironment[$name], 'Process')
    }
    [Environment]::SetEnvironmentVariable('HEAGO_TEST_REPLICATION', [NullString]::Value, 'Process')
    $testArguments = @('test', '-count=1')
    if ($Race) { $testArguments += '-race' }
    $testArguments += @('./...', '-timeout', '240s')

    if ($Database -ne 'MySQL') {
        # Other driver's suite skips naturally; this proves each driver's coverage independently.
        [Environment]::SetEnvironmentVariable('HEAGO_TEST_DATABASE_URL', $testEnvironment.HEAGO_TEST_DATABASE_URL, 'Process')
        [Environment]::SetEnvironmentVariable('HEAGO_TEST_MYSQL_DSN', [NullString]::Value, 'Process')
        Invoke-GoTests 'Running full suite against PostgreSQL 16...' $testArguments
    }
    if ($Database -ne 'Postgres') {
        [Environment]::SetEnvironmentVariable('HEAGO_TEST_DATABASE_URL', [NullString]::Value, 'Process')
        [Environment]::SetEnvironmentVariable('HEAGO_TEST_MYSQL_DSN', $testEnvironment.HEAGO_TEST_MYSQL_DSN, 'Process')
        Invoke-GoTests 'Running full suite against MySQL 8.4...' $testArguments
    }
    if ($IncludeReplication) {
        # This gate explicitly authorizes the test to pause replay and stop only harness replicas.
        # Full-suite tests above never alter node availability while they run concurrently.
        [Environment]::SetEnvironmentVariable('HEAGO_TEST_REPLICATION', '1', 'Process')
        $replicationArguments = @('test', '-count=1')
        if ($Race) { $replicationArguments += '-race' }
        # Go matches slash-separated -run expressions against each subtest level
        # Anchor the engine too, so selecting MySQL never reaches the PostgreSQL fault injection
        # ValidateSet is case-insensitive, while Go subtest regexes are case-sensitive
        $enginePattern = switch ($Database) {
            'Postgres' { 'Postgres' }
            'MySQL' { 'MySQL' }
            default { '(Postgres|MySQL)' }
        }
        $replicationPattern = '^TestReplicationIntegration$/^' + $enginePattern + '$'
        $replicationArguments += @('./internal/db', '-run', $replicationPattern, '-timeout', '240s')
        Invoke-GoTests 'Running real replication delay and replica-failure integration...' $replicationArguments
    }
    Write-Host 'Selected database verification completed.'
} finally {
    foreach ($name in $previous.Keys) {
        # PowerShell can coerce $null to an empty string when binding a string argument
        # NullString preserves absence rather than creating an empty environment entry
        if ($null -eq $previous[$name]) {
            [Environment]::SetEnvironmentVariable($name, [NullString]::Value, 'Process')
        } else {
            [Environment]::SetEnvironmentVariable($name, $previous[$name], 'Process')
        }
    }
    Pop-Location
}
