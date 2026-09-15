# ============================================
# AirCoins v1.10.1 — Build & Publish to Supabase Storage
# ============================================
# Run from the AirCoins project root:
#   .\publish-release.ps1
#
# Prerequisites:
#   - Git Bash (for cross-compiling Go binary for ARM)
#     OR a pre-compiled aircoins-api binary at:
#     system\usr\local\bin\aircoins-api\aircoins-api
#   - .env file with SUPABASE_URL and SUPABASE_SERVICE_ROLE_KEY
# ============================================

$ErrorActionPreference = "Stop"

$VERSION = "1.29.14"
$TARBALL_NAME = "aircoins-v$VERSION.tar.gz"
$CHECKSUM_NAME = "aircoins-v$VERSION.sha256"
$BUCKET = "aircoins"
$PROJECT_ROOT = Split-Path -Parent $MyInvocation.MyCommand.Path

Write-Host ""
Write-Host "=========================================" -ForegroundColor Cyan
Write-Host "  AirCoins v$VERSION Builder & Publisher" -ForegroundColor Cyan
Write-Host "=========================================" -ForegroundColor Cyan
Write-Host ""

# --- Load .env ---
$envFile = Join-Path $PROJECT_ROOT ".env"
if (Test-Path $envFile) {
    Get-Content $envFile | ForEach-Object {
        if ($_ -match '^\s*([^#][^=]+)=(.*)$') {
            $key = $Matches[1].Trim()
            $val = $Matches[2].Trim()
            [System.Environment]::SetEnvironmentVariable($key, $val, "Process")
        }
    }
    Write-Host "[OK] Loaded .env" -ForegroundColor Green
} else {
    Write-Host "[ERROR] .env file not found!" -ForegroundColor Red
    exit 1
}

$SUPABASE_URL = $env:SUPABASE_URL.TrimEnd('/')
$SUPABASE_KEY = $env:SUPABASE_SERVICE_ROLE_KEY

if (-not $SUPABASE_URL -or -not $SUPABASE_KEY) {
    Write-Host "[ERROR] SUPABASE_URL and SUPABASE_SERVICE_ROLE_KEY must be set in .env" -ForegroundColor Red
    exit 1
}

# --- Check for tarball ---
$tarballPath = Join-Path $PROJECT_ROOT $TARBALL_NAME

if (-not (Test-Path $tarballPath)) {
    Write-Host ""
    Write-Host "Tarball not found. Attempting to build..." -ForegroundColor Yellow

    # Try to find Git Bash for cross-compilation
    $gitBash = "C:\Program Files\Git\bin\bash.exe"
    if (-not (Test-Path $gitBash)) {
        $gitBash = "C:\Program Files (x86)\Git\bin\bash.exe"
    }

    if (Test-Path $gitBash) {
        Write-Host "  Git Bash found at: $gitBash" -ForegroundColor Green
        Write-Host "  Running build-release.sh..." -ForegroundColor Yellow

        $buildScript = Join-Path $PROJECT_ROOT "build-release.sh"
        & $gitBash -c "cd '$($PROJECT_ROOT -replace '\\','/')' && bash build-release.sh $VERSION"

        if (Test-Path $tarballPath) {
            Write-Host "[OK] Tarball built successfully!" -ForegroundColor Green
        } else {
            Write-Host "[ERROR] Build completed but tarball not found!" -ForegroundColor Red
            Write-Host "  Make sure Go is installed and in PATH." -ForegroundColor Yellow
            exit 1
        }
    } else {
        Write-Host "[ERROR] No tarball found and Git Bash not available." -ForegroundColor Red
        Write-Host ""
        Write-Host "To build manually:" -ForegroundColor Yellow
        Write-Host "  1. Install Git for Windows (includes Git Bash)" -ForegroundColor Yellow
        Write-Host "  2. Or cross-compile the Go binary on Linux:" -ForegroundColor Yellow
        Write-Host "     CGO_ENABLED=0 GOOS=linux GOARCH=arm GOARM=7 go build -ldflags '-s -w -X main.Version=v$VERSION' -o aircoins-api ." -ForegroundColor Yellow
        Write-Host "  3. Place the binary at: system\usr\local\bin\aircoins-api\aircoins-api" -ForegroundColor Yellow
        Write-Host "  4. Run this script again." -ForegroundColor Yellow
        exit 1
    }
} else {
    Write-Host "[OK] Tarball found: $TARBALL_NAME" -ForegroundColor Green
}

