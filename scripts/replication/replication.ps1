#!/usr/bin/env pwsh
# These disposable databases verify real asynchronous replication, never production failover.
[CmdletBinding()]
param(
    [ValidateSet('Start', 'Status', 'Stop', 'StopReplica', 'StartReplica', 'Pause', 'Resume', 'Clean')]
    [string]$Action = 'Start',
    [ValidateSet('All', 'Postgres', 'MySQL')]
    [string]$Database = 'All'
)

$ErrorActionPreference = 'Stop'
$PSNativeCommandUseErrorActionPreference = $false
$taskRoot = [IO.Path]::GetFullPath((Join-Path $PSScriptRoot '../..'))
$stateRoot = Join-Path $taskRoot '.local/replication-test'
$credentialsFile = Join-Path $stateRoot 'credentials.json'
$environmentFile = Join-Path $stateRoot 'test.env'
$networkName = 'heago-rw-test-network'
$resourceLabel = 'heago.replication-test=true'
$postgresNames = @('heago-rw-test-pg-primary', 'heago-rw-test-pg-replica')
$mysqlNames = @('heago-rw-test-mysql-primary', 'heago-rw-test-mysql-replica')
$postgresVolumes = @('heago-rw-test-pg-primary-data', 'heago-rw-test-pg-replica-data')
$mysqlVolumes = @('heago-rw-test-mysql-primary-data', 'heago-rw-test-mysql-replica-data')
$credentials = $null
$probeValue = [Guid]::NewGuid().ToString('N')

function New-Password([int]$ByteCount = 24) {
    # Hex avoids SQL, URI and Docker env-file escaping ambiguities without weakening entropy.
    return [Convert]::ToHexString([Security.Cryptography.RandomNumberGenerator]::GetBytes($ByteCount)).ToLowerInvariant()
}

function Protect-Output([string]$Value) {
    if ($null -ne $credentials) {
        foreach ($item in $credentials.Values) {
            if ($item -is [string] -and $item.Length -ge 16) { $Value = $Value.Replace($item, '[redacted]') }
        }
    }
    return $Value
}

function Invoke-Docker {
    param([string[]]$Arguments, [string]$InputText)
    $result = if ($PSBoundParameters.ContainsKey('InputText')) {
        $InputText | & docker @Arguments 2>&1
    } else {
        & docker @Arguments 2>&1
    }
    if ($LASTEXITCODE -ne 0) { throw (Protect-Output (($result | ForEach-Object { "$_" }) -join "`n")) }
    return ($result | ForEach-Object { "$_" })
}

function Test-Resource([string]$Kind, [string]$Name) {
    $format = if ($Kind -eq 'container') { '{{index .Config.Labels "heago.replication-test"}}' } else { '{{index .Labels "heago.replication-test"}}' }
    $result = & docker $Kind inspect $Name --format $format 2>$null
    if ($LASTEXITCODE -ne 0) { return $false }
    if ($result -ne 'true') { throw "Refusing to manage an unrelated $Kind named $Name." }
    return $true
}

function Ensure-Volume([string]$Name) {
    if (-not (Test-Resource volume $Name)) { $null = Invoke-Docker -Arguments @('volume', 'create', '--label', $resourceLabel, $Name) }
}

function Assert-PortAvailable([int]$Port) {
    # Bind only the address exposed by this harness, so an occupied port fails before Docker mutation.
    $listener = [Net.Sockets.TcpListener]::new([Net.IPAddress]::Loopback, $Port)
    try { $listener.Start() } catch { throw "127.0.0.1:$Port is occupied; stop this harness or select another machine." }
    finally { $listener.Stop() }
}

function Start-Existing([string]$Name) {
    if (Test-Resource container $Name) { $null = Invoke-Docker -Arguments @('start', $Name); return $true }
    return $false
}

