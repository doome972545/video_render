<#
.SYNOPSIS
    Fetches ffmpeg, ffprobe and yt-dlp into internal/binaries/embedded/ so a
    portable build (-tags embed_binaries) can embed them.

.DESCRIPTION
    Installs the tools via winget if needed, locates the installed executables,
    and copies them into the embedded/ directory. The copied executables are
    gitignored; only the placeholder README.txt is committed.

.EXAMPLE
    ./scripts/fetch-binaries.ps1
#>

$ErrorActionPreference = "Stop"

# Resolve the embedded/ directory relative to this script.
$scriptDir = Split-Path -Parent $MyInvocation.MyCommand.Path
$embeddedDir = Join-Path (Split-Path -Parent $scriptDir) "internal\binaries\embedded"
New-Item -ItemType Directory -Path $embeddedDir -Force | Out-Null

Write-Host "Embedded dir: $embeddedDir"

function Ensure-Winget-Package($id) {
    Write-Host "Installing $id via winget (skip if already present)..."
    winget install --id $id -e --accept-source-agreements --accept-package-agreements --disable-interactivity 2>&1 | Out-Null
}

Ensure-Winget-Package "Gyan.FFmpeg"
Ensure-Winget-Package "yt-dlp.yt-dlp"

$pkgRoot = Join-Path $env:LOCALAPPDATA "Microsoft\WinGet\Packages"

function Copy-First($filter, $destName) {
    $found = Get-ChildItem $pkgRoot -Recurse -Filter $filter -ErrorAction SilentlyContinue |
        Sort-Object Length -Descending | Select-Object -First 1
    if (-not $found) {
        throw "Could not find $filter under $pkgRoot. Is winget install complete?"
    }
    $dest = Join-Path $embeddedDir $destName
    Copy-Item $found.FullName $dest -Force
    "{0,10:N1} MB  {1}" -f ($found.Length / 1MB), $destName | Write-Host
}

Copy-First "ffmpeg.exe"  "ffmpeg.exe"
Copy-First "ffprobe.exe" "ffprobe.exe"
Copy-First "yt-dlp.exe"  "yt-dlp.exe"

Write-Host ""
Write-Host "Done. You can now build the portable binary:"
Write-Host "    go build -tags embed_binaries -o videoremix-portable.exe ./cmd/videoremix"
