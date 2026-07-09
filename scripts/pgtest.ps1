# Spin up a throwaway Postgres, apply db/schema.sql, and run the Postgres store
# integration test. Requires Docker. Usage:  ./scripts/pgtest.ps1
$ErrorActionPreference = "Stop"

$name = "tollgate-pgtest"
$pass = "postgres"
$port = 55432
$dsn  = "postgres://postgres:$pass@localhost:$port/postgres?sslmode=disable"
$root = Split-Path -Parent $PSScriptRoot
$go   = "$env:ProgramFiles\Go\bin\go.exe"

try { docker rm -f $name 2>&1 | Out-Null } catch {}
docker run -d --name $name -e POSTGRES_PASSWORD=$pass -p "${port}:5432" postgres:16-alpine | Out-Null

try {
    Write-Host "waiting for postgres..."
    for ($i = 0; $i -lt 30; $i++) {
        docker exec $name pg_isready -U postgres 2>$null | Out-Null
        if ($LASTEXITCODE -eq 0) { break }
        Start-Sleep -Milliseconds 500
    }

    Get-Content "$root\db\schema.sql" -Raw | docker exec -i $name psql -U postgres -q
    Write-Host "schema applied; running test..."

    $env:TOLLGATE_TEST_DB = $dsn
    & $go test "$root\internal\ledger\..." -run TestPGStore -count=1 -v
}
finally {
    try { docker rm -f $name 2>&1 | Out-Null } catch {}
}