function Wait-Ready([string]$Name, [string]$Kind) {
    $deadline = [DateTime]::UtcNow.AddSeconds(180)
    do {
        $arguments = if ($Kind -eq 'Postgres') { @('exec', $Name, 'pg_isready', '-h', '127.0.0.1', '-U', 'heago_admin', '-d', 'heago') }
        else { @('exec', $Name, 'sh', '-ec', 'MYSQL_PWD="$MYSQL_ROOT_PASSWORD" exec mysql --protocol=tcp -uroot --batch --skip-column-names -e "SELECT 1"') }
        $null = & docker @arguments 2>&1
        if ($LASTEXITCODE -eq 0) { return }
        Start-Sleep -Milliseconds 1000
    } while ([DateTime]::UtcNow -lt $deadline)
    throw "$Name did not become ready in 180 seconds. Inspect its local Docker logs."
}

function Invoke-Pg([string]$Name, [string]$SQL) {
    return Invoke-Docker -Arguments @('exec', '-i', $Name, 'psql', '-X', '-v', 'ON_ERROR_STOP=1', '-U', 'heago_admin', '-d', 'heago', '-At') -InputText $SQL
}

function Invoke-MySQL([string]$Name, [string]$SQL) {
    return Invoke-Docker -Arguments @('exec', '-i', $Name, 'sh', '-ec', 'MYSQL_PWD="$MYSQL_ROOT_PASSWORD" exec mysql --protocol=tcp -uroot --batch --skip-column-names') -InputText $SQL
}

function Initialize-State {
    $null = New-Item -ItemType Directory -Path $stateRoot -Force
    if (Test-Path -LiteralPath $credentialsFile) {
        $script:credentials = Get-Content -LiteralPath $credentialsFile -Raw | ConvertFrom-Json -AsHashtable
    } else {
        # Reusing existing data with new passwords would conceal a partial cleanup mistake.
        foreach ($name in ($postgresNames + $mysqlNames)) {
            if (Test-Resource container $name) { throw 'Harness containers exist but credentials.json is absent; recover that file or run Clean first.' }
        }
        $script:credentials = @{
            PostgresAdmin = New-Password; PostgresWrite = New-Password; PostgresRead = New-Password; PostgresReplication = New-Password
            MySQLAdmin = New-Password; MySQLWrite = New-Password; MySQLRead = New-Password; MySQLReplication = New-Password 16
        }
        $credentials | ConvertTo-Json | Set-Content -LiteralPath $credentialsFile -Encoding utf8NoBOM
    }
    if ($credentials.MySQLReplication.Length -gt 32) {
        # MySQL's replication protocol limits SOURCE_PASSWORD to 32 characters.
        $credentials.MySQLReplication = New-Password 16
        $credentials | ConvertTo-Json | Set-Content -LiteralPath $credentialsFile -Encoding utf8NoBOM
    }
    @("POSTGRES_USER=heago_admin", "POSTGRES_DB=heago", "POSTGRES_PASSWORD=$($credentials.PostgresAdmin)") | Set-Content (Join-Path $stateRoot 'postgres-primary.env') -Encoding utf8NoBOM
    @("PGDATA=/var/lib/postgresql/data", "PGPASSWORD=$($credentials.PostgresReplication)") | Set-Content (Join-Path $stateRoot 'postgres-replica.env') -Encoding utf8NoBOM
    @("MYSQL_ROOT_PASSWORD=$($credentials.MySQLAdmin)", 'MYSQL_ROOT_HOST=%') | Set-Content (Join-Path $stateRoot 'mysql.env') -Encoding utf8NoBOM
    @(
        "HEAGO_TEST_POSTGRES_PRIMARY_DSN=postgres://heago_write:$($credentials.PostgresWrite)@127.0.0.1:55435/heago?sslmode=disable"
        "HEAGO_TEST_POSTGRES_REPLICA_DSN=postgres://heago_read:$($credentials.PostgresRead)@127.0.0.1:55436/heago?sslmode=disable"
        "HEAGO_TEST_MYSQL_PRIMARY_DSN=heago_write:$($credentials.MySQLWrite)@tcp(127.0.0.1:53316)/heago?charset=utf8mb4&parseTime=true&loc=UTC"
        "HEAGO_TEST_MYSQL_REPLICA_DSN=heago_read:$($credentials.MySQLRead)@tcp(127.0.0.1:53317)/heago?charset=utf8mb4&parseTime=true&loc=UTC"
        "HEAGO_TEST_POSTGRES_ADMIN_DSN=postgres://heago_admin:$($credentials.PostgresAdmin)@127.0.0.1:55435/heago?sslmode=disable"
        "HEAGO_TEST_MYSQL_ADMIN_DSN=root:$($credentials.MySQLAdmin)@tcp(127.0.0.1:53316)/heago?charset=utf8mb4&parseTime=true&loc=UTC"
        "HEAGO_TEST_DATABASE_URL=postgres://heago_admin:$($credentials.PostgresAdmin)@127.0.0.1:55435/heago?sslmode=disable"
        "HEAGO_TEST_MYSQL_DSN=root:$($credentials.MySQLAdmin)@tcp(127.0.0.1:53316)/heago?charset=utf8mb4&parseTime=true&loc=UTC"
    ) | Set-Content -LiteralPath $environmentFile -Encoding utf8NoBOM
    # state.json is private too; consumers can build config structs without parsing DSNs.
    @{
        postgres = @{
            primary = @{ host = '127.0.0.1'; port = 55435; name = 'heago'; user = 'heago_write'; password = $credentials.PostgresWrite; sslmode = 'disable' }
            replica = @{ host = '127.0.0.1'; port = 55436; name = 'heago'; user = 'heago_read'; password = $credentials.PostgresRead; sslmode = 'disable' }
        }
        mysql = @{
            primary = @{ host = '127.0.0.1'; port = 53316; name = 'heago'; user = 'heago_write'; password = $credentials.MySQLWrite; tls = 'false' }
            replica = @{ host = '127.0.0.1'; port = 53317; name = 'heago'; user = 'heago_read'; password = $credentials.MySQLRead; tls = 'false' }
        }
    } | ConvertTo-Json -Depth 5 | Set-Content -LiteralPath (Join-Path $stateRoot 'state.json') -Encoding utf8NoBOM
}

