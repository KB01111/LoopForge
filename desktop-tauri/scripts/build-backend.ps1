# Builds the LoopForge Go backend and places it at desktop-tauri/bin/loopforge-backend.exe,
# where tauri.conf.json bundles it as a resource.
param([string]$GoArch = "amd64")
$ErrorActionPreference = "Stop"
$root = (Resolve-Path (Join-Path $PSScriptRoot "..\..")).Path
$outDir = Join-Path $root "desktop-tauri\bin"
New-Item -ItemType Directory -Force $outDir | Out-Null
$out = Join-Path $outDir "loopforge-backend.exe"
Push-Location $root
try {
  $env:GOOS = "windows"; $env:GOARCH = $GoArch; $env:CGO_ENABLED = "0"
  go build -trimpath -ldflags "-s -w" -o $out ./cmd/agentflow
  if ($LASTEXITCODE -ne 0) { throw "go build failed" }
} finally { Pop-Location }
Write-Host "Built $out"
