# Regenera docs/detection-report.json a partir do corpus sintetico rotulado.
# O comando Go abaixo e o contrato do relatorio e funciona igual em Linux e macOS.
$ErrorActionPreference = 'Stop'
$root = Split-Path -Parent $PSScriptRoot
Push-Location $root
try {
    go test ./internal/sensitivedata -run '^TestCorpusReport$' -count=1 -args -detection-report=../../docs/detection-report.json
    if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }
    Write-Output 'docs/detection-report.json atualizado'
}
finally {
    Pop-Location
}