function Start-Postgres {
    $primary = $postgresNames[0]; $replica = $postgresNames[1]
    foreach ($volume in $postgresVolumes) { Ensure-Volume $volume }
    if (-not (Start-Existing $primary)) {
        Assert-PortAvailable 55435
        $null = Invoke-Docker -Arguments @('run', '-d', '--name', $primary, '--label', $resourceLabel, '--network', $networkName, '--network-alias', 'pg-primary', '-p', '127.0.0.1:55435:5432', '--env-file', (Join-Path $stateRoot 'postgres-primary.env'), '--mount', "type=volume,src=$($postgresVolumes[0]),dst=/var/lib/postgresql/data", 'postgres:16', '-c', 'wal_level=replica', '-c', 'max_wal_senders=4', '-c', 'max_replication_slots=4')
    }
    Wait-Ready $primary Postgres
    $null = Invoke-Pg $primary @"
DO `$`$ BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'heago_write') THEN CREATE ROLE heago_write LOGIN PASSWORD '$($credentials.PostgresWrite)'; END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'heago_read') THEN CREATE ROLE heago_read LOGIN PASSWORD '$($credentials.PostgresRead)'; END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'heago_repl') THEN CREATE ROLE heago_repl REPLICATION LOGIN PASSWORD '$($credentials.PostgresReplication)'; END IF;
END `$`$;
ALTER DATABASE heago OWNER TO heago_write;
GRANT ALL ON SCHEMA public TO heago_write;
GRANT CONNECT ON DATABASE heago TO heago_read;
GRANT USAGE ON SCHEMA public TO heago_read;
ALTER DEFAULT PRIVILEGES FOR ROLE heago_write IN SCHEMA public GRANT SELECT ON TABLES TO heago_read;
ALTER DEFAULT PRIVILEGES FOR ROLE heago_write GRANT USAGE ON SCHEMAS TO heago_read;
ALTER DEFAULT PRIVILEGES FOR ROLE heago_write GRANT SELECT ON TABLES TO heago_read;
GRANT SELECT ON ALL TABLES IN SCHEMA public TO heago_read;
CREATE TABLE IF NOT EXISTS public.heago_replication_probe (id integer PRIMARY KEY, value text NOT NULL);
INSERT INTO public.heago_replication_probe VALUES (1, '$probeValue') ON CONFLICT (id) DO UPDATE SET value = EXCLUDED.value;
GRANT SELECT, INSERT, UPDATE, DELETE ON public.heago_replication_probe TO heago_write;
GRANT SELECT ON public.heago_replication_probe TO heago_read;
"@
    $null = Invoke-Docker -Arguments @('exec', $primary, 'sh', '-ec', 'if ! grep -q "^host replication heago_repl " "$PGDATA/pg_hba.conf"; then printf "host replication heago_repl all scram-sha-256\n" >> "$PGDATA/pg_hba.conf"; fi')
    $null = Invoke-Pg $primary 'SELECT pg_reload_conf();'
    if (-not (Start-Existing $replica)) {
        Assert-PortAvailable 55436
        # The replica volume belongs only to this harness. Refuse stale partial data rather than delete it silently.
        $null = Invoke-Docker -Arguments @('run', '--rm', '--name', 'heago-rw-test-pg-basebackup', '--label', $resourceLabel, '--network', $networkName, '--env-file', (Join-Path $stateRoot 'postgres-replica.env'), '--mount', "type=volume,src=$($postgresVolumes[1]),dst=/var/lib/postgresql/data", '--entrypoint', 'sh', 'postgres:16', '-ec', 'test -z "$(ls -A "$PGDATA")" || { echo "Replica volume is nonempty; run Clean before rebuilding." >&2; exit 1; }; chown postgres:postgres "$PGDATA"; chmod 700 "$PGDATA"; exec gosu postgres pg_basebackup -h pg-primary -U heago_repl -D "$PGDATA" -R -X stream')
        $null = Invoke-Docker -Arguments @('run', '-d', '--name', $replica, '--label', $resourceLabel, '--network', $networkName, '-p', '127.0.0.1:55436:5432', '--mount', "type=volume,src=$($postgresVolumes[1]),dst=/var/lib/postgresql/data", 'postgres:16', '-c', 'hot_standby=on')
    }
    Wait-Ready $replica Postgres
    $null = Invoke-Pg $replica 'SELECT pg_wal_replay_resume();'
}

