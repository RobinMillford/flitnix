<#
.SYNOPSIS
    Flitnix Windows Installer Script
    https://github.com/itsyamin/flitnix

.DESCRIPTION
    Downloads and installs the latest or specified version of Flitnix on Windows x64.
    Performs integrity validation against SHA256SUMS.txt prior to execution.

.EXAMPLE
    irm https://raw.githubusercontent.com/itsyamin/flitnix/main/install.ps1 | iex

.EXAMPLE
    .\install.ps1 -Version "0.1.0" -DryRun
#>

[CmdletBinding()]
param(
    [Parameter(Mandatory = $false)]
    [string]$Version,

    [Parameter(Mandatory = $false)]
    [switch]$DryRun,

    [Parameter(Mandatory = $false)]
    [switch]$Silent,

    [Parameter(Mandatory = $false)]
    [switch]$Help
)

$ErrorActionPreference = "Stop"

if ($Help) {
    Get-Help $MyInvocation.MyCommand.Path
    exit 0
}

$Repo = "itsyamin/flitnix"
$DefaultVersion = "0.1.0"

# Check Architecture
if (-not [Environment]::Is64BitOperatingSystem) {
    Write-Error "Flitnix currently requires a 64-bit Windows operating system (x64)."
    exit 1
}

# Determine Version and Release Tag
if (-not [string]::IsNullOrWhiteSpace($Version)) {
    $CleanVersion = $Version.TrimStart('v')
    $ReleaseTag = "v$CleanVersion"
} else {
    try {
        $ReleaseInfo = Invoke-RestMethod -Uri "https://api.github.com/repos/$Repo/releases/latest" -UseBasicParsing -TimeoutSec 10
        $ReleaseTag = $ReleaseInfo.tag_name
        $CleanVersion = $ReleaseTag.TrimStart('v')
    } catch {
        $CleanVersion = $DefaultVersion
        $ReleaseTag = "v$CleanVersion"
    }
}

$AssetName = "flitnix_${CleanVersion}_windows_amd64_setup.exe"
$DownloadUrl = "https://github.com/$Repo/releases/download/$ReleaseTag/$AssetName"
$ChecksumUrl = "https://github.com/$Repo/releases/download/$ReleaseTag/SHA256SUMS.txt"

Write-Host "==> Flitnix Windows Installer" -ForegroundColor Cyan
Write-Host "==> Version: $CleanVersion ($ReleaseTag)"
Write-Host "==> Asset:   $AssetName"

if ($DryRun) {
    Write-Host "==> Dry-run mode enabled. Would download from:" -ForegroundColor Yellow
    Write-Host "      Installer: $DownloadUrl"
    Write-Host "      Checksum:  $ChecksumUrl"
    exit 0
}

# Temporary workspace
$TempDir = Join-Path ([System.IO.Path]::GetTempPath()) ("flitnix-install-" + [System.Guid]::NewGuid().ToString("N"))
New-Item -ItemType Directory -Path $TempDir -Force | Out-Null

try {
    $InstallerPath = Join-Path $TempDir $AssetName
    $ChecksumPath = Join-Path $TempDir "SHA256SUMS.txt"

    Write-Host "==> Downloading $AssetName..." -ForegroundColor Cyan
    Invoke-WebRequest -Uri $DownloadUrl -OutFile $InstallerPath -UseBasicParsing

    Write-Host "==> Downloading SHA256SUMS.txt..." -ForegroundColor Cyan
    Invoke-WebRequest -Uri $ChecksumUrl -OutFile $ChecksumPath -UseBasicParsing

    Write-Host "==> Verifying SHA-256 checksum..." -ForegroundColor Cyan
    $ActualHash = (Get-FileHash -Path $InstallerPath -Algorithm SHA256).Hash.ToLowerInvariant()
    $ChecksumLines = Get-Content -Path $ChecksumPath

    $ExpectedHash = $null
    foreach ($line in $ChecksumLines) {
        if ($line -match "^([a-fA-F0-9]{64})\s+\*?$([regex]::Escape($AssetName))$") {
            $ExpectedHash = $matches[1].ToLowerInvariant()
            break
        }
    }

    if (-not $ExpectedHash) {
        Write-Error "Asset $AssetName not found in SHA256SUMS.txt"
        exit 1
    }

    if ($ActualHash -ne $ExpectedHash) {
        Write-Error "Checksum verification FAILED!`n  Expected: $ExpectedHash`n  Actual:   $ActualHash"
        exit 1
    }

    Write-Host "==> Checksum verified OK ($ActualHash)" -ForegroundColor Green

    Write-Host "==> Launching Flitnix installer..." -ForegroundColor Cyan
    if ($Silent) {
        $process = Start-Process -FilePath $InstallerPath -ArgumentList "/S" -Wait -PassThru
    } else {
        $process = Start-Process -FilePath $InstallerPath -Wait -PassThru
    }

    if ($process.ExitCode -ne 0) {
        Write-Error "Installer exited with error code: $($process.ExitCode)"
        exit $process.ExitCode
    }

    Write-Host "==> Flitnix installed successfully!" -ForegroundColor Green
}
finally {
    if (Test-Path $TempDir) {
        Remove-Item -Path $TempDir -Recurse -Force -ErrorAction SilentlyContinue
    }
}