# --- Compute SHA256 ---
Write-Host ""
Write-Host "Computing SHA256..." -ForegroundColor Yellow
$hash = Get-FileHash -Path $tarballPath -Algorithm SHA256
$SHA256 = $hash.Hash.ToLower()
Write-Host "  SHA256: $SHA256" -ForegroundColor Green

# --- Update checksum file ---
$checksumPath = Join-Path $PROJECT_ROOT $CHECKSUM_NAME
"$SHA256  $TARBALL_NAME" | Set-Content -Path $checksumPath -NoNewline
Write-Host "[OK] Checksum file updated: $CHECKSUM_NAME" -ForegroundColor Green

# --- Update manifest.json ---
$releasedAt = (Get-Date).ToUniversalTime().ToString("yyyy-MM-ddTHH:mm:ssZ")

# Extract changelog for this version
$changelogPath = Join-Path $PROJECT_ROOT "CHANGELOG.md"
$changelogContent = ""
if (Test-Path $changelogPath) {
    $inSection = $false
    Get-Content $changelogPath | ForEach-Object {
        if ($_ -match "^## v$VERSION") {
            $inSection = $true
            return
        }
        if ($inSection -and $_ -match "^## v[0-9]") {
            $inSection = $false
            return
        }
        if ($inSection) {
            $changelogContent += $_ + "`n"
        }
    }
}
$changelogContent = $changelogContent.Trim()

if (-not $changelogContent) {
    $changelogContent = "v$VERSION release"
}

# Read existing manifest to preserve versions array
$existingManifest = $null
$localManifestPath = Join-Path $PROJECT_ROOT "manifest.json"
if (Test-Path $localManifestPath) {
    try {
        $existingManifest = Get-Content $localManifestPath -Raw | ConvertFrom-Json
    } catch {
        Write-Host "[WARN] Could not parse existing manifest.json" -ForegroundColor Yellow
    }
}

# Also try to fetch from Supabase if local doesn't have versions
if (-not $existingManifest -or -not $existingManifest.versions) {
    try {
        $remoteManifestUrl = "$SUPABASE_URL/storage/v1/object/public/$BUCKET/manifest.json"
        $remoteResp = Invoke-RestMethod -Uri $remoteManifestUrl -Method GET -TimeoutSec 10
        if ($remoteResp -and $remoteResp.versions) {
            $existingManifest = $remoteResp
            Write-Host "[OK] Fetched existing manifest from Supabase ($($remoteResp.versions.Count) versions)" -ForegroundColor Green
        }
    } catch {
        Write-Host "[INFO] No remote manifest found, starting fresh" -ForegroundColor Yellow
    }
}

# Build the new version entry
$newEntry = @{
    version = "v$VERSION"
    released_at = $releasedAt
    release_notes = $changelogContent
    tarball = "releases/$TARBALL_NAME"
    sha256 = $SHA256
}

# Build versions array: prepend new version, keep last 5
$versionsList = @()
if ($existingManifest -and $existingManifest.versions) {
    foreach ($v in $existingManifest.versions) {
        # Skip if this version already exists (avoid duplicates)
        if ($v.version -ne "v$VERSION") {
            $versionsList += $v
        }
    }
}
# Prepend the new version at the top
$versionsList = @($newEntry) + $versionsList
# Keep only the last 5 versions
if ($versionsList.Count -gt 5) {
    $versionsList = $versionsList[0..4]
}

Write-Host "[OK] Versions in manifest: $($versionsList.Count)" -ForegroundColor Green

# Build manifest JSON with versions array
$manifest = @{
    version = "v$VERSION"
    released_at = $releasedAt
    release_notes = $changelogContent
    tarball = "releases/$TARBALL_NAME"
    sha256 = $SHA256
    versions = $versionsList
} | ConvertTo-Json -Depth 5

$manifestPath = Join-Path $PROJECT_ROOT "manifest.json"
[System.IO.File]::WriteAllText($manifestPath, $manifest, [System.Text.Encoding]::UTF8)
Write-Host "[OK] manifest.json updated" -ForegroundColor Green

# --- Upload to Supabase Storage ---
Write-Host ""
Write-Host "=========================================" -ForegroundColor Cyan
Write-Host "  Uploading to Supabase Storage" -ForegroundColor Cyan
Write-Host "=========================================" -ForegroundColor Cyan
Write-Host ""