function Start-MySQL {
    $primary = $mysqlNames[0]; $replica = $mysqlNames[1]
    foreach ($volume in $mysqlVolumes) { Ensure-Volume $volume }
    if (-not (Start-Existing $primary)) {
        Assert-PortAvailable 53316
        $null = Invoke-Docker -Arguments @('run', '-d', '--name', $primary, '--label', $resourceLabel, '--network', $networkName, '--network-alias', 'mysql-primary', '-p', '127.0.0.1:53316:3306', '--env-file', (Join-Path $stateRoot 'mysql.env'), '--mount', "type=volume,src=$($mysqlVolumes[0]),dst=/var/lib/mysql", 'mysql:8.4', '--server-id=1', '--log-bin=mysql-bin', '--gtid-mode=ON', '--enforce-gtid-consistency=ON')
    }
    Wait-Ready $primary MySQL
    $null = Invoke-MySQL $primary @"
CREATE DATABASE IF NOT EXISTS heago CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci;
CREATE USER IF NOT EXISTS 'heago_write'@'%' IDENTIFIED BY '$($credentials.MySQLWrite)';
CREATE USER IF NOT EXISTS 'heago_read'@'%' IDENTIFIED BY '$($credentials.MySQLRead)';
CREATE USER IF NOT EXISTS 'heago_repl'@'%' IDENTIFIED BY '$($credentials.MySQLReplication)';
ALTER USER 'heago_repl'@'%' IDENTIFIED BY '$($credentials.MySQLReplication)';
GRANT ALL PRIVILEGES ON heago.* TO 'heago_write'@'%';
GRANT SELECT, SHOW VIEW ON heago.* TO 'heago_read'@'%';
GRANT REPLICATION SLAVE ON *.* TO 'heago_repl'@'%';
CREATE TABLE IF NOT EXISTS heago.heago_replication_probe (id INT PRIMARY KEY, value VARCHAR(255) NOT NULL);
INSERT INTO heago.heago_replication_probe VALUES (1, '$probeValue') ON DUPLICATE KEY UPDATE value = '$probeValue';
"@
    if (-not (Start-Existing $replica)) {
        Assert-PortAvailable 53317
        # read_only is enabled after first-time initialization, because MySQL's initializer must write.
        $null = Invoke-Docker -Arguments @('run', '-d', '--name', $replica, '--label', $resourceLabel, '--network', $networkName, '-p', '127.0.0.1:53317:3306', '--env-file', (Join-Path $stateRoot 'mysql.env'), '--mount', "type=volume,src=$($mysqlVolumes[1]),dst=/var/lib/mysql", 'mysql:8.4', '--server-id=2', '--log-bin=mysql-bin', '--relay-log=relay-bin', '--gtid-mode=ON', '--enforce-gtid-consistency=ON')
    }
    Wait-Ready $replica MySQL
    $configured = Invoke-MySQL $replica 'SELECT COUNT(*) FROM performance_schema.replication_connection_configuration;'
    if ($configured -contains '0') {
        $null = Invoke-MySQL $replica @"
CHANGE REPLICATION SOURCE TO SOURCE_HOST='mysql-primary', SOURCE_PORT=3306, SOURCE_USER='heago_repl', SOURCE_PASSWORD='$($credentials.MySQLReplication)', SOURCE_AUTO_POSITION=1, GET_SOURCE_PUBLIC_KEY=1;
START REPLICA;
SET PERSIST read_only=ON;
SET PERSIST super_read_only=ON;
"@
    } else {
        $null = Invoke-MySQL $replica 'START REPLICA; SET PERSIST read_only=ON; SET PERSIST super_read_only=ON;'
    }
}

