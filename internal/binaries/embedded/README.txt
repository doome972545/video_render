This directory holds the external tool executables that get embedded into the
binary when building with `-tags embed_binaries`.

The actual executables (ffmpeg, ffprobe, yt-dlp) are NOT committed to git
because they are large (~480 MB total) and are third-party redistributables.

To populate this directory before a portable build, run:

    # Windows (PowerShell)
    ./scripts/fetch-binaries.ps1

Expected files after fetching (Windows):
    ffmpeg.exe
    ffprobe.exe
    yt-dlp.exe

This README.txt is a committed placeholder so the directory exists on a fresh
clone; //go:embed embedded/* requires at least one matching file to compile.
The extractor ignores non-executable files like this one.
