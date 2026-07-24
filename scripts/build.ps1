<#
.SYNOPSIS
    Builds the videoremix CLI in dev or portable mode.

.PARAMETER Mode
    "dev"      -> small binary, relies on ffmpeg/yt-dlp on PATH or in bin/.
    "portable" -> embeds ffmpeg/ffprobe/yt-dlp (fetches them first if missing).

.EXAMPLE
    ./scripts/build.ps1 -Mode dev
    ./scripts/build.ps1 -Mode portable
#>

param(
    [ValidateSet("dev", "portable")]
    [string]$Mode = "dev"
)

$ErrorActionPreference = "Stop"
$root = Split-Path -Parent (Split-Path -Parent $MyInvocation.MyCommand.Path)
Push-Location $root
try {
    if ($Mode -eq "portable") {
        $embeddedDir = Join-Path $root "internal\binaries\embedded"
        $haveAll = (Test-Path (Join-Path $embeddedDir "ffmpeg.exe")) -and
                   (Test-Path (Join-Path $embeddedDir "ffprobe.exe")) -and
                   (Test-Path (Join-Path $embeddedDir "yt-dlp.exe"))
        if (-not $haveAll) {
            Write-Host "Embedded binaries missing; fetching..."
            & (Join-Path $root "scripts\fetch-binaries.ps1")
        }
        Write-Host "Building portable binary (this embeds ~480MB, may take a while)..."
        go build -tags embed_binaries -o videoremix-portable.exe ./cmd/videoremix
        Get-Item "videoremix-portable.exe" | ForEach-Object { "Built: {0} ({1:N1} MB)" -f $_.Name, ($_.Length/1MB) }
    }
    else {
        Write-Host "Building dev binary..."
        go build -o videoremix.exe ./cmd/videoremix
        Get-Item "videoremix.exe" | ForEach-Object { "Built: {0} ({1:N1} MB)" -f $_.Name, ($_.Length/1MB) }
    }
}
finally {
    Pop-Location
}
