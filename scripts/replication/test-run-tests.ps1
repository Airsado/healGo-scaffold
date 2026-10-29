#!/usr/bin/env pwsh
#requires -Version 7.0
# Exercise the real runner with fake connections and a mocked Go command
# No Docker, database, Pester dependency or existing private configuration is used
[CmdletBinding()]
param()

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'
$PSNativeCommandUseErrorActionPreference = $false
$tempParent = [IO.Path]::GetFullPath([IO.Path]::GetTempPath()).TrimEnd([char[]]@('/', '\'))
$sandbox = Join-Path $tempParent ('heago-run-tests-' + [Guid]::NewGuid().ToString('N'))
$runner = Join-Path $sandbox 'scripts/replication/run-tests.ps1'
$fakeEnvironment = @{
    HEAGO_TEST_DATABASE_URL = 'fake-postgres'
    HEAGO_TEST_MYSQL_DSN = 'fake-mysql'
    HEAGO_TEST_POSTGRES_PRIMARY_DSN = 'fake-postgres-primary'
    HEAGO_TEST_POSTGRES_REPLICA_DSN = 'fake-postgres-replica'
    HEAGO_TEST_MYSQL_PRIMARY_DSN = 'fake-mysql-primary'
    HEAGO_TEST_MYSQL_REPLICA_DSN = 'fake-mysql-replica'
    HEAGO_TEST_POSTGRES_ADMIN_DSN = 'fake-postgres-admin'
    HEAGO_TEST_MYSQL_ADMIN_DSN = 'fake-mysql-admin'
}
$environmentNames = @($fakeEnvironment.Keys) + @('HEAGO_TEST_REPLICATION', 'HEAGO_RUNNER_TEST_SENTINEL')
$originalEnvironment = @{}
foreach ($name in $environmentNames) {
    $originalEnvironment[$name] = [Environment]::GetEnvironmentVariable($name, 'Process')
}
$originalLastExit = Get-Variable LASTEXITCODE -Scope Global -ErrorAction SilentlyContinue
$originalLastExitValue = if ($null -ne $originalLastExit) { $originalLastExit.Value } else { $null }
$mockState = [pscustomobject]@{
    Calls = [Collections.Generic.List[object]]::new()
    FailureAt = 0
}
$script:checkedCases = 0

function Assert-Condition([bool]$Condition, [string]$Message) {
    if (-not $Condition) { throw $Message }
}

# A function shadows the executable only inside this script's scope
# Returning a failing native exit code exercises the runner's own error handling
$mockGo = {
    $mockState.Calls.Add([pscustomobject]@{
        Arguments = @($args)
        Postgres = [Environment]::GetEnvironmentVariable('HEAGO_TEST_DATABASE_URL', 'Process')
        MySQL = [Environment]::GetEnvironmentVariable('HEAGO_TEST_MYSQL_DSN', 'Process')
        Replication = [Environment]::GetEnvironmentVariable('HEAGO_TEST_REPLICATION', 'Process')
        Directory = (Get-Location).Path
    })
    $global:LASTEXITCODE = if ($mockState.Calls.Count -eq $mockState.FailureAt) { 23 } else { 0 }
}.GetNewClosure()
Set-Item -LiteralPath Function:go -Value $mockGo

function Test-RunnerCase([string]$Database, [bool]$Replication, [bool]$Race, [int]$FailureAt = 0) {
    Write-Verbose "Runner case: Database=$Database Replication=$Replication Race=$Race FailureAt=$FailureAt"
    $mockState.Calls.Clear()
    $mockState.FailureAt = $FailureAt
    $expectedEnvironment = @{}
    $index = 0
    foreach ($name in $environmentNames) {
        # Test restoration of both existing values and previously absent variables
        $value = if ($index++ % 2 -eq 0) { 'caller-sentinel-' + $name } else { $null }
        $expectedEnvironment[$name] = $value
        if ($null -eq $value) { Remove-Item -LiteralPath "Env:$name" -ErrorAction SilentlyContinue }
        else { [Environment]::SetEnvironmentVariable($name, $value, 'Process') }
    }
    $beforeDirectory = (Get-Location).Path
    $messages = [Collections.Generic.List[string]]::new()
    $failure = $null
    try {
        & $runner -Database $Database -IncludeReplication:$Replication -Race:$Race 6>&1 |
            ForEach-Object { $messages.Add("$_") }
    } catch {
        $failure = $_
    }

    foreach ($name in $environmentNames) {
        Assert-Condition ([Environment]::GetEnvironmentVariable($name, 'Process') -ceq $expectedEnvironment[$name]) "Environment was not restored: $name"
    }
    Assert-Condition ((Get-Location).Path -eq $beforeDirectory) 'Working directory was not restored'
    $engines = @(switch ($Database) {
        'Postgres' { 'Postgres' }
        'MySQL' { 'MySQL' }
        default { 'Postgres'; 'MySQL' }
    })
    $expectedCount = $engines.Count + [int]$Replication
    if ($FailureAt -gt 0) {
        Assert-Condition ($null -ne $failure -and $failure.Exception.Message -like '* failed.') 'A failed Go command did not propagate as an error'
        Assert-Condition ($mockState.Calls.Count -eq $FailureAt) 'The runner continued after a failed Go command'
        Assert-Condition (-not ($messages -contains 'Selected database verification completed.')) 'The runner reported success after failure'
    } else {
        if ($null -ne $failure) { throw $failure }
        Assert-Condition ($mockState.Calls.Count -eq $expectedCount) 'Unexpected number of Go commands'
        Assert-Condition ($messages -contains 'Selected database verification completed.') 'The runner did not report completion'
    }

    for ($callIndex = 0; $callIndex -lt $mockState.Calls.Count; $callIndex++) {
        $call = $mockState.Calls[$callIndex]
        Assert-Condition ($call.Directory -eq $sandbox) 'Go was not invoked from the copied project root'
        Assert-Condition (($call.Arguments -contains '-race') -eq $Race) 'The race option was not propagated correctly'
        Assert-Condition ($call.Arguments[0] -eq 'test') 'Unexpected Go command'
        if ($callIndex -lt $engines.Count) {
            Assert-Condition ($call.Arguments -contains './...') 'The full suite was omitted'
            Assert-Condition ([string]::IsNullOrEmpty($call.Replication)) 'Fault injection was enabled during the full suite'
            $isPostgres = $engines[$callIndex] -eq 'Postgres'
            Assert-Condition (($call.Postgres -eq 'fake-postgres') -eq $isPostgres) 'The full suite selected the wrong PostgreSQL connection'
            Assert-Condition (($call.MySQL -eq 'fake-mysql') -eq (-not $isPostgres)) 'The full suite selected the wrong MySQL connection'
        } else {
            Assert-Condition ($call.Replication -eq '1') 'Replication was not explicitly enabled'
            Assert-Condition ($call.Arguments -contains './internal/db') 'Replication targeted the wrong package'
            $runIndex = [Array]::IndexOf($call.Arguments, '-run')
            Assert-Condition ($runIndex -ge 0) 'Replication was not filtered'
            $parts = $call.Arguments[$runIndex + 1].Split('/')
            Assert-Condition ($parts.Count -eq 2) 'The filter does not restrict a subtest level'
            Assert-Condition ([regex]::IsMatch('TestReplicationIntegration', $parts[0])) 'The replication parent test was excluded'
            Assert-Condition (-not [regex]::IsMatch('TestReplicationIntegrationExtra', $parts[0])) 'The parent test filter is not anchored'
            foreach ($engine in @('Postgres', 'MySQL', 'PostgresExtra', 'MySQLExtra', 'OtherMySQL')) {
                # Check behavior against real and near-matching names, rather than one literal regex
                Assert-Condition ([regex]::IsMatch($engine, $parts[1]) -eq ($engines -contains $engine)) "Unexpected replication engine selected: $engine"
            }
        }
    }
    $script:checkedCases++
}

try {
    [IO.Directory]::CreateDirectory((Split-Path $runner)) | Out-Null
    $privateDirectory = Join-Path $sandbox '.local/replication-test'
    [IO.Directory]::CreateDirectory($privateDirectory) | Out-Null
    Copy-Item -LiteralPath (Join-Path $PSScriptRoot 'run-tests.ps1') -Destination $runner
    $fakeEnvironment.GetEnumerator() | ForEach-Object { $_.Key + '=' + $_.Value } |
        Set-Content -LiteralPath (Join-Path $privateDirectory 'test.env') -Encoding utf8NoBOM
    foreach ($database in @('All', 'Postgres', 'MySQL', 'all', 'pOsTgReS', 'mysql')) {
        Test-RunnerCase $database $false $false
        Test-RunnerCase $database $true $false
        Test-RunnerCase $database $true $true
        Test-RunnerCase $database $true $true 1
        $replicationCall = if ($database -eq 'All') { 3 } else { 2 }
        Test-RunnerCase $database $true $true $replicationCall
    }

    # Confirm an uncaught runner failure produces a nonzero process exit as CI expects
    $wrapper = Join-Path $sandbox 'invoke-failure.ps1'
    @'
param([string]$Runner)
function go { $global:LASTEXITCODE = 23 }
& $Runner -Database MySQL -IncludeReplication
'@ | Set-Content -LiteralPath $wrapper -Encoding utf8NoBOM
    $pwshName = if ($IsWindows) { 'pwsh.exe' } else { 'pwsh' }
    $failureOutput = & (Join-Path $PSHOME $pwshName) -NoProfile -File $wrapper -Runner $runner 2>&1
    Assert-Condition ($LASTEXITCODE -ne 0) 'The failed runner returned a successful process exit'
    Assert-Condition (-not (($failureOutput | Out-String) -match 'Selected database verification completed')) 'The failed process reported success'
    Write-Host "Runner regression passed: $($script:checkedCases) cases and a nonzero-exit check"
} finally {
    foreach ($name in $environmentNames) {
        if ($null -eq $originalEnvironment[$name]) { Remove-Item -LiteralPath "Env:$name" -ErrorAction SilentlyContinue }
        else { [Environment]::SetEnvironmentVariable($name, $originalEnvironment[$name], 'Process') }
    }
    if ($null -ne $originalLastExit) { $global:LASTEXITCODE = $originalLastExitValue }
    else { Remove-Variable LASTEXITCODE -Scope Global -ErrorAction SilentlyContinue }
    # Recursive cleanup must remain confined to our unique direct child of the temp directory
    $resolvedSandbox = [IO.Path]::GetFullPath($sandbox)
    if ([IO.Path]::GetDirectoryName($resolvedSandbox) -ne $tempParent -or [IO.Path]::GetFileName($resolvedSandbox) -notmatch '^heago-run-tests-[a-f0-9]{32}$') {
        throw 'Refusing cleanup outside the runner regression temp directory'
    }
    if (Test-Path -LiteralPath $resolvedSandbox) { Remove-Item -LiteralPath $resolvedSandbox -Recurse -Force }
}
