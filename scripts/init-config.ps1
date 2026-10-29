#requires -Version 7.0
<##
Generate private bootstrap files once. Re-running never rotates an initialized database's
password or overwrites an operator's configuration. No secret is written to stdout.
##>
[CmdletBinding()]
param()

$ErrorActionPreference = 'Stop'
$templateRoot = [IO.Path]::GetFullPath((Join-Path $PSScriptRoot '..'))
$examplePath = Join-Path $templateRoot 'configs/config.example.yaml'
$privatePath = Join-Path $templateRoot '.local'
$generatedPaths = @(
    (Join-Path $privatePath 'postgres_password'),
    (Join-Path $templateRoot 'configs/config.local.yaml'),
    (Join-Path $templateRoot 'configs/config.docker.yaml'),
    (Join-Path $privatePath 'compose.env')
)
$existingPaths = @($generatedPaths | Where-Object { Test-Path -LiteralPath $_ })
if ($existingPaths.Count -eq $generatedPaths.Count) {
    Write-Host 'Private configuration already exists; no files changed.'
    exit 0
}
if ($existingPaths.Count -ne 0) {
    throw 'Private configuration is incomplete. Resolve the existing files manually; nothing was overwritten.'
}

function New-RandomHex {
    param([int]$Length)
    return [Convert]::ToHexString([Security.Cryptography.RandomNumberGenerator]::GetBytes($Length)).ToLowerInvariant()
}

$databasePassword = New-RandomHex 32
$adminPassword = New-RandomHex 24
$encryptionKey = [Convert]::ToBase64String([Security.Cryptography.RandomNumberGenerator]::GetBytes(32))
$example = [IO.File]::ReadAllText($examplePath)
$configured = $example.Replace('REPLACE_WITH_DATABASE_PASSWORD', $databasePassword).
    Replace('REPLACE_WITH_ADMIN_PASSWORD', $adminPassword).
    Replace('REPLACE_WITH_BASE64_ENCRYPTION_KEY', $encryptionKey)
# Keep both files aligned: changing a database password later does not mutate PostgreSQL roles.
$localConfig = $configured.Replace('  port: 5432', '  port: 55434').Replace('  addr: ":8080"', '  addr: ":18081"')
$dockerConfig = $configured.Replace('  host: 127.0.0.1', '  host: postgres')
$configGroup = '0'
if (-not $IsWindows) {
    $configGroup = (& id -g).Trim()
    if ($LASTEXITCODE -ne 0 -or $configGroup -notmatch '^\d+$') { throw 'Cannot determine configuration file group.' }
}

[IO.Directory]::CreateDirectory($privatePath) | Out-Null
$encoding = [Text.UTF8Encoding]::new($false)
[IO.File]::WriteAllText($generatedPaths[0], $databasePassword + "`n", $encoding)
[IO.File]::WriteAllText($generatedPaths[1], $localConfig, $encoding)
[IO.File]::WriteAllText($generatedPaths[2], $dockerConfig, $encoding)
# This file contains only a host group ID; application values remain in the YAML file.
[IO.File]::WriteAllText($generatedPaths[3], "HEAGO_CONFIG_GID=$configGroup`n", $encoding)
if (-not $IsWindows) {
    & chmod 700 $privatePath
    if ($LASTEXITCODE -ne 0) { throw 'Cannot protect private directory.' }
    & chmod 600 $generatedPaths[0] $generatedPaths[1] $generatedPaths[3]
    if ($LASTEXITCODE -ne 0) { throw 'Cannot protect private configuration files.' }
    & chgrp $configGroup $generatedPaths[2]
    if ($LASTEXITCODE -ne 0) { throw 'Cannot set Docker configuration group.' }
    & chmod 640 $generatedPaths[2]
    if ($LASTEXITCODE -ne 0) { throw 'Cannot protect Docker configuration file.' }
}
Write-Host 'Created private PostgreSQL/bootstrap configuration; credentials were not printed.'
Write-Host 'Docker: docker compose --env-file .local/compose.env up -d --build app'
