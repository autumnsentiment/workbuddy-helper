$ErrorActionPreference = "Stop"
$root = Split-Path -Parent $MyInvocation.MyCommand.Path
$version = if ($args.Count -gt 0) { $args[0] } else { "1.0.2" }
$output = Join-Path $root "..\outputs\WorkBuddyHelper.exe"
New-Item -ItemType Directory -Force -Path (Split-Path $output) | Out-Null
Push-Location $root
try {
    go test ./...
    go vet ./...
    go build -trimpath -buildvcs=false -ldflags "-s -w -X main.appVersion=$version" -o $output .\cmd\workbuddy-helper
} finally {
    Pop-Location
}
Write-Host "Built v$version -> $output"
