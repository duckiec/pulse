# Pulse one-shot installer for Windows (PowerShell 5.1+).
#
#   irm https://raw.githubusercontent.com/duckiec/pulse/main/install.ps1 | iex
#
# Parameters (also accepted as PULSE_VERSION / PULSE_PREFIX env):
#   -Version vX.Y.Z   install a pinned release (default: latest)
#   -Prefix DIR       install directory (default: %LocalAppData%\pulse\bin)
#
# What it does: detect architecture, resolve the release tag, download the
# matching binary plus checksums.txt, verify SHA-256 fail-closed, install,
# then self-check (`pulse --version` + `pulse doctor` in an isolated temp
# HOME). It never touches your workspace and never edits your PATH for
# you: the exact command to add it is printed, not run.
[CmdletBinding()]
param(
    [string]$Version = $env:PULSE_VERSION,
    [string]$Prefix = $env:PULSE_PREFIX
)

$ErrorActionPreference = "Stop"
$Repo = "duckiec/pulse"

function Fail($msg) { Write-Error "install.ps1: $msg" -ErrorAction Stop }

if ([string]::IsNullOrWhiteSpace($Prefix)) {
    $Prefix = Join-Path $env:LOCALAPPDATA "pulse\bin"
}
if ($Version -notmatch '^v\d+\.\d+\.\d+(-[0-9A-Za-z.]+)?$') {
    if ([string]::IsNullOrWhiteSpace($Version)) {
        # Latest non-prerelease, non-draft release for the repository.
        $latest = Invoke-RestMethod -Uri "https://api.github.com/repos/$Repo/releases/latest"
        $Version = $latest.tag_name
    }
    if ($Version -notmatch '^v\d+\.\d+\.\d+(-[0-9A-Za-z.]+)?$') {
        Fail "--Version must look like v1.2.3 (got '$Version')"
    }
}

switch ($env:PROCESSOR_ARCHITECTURE) {
    "AMD64" { $goarch = "amd64" }
    "ARM64" { $goarch = "arm64" }
    default { Fail "unsupported architecture '$($env:PROCESSOR_ARCHITECTURE)' (amd64 and arm64 only)" }
}

$asset = "pulse-windows-$goarch.exe"
$base = "https://github.com/$Repo/releases/download/$Version"
$work = Join-Path ([System.IO.Path]::GetTempPath()) ("pulse-install-" + [System.Guid]::NewGuid().ToString("N"))
New-Item -ItemType Directory -Path $work | Out-Null
try {
    # TLS 1.2 floor on Windows PowerShell 5.1, whose default still
    # negotiates down to 1.0 against some endpoints.
    [Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12
    Invoke-WebRequest -Uri "$base/$asset" -OutFile (Join-Path $work $asset)
    Invoke-WebRequest -Uri "$base/checksums.txt" -OutFile (Join-Path $work "checksums.txt")

    # Fail closed: the checksums file must name the asset, and the hash
    # must match exactly (compared case-insensitively: hex case is free).
    $want = Get-Content -Path (Join-Path $work "checksums.txt") |
        ForEach-Object { $f = ($_ -split '\s+'); if ($f.Count -ge 2 -and $f[1] -eq $asset) { $f[0] } } |
        Select-Object -First 1
    if ([string]::IsNullOrWhiteSpace($want)) { Fail "checksums.txt names no $asset entry" }
    $got = (Get-FileHash -Path (Join-Path $work $asset) -Algorithm SHA256).Hash
    if ($got -ne $want.ToUpperInvariant()) { Fail "checksum mismatch for $asset" }

    New-Item -ItemType Directory -Path $Prefix -Force | Out-Null
    Copy-Item -Path (Join-Path $work $asset) -Destination (Join-Path $Prefix "pulse.exe") -Force

    # Self-check in an isolated HOME (never touches your config).
    $checkHome = Join-Path ([System.IO.Path]::GetTempPath()) ("pulse-check-" + [System.Guid]::NewGuid().ToString("N"))
    $env:PULSE_HOME = $checkHome
    $env:PULSE_SKIP_SYNC = "1"
    try {
        & (Join-Path $Prefix "pulse.exe") --version
        if ($LASTEXITCODE -ne 0) { Fail "installed binary failed --version" }
        & (Join-Path $Prefix "pulse.exe") doctor *> $null
        if ($LASTEXITCODE -ne 0) { Fail "installed binary failed its doctor self-check" }
    }
    finally {
        Remove-Item Env:\PULSE_HOME -ErrorAction SilentlyContinue
        Remove-Item Env:\PULSE_SKIP_SYNC -ErrorAction SilentlyContinue
        Remove-Item -Recurse -Force $checkHome -ErrorAction SilentlyContinue
    }

    Write-Output "pulse $Version installed to $Prefix\pulse.exe"
    $userPath = [Environment]::GetEnvironmentVariable("Path", "User")
    if (($userPath -split ';') -notcontains $Prefix) {
        Write-Output "add it to your PATH, then restart the terminal:"
        Write-Output "  setx PATH `"$Prefix;%PATH%`""
    }
    Write-Output "next: cd your-api; pulse init; pulse vault set DEMO_TOKEN; pulse doctor"
}
finally {
    Remove-Item -Recurse -Force $work -ErrorAction SilentlyContinue
}
