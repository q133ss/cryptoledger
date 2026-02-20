# Генерация Go-кода из proto.
#
# Требования:
#   protoc 29+          https://github.com/protocolbuffers/protobuf/releases
#   protoc-gen-go       go install google.golang.org/protobuf/cmd/protoc-gen-go@v1.36.5
#   protoc-gen-go-grpc  go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@v1.5.1
#
# Запуск из корня репозитория:
#   powershell -ExecutionPolicy Bypass -File scripts/gen.ps1
#
# Готовые файлы лежат в gen/ и закоммичены: чтобы собрать проект, protoc не нужен.

$ErrorActionPreference = "Stop"
$root = Split-Path -Parent $PSScriptRoot
Set-Location $root

$protoc = $env:PROTOC
if (-not $protoc) { $protoc = "protoc" }

$include = $env:PROTOC_INCLUDE
if (-not $include) {
    # Если protoc установлен вручную, include обычно лежит рядом с bin.
    $candidate = Join-Path (Split-Path -Parent (Get-Command $protoc).Source) "..\include"
    if (Test-Path $candidate) { $include = $candidate }
}

$protocArgs = @(
    "-I", "proto",
    "--go_out=.", "--go_opt=module=cryptoledger",
    "--go-grpc_out=.", "--go-grpc_opt=module=cryptoledger",
    "proto/ledger/v1/ledger.proto"
)
if ($include) { $protocArgs = @("-I", $include) + $protocArgs }

& $protoc @protocArgs
Write-Host "OK: gen/ledger/v1 обновлён"
