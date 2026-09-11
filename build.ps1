$ErrorActionPreference = "Stop"
$root = Split-Path -Parent $MyInvocation.MyCommand.Path
$output = Join-Path $root "..\..\outputs\WorkBuddyHelper.exe"
New-Item -ItemType Directory -Force -Path (Split-Path $output) | Out-Null
Push-Location $root
try {
    go test ./...
    go vet ./...
    go build -trimpath -buildvcs=false -ldflags "-s -w" -o $output .\cmd\workbuddy-helper
} finally {
    Pop-Location
}
Write-Host "Built $output"