# 1. Upload tarball
Write-Host "  [1/3] Uploading tarball..." -ForegroundColor Yellow
$tarballBytes = [System.IO.File]::ReadAllBytes($tarballPath)
$tarballUrl = "$SUPABASE_URL/storage/v1/object/$BUCKET/releases/$TARBALL_NAME"

try {
    $response = Invoke-RestMethod -Uri $tarballUrl `
        -Method POST `
        -Headers @{
            "Authorization" = "Bearer $SUPABASE_KEY"
            "Content-Type" = "application/gzip"
            "x-upsert" = "true"
        } `
        -Body $tarballBytes `
        -TimeoutSec 300
    Write-Host "  [OK] Tarball uploaded" -ForegroundColor Green
} catch {
    Write-Host "  [ERROR] Tarball upload failed: $_" -ForegroundColor Red
    exit 1
}

# 2. Upload checksum
Write-Host "  [2/3] Uploading checksum..." -ForegroundColor Yellow
$checksumBytes = [System.Text.Encoding]::UTF8.GetBytes("$SHA256  $TARBALL_NAME")
$checksumUrl = "$SUPABASE_URL/storage/v1/object/$BUCKET/releases/$CHECKSUM_NAME"

try {
    $response = Invoke-RestMethod -Uri $checksumUrl `
        -Method POST `
        -Headers @{
            "Authorization" = "Bearer $SUPABASE_KEY"
            "Content-Type" = "text/plain"
            "x-upsert" = "true"
        } `
        -Body $checksumBytes `
        -TimeoutSec 30
    Write-Host "  [OK] Checksum uploaded" -ForegroundColor Green
} catch {
    Write-Host "  [ERROR] Checksum upload failed: $_" -ForegroundColor Red
    exit 1
}

# 3. Upload manifest.json (LAST — this signals the release is live)
Write-Host "  [3/3] Uploading manifest.json..." -ForegroundColor Yellow
$manifestBytes = [System.Text.Encoding]::UTF8.GetBytes($manifest)
$manifestUrl = "$SUPABASE_URL/storage/v1/object/$BUCKET/manifest.json"

try {
    $response = Invoke-RestMethod -Uri $manifestUrl `
        -Method POST `
        -Headers @{
            "Authorization" = "Bearer $SUPABASE_KEY"
            "Content-Type" = "application/json"
            "x-upsert" = "true"
            "Cache-Control" = "no-cache"
        } `
        -Body $manifestBytes `
        -TimeoutSec 30
    Write-Host "  [OK] Manifest uploaded" -ForegroundColor Green
} catch {
    Write-Host "  [ERROR] Manifest upload failed: $_" -ForegroundColor Red
    exit 1
}

# --- Verify ---
Write-Host ""
Write-Host "=========================================" -ForegroundColor Cyan
Write-Host "  Verifying" -ForegroundColor Cyan
Write-Host "=========================================" -ForegroundColor Cyan
Write-Host ""

try {
    $verifyResponse = Invoke-WebRequest -Uri "$SUPABASE_URL/storage/v1/object/public/$BUCKET/manifest.json" -Method GET -TimeoutSec 10
    if ($verifyResponse.StatusCode -eq 200) {
        Write-Host "  [OK] manifest.json is accessible (HTTP 200)" -ForegroundColor Green
    } else {
        Write-Host "  [WARN] manifest.json returned HTTP $($verifyResponse.StatusCode)" -ForegroundColor Yellow
    }
} catch {
    Write-Host "  [WARN] Could not verify manifest: $_" -ForegroundColor Yellow
}

# --- Done ---
Write-Host ""
Write-Host "=========================================" -ForegroundColor Green
Write-Host "  AirCoins v$VERSION published!" -ForegroundColor Green
Write-Host "=========================================" -ForegroundColor Green
Write-Host ""
Write-Host "  Version:  v$VERSION"
Write-Host "  SHA256:   $SHA256"
Write-Host "  Tarball:  $SUPABASE_URL/storage/v1/object/public/$BUCKET/releases/$TARBALL_NAME"
Write-Host "  Manifest: $SUPABASE_URL/storage/v1/object/public/$BUCKET/manifest.json"
Write-Host ""
Write-Host "  Devices will detect the update on next check (or force refresh)." -ForegroundColor Yellow
Write-Host ""




