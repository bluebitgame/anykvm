# Run in Windows PowerShell with Go, a MinGW-w64 GCC, and Python on PATH.
# Use -Tags ffmpeg to enable FFmpeg; FFmpegBin optionally stages its runtime.
param(
    [string]$FFmpegBin = "",
    [string]$Tags = "",
    [string]$Compiler = "gcc",
    [string]$Output = "build/windows"
)
$ErrorActionPreference = "Stop"
if ($FFmpegBin -and !(($Tags -split '[,\s]+') -contains "ffmpeg")) {
    throw "FFmpegBin requires -Tags ffmpeg"
}
$Root = Split-Path $PSScriptRoot -Parent
$Saved = @{}
foreach ($Name in @("GOOS", "GOARCH", "CGO_ENABLED", "CC")) {
    $Saved[$Name] = [Environment]::GetEnvironmentVariable($Name, "Process")
}
Push-Location $Root
try {
    $env:GOOS = "windows"
    $env:GOARCH = "amd64"
    $env:CGO_ENABLED = "1"
    $env:CC = $Compiler
    $SourceArgs = @("tools/prepare_sources.py", "--platforms", "windows")
    if (($Tags -split '[,\s]+') -contains "ffmpeg") { $SourceArgs += "--ffmpeg" }
    & python @SourceArgs
    if ($LASTEXITCODE -ne 0) { throw "Source preparation failed" }
    New-Item -ItemType Directory -Force -Path $Output | Out-Null
    $BuildArgs = @("build", "-trimpath")
    if ($Tags) { $BuildArgs += @("-tags", $Tags) }
    $BuildArgs += @("-o", (Join-Path $Output "anykvm.exe"), "./cmd/anykvm")
    & go @BuildArgs
    if ($LASTEXITCODE -ne 0) { throw "Windows build failed" }
    & python @SourceArgs --stage-notices $Output
    if ($LASTEXITCODE -ne 0) { throw "License packaging failed" }
    if ($FFmpegBin) {
        & python tools/package_ffmpeg.py $FFmpegBin (Join-Path $Output "lib")
        if ($LASTEXITCODE -ne 0) { throw "DLL packaging failed" }
        Write-Host "Staged FFmpeg DLLs under $Output/lib."
    }
    if (!(($Tags -split '[,\s]+') -contains "ffmpeg")) {
        try {
            & python tools/package_opus.py --arch amd64 --output (Join-Path $Output "lib")
            if ($LASTEXITCODE -ne 0) { throw "Opus packaging failed" }
        } catch {
            Write-Warning "Opus DLL was not packaged: $_. Audio will be disabled unless a matching opus.dll is supplied."
        }
    }
    Write-Host "Built $Output/anykvm.exe. Service was not started."
} finally {
    Pop-Location
    foreach ($Name in $Saved.Keys) {
        [Environment]::SetEnvironmentVariable($Name, $Saved[$Name], "Process")
    }
}