function Wait-Replication {
    param([string]$Kind)
    $deadline = [DateTime]::UtcNow.AddSeconds(90)
    do {
        try {
            if ($Kind -eq 'Postgres') {
                $result = Invoke-Pg $postgresNames[1] "SELECT value FROM public.heago_replication_probe WHERE id=1;"
            } else {
                $result = Invoke-MySQL $mysqlNames[1] 'SELECT value FROM heago.heago_replication_probe WHERE id=1;'
            }
            if ($result -contains $probeValue) { return }
        } catch { }
        Start-Sleep -Milliseconds 500
    } while ([DateTime]::UtcNow -lt $deadline)
    throw "$Kind replication did not catch up in 90 seconds."
}

function Get-SelectedNames {
    if ($Database -eq 'Postgres') { return $postgresNames }
    if ($Database -eq 'MySQL') { return $mysqlNames }
    return ($postgresNames + $mysqlNames)
}

# Start never mutates existing unrelated containers, networks or volumes, even when names collide.
if ($Action -eq 'Start') {
    Initialize-State
    if (-not (Test-Resource network $networkName)) { $null = Invoke-Docker -Arguments @('network', 'create', '--label', $resourceLabel, $networkName) }
    if ($Database -ne 'MySQL') { Write-Host 'Starting PostgreSQL 16 primary and replica...'; Start-Postgres; Wait-Replication Postgres }
    if ($Database -ne 'Postgres') { Write-Host 'Starting MySQL 8.4 primary and replica...'; Start-MySQL; Wait-Replication MySQL }
    Write-Host "Ready. Private environment file: $environmentFile"
    if ($Database -ne 'MySQL') { Write-Host 'PostgreSQL: 127.0.0.1:55435 / 55436.' }
    if ($Database -ne 'Postgres') { Write-Host 'MySQL: 127.0.0.1:53316 / 53317.' }
} elseif ($Action -eq 'Stop') {
    foreach ($name in (Get-SelectedNames)) { if (Test-Resource container $name) { $null = Invoke-Docker -Arguments @('stop', '--time', '15', $name) } }
    Write-Host 'Selected test containers stopped; volumes and credentials retained.'
} elseif ($Action -in 'StopReplica', 'StartReplica') {
    $replicas = if ($Database -eq 'Postgres') { @($postgresNames[1]) } elseif ($Database -eq 'MySQL') { @($mysqlNames[1]) } else { @($postgresNames[1], $mysqlNames[1]) }
    foreach ($name in $replicas) {
        if (-not (Test-Resource container $name)) { throw "Test replica $name does not exist; run Start first." }
        if ($Action -eq 'StopReplica') {
            $null = Invoke-Docker -Arguments @('stop', '--time', '15', $name)
        } else {
            $null = Invoke-Docker -Arguments @('start', $name)
            $kind = if ($name -eq $postgresNames[1]) { 'Postgres' } else { 'MySQL' }
            Wait-Ready $name $kind
        }
    }
    Write-Host "Selected replica container action: $Action. Primary containers unchanged."
} elseif ($Action -eq 'Clean') {
    # Exact, labelled Docker resources only. No recursive host filesystem deletion is used.
    foreach ($name in (Get-SelectedNames)) { if (Test-Resource container $name) { $null = Invoke-Docker -Arguments @('rm', '-f', $name) } }
    $volumes = if ($Database -eq 'Postgres') { $postgresVolumes } elseif ($Database -eq 'MySQL') { $mysqlVolumes } else { $postgresVolumes + $mysqlVolumes }
    foreach ($name in $volumes) { if (Test-Resource volume $name) { $null = Invoke-Docker -Arguments @('volume', 'rm', $name) } }
    if ($Database -eq 'All' -and (Test-Resource network $networkName)) { $null = Invoke-Docker -Arguments @('network', 'rm', $networkName) }
    Write-Host 'Selected test Docker resources removed. Private files retained for review/reuse.'
} elseif ($Action -in 'Pause', 'Resume') {
    $pgSQL = if ($Action -eq 'Pause') { 'SELECT pg_wal_replay_pause();' } else { 'SELECT pg_wal_replay_resume();' }
    $mysqlSQL = if ($Action -eq 'Pause') { 'STOP REPLICA SQL_THREAD;' } else { 'START REPLICA SQL_THREAD;' }
    if ($Database -ne 'MySQL') { $null = Invoke-Pg $postgresNames[1] $pgSQL }
    if ($Database -ne 'Postgres') { $null = Invoke-MySQL $mysqlNames[1] $mysqlSQL }
    Write-Host "Selected replica replay action: $Action."
} else {
    foreach ($name in (Get-SelectedNames)) {
        if (Test-Resource container $name) { Invoke-Docker -Arguments @('inspect', $name, '--format', '{{.Name}}: {{.State.Status}}') }
    }
    if ($Database -ne 'MySQL' -and (Test-Resource container $postgresNames[1]) -and ((Invoke-Docker -Arguments @('inspect', $postgresNames[1], '--format', '{{.State.Running}}')) -eq 'true')) {
        Invoke-Pg $postgresNames[1] "SELECT 'PostgreSQL recovery=' || pg_is_in_recovery() || ', paused=' || pg_is_wal_replay_paused();"
    }
    if ($Database -ne 'Postgres' -and (Test-Resource container $mysqlNames[1]) -and ((Invoke-Docker -Arguments @('inspect', $mysqlNames[1], '--format', '{{.State.Running}}')) -eq 'true')) {
        Invoke-MySQL $mysqlNames[1] "SELECT CONCAT('MySQL read_only=', @@global.read_only, ', super_read_only=', @@global.super_read_only); SELECT CONCAT('MySQL applier=', SERVICE_STATE) FROM performance_schema.replication_applier_status;"
    }
}
