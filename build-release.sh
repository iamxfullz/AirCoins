#!/usr/bin/env bash
set -euo pipefail

# AirCoins Release Build Script
# Cross-compiles the Go API for linux/arm/7 and assembles the release tarball.
# Usage: bash build-release.sh [VERSION]

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
VERSION="${1:-}"

# Parse version from install.sh if not provided
if [ -z "$VERSION" ]; then
    VERSION=$(grep -m1 '^VERSION=' "$SCRIPT_DIR/install.sh" | cut -d'"' -f2)
fi

if [ -z "$VERSION" ]; then
    echo "ERROR: Could not determine version. Pass as argument or ensure install.sh has VERSION=..."
    exit 1
fi

TARBALL_NAME="aircoins-v${VERSION}"
GO_SRC_DIR="$SCRIPT_DIR/system/usr/local/bin/aircoins-api"

echo "========================================="
echo "  AirCoins Release Builder v${VERSION}"
echo "========================================="
echo ""

# --- Pre-flight checks ---
if ! command -v go &>/dev/null; then
    echo "ERROR: Go toolchain not found. Install Go 1.21+ first."
    exit 1
fi

GO_VERSION=$(go version | grep -oP 'go\K[0-9]+\.[0-9]+')
echo "Go version: $GO_VERSION"

# --- Cross-compile ---
echo ""
echo "[1/4] Cross-compiling aircoins-api for linux/arm/7 and linux/amd64..."
cd "$GO_SRC_DIR"
CGO_ENABLED=0 GOOS=linux GOARCH=arm GOARM=7 go build \
    -ldflags "-s -w -X main.Version=v${VERSION}" \
    -o aircoins-api .
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build \
    -ldflags "-s -w -X main.Version=v${VERSION}" \
    -o aircoins-api-x64 .
cd "$SCRIPT_DIR"

# Validate binaries
if ! file "$GO_SRC_DIR/aircoins-api" | grep -q "ELF"; then
    echo "ERROR: ARM binary is not a valid ELF executable."
    rm -f "$GO_SRC_DIR/aircoins-api" "$GO_SRC_DIR/aircoins-api-x64"
    exit 1
fi
if ! file "$GO_SRC_DIR/aircoins-api-x64" | grep -q "ELF"; then
    echo "ERROR: x64 binary is not a valid ELF executable."
    rm -f "$GO_SRC_DIR/aircoins-api" "$GO_SRC_DIR/aircoins-api-x64"
    exit 1
fi

BINARY_SIZE=$(du -h "$GO_SRC_DIR/aircoins-api" | cut -f1)
X64_SIZE=$(du -h "$GO_SRC_DIR/aircoins-api-x64" | cut -f1)
echo "  ✓ ARM binary compiled: ${BINARY_SIZE} (ELF ARM)"
echo "  ✓ x64 binary compiled: ${X64_SIZE} (ELF x86-64, no GPIO — Sub-Vendo coinslots only)"

# --- Assemble tarball ---
echo ""
echo "[2/4] Assembling release tarball..."

STAGING_DIR=$(mktemp -d)
TARBALL_DIR="$STAGING_DIR/$TARBALL_NAME"
mkdir -p "$TARBALL_DIR/system"

# Copy root-level files
for f in install.sh aircoins-recover.sh .env.example index.html admin.html DEPLOYMENT.md CHANGELOG.md VERSION; do
    if [ -f "$SCRIPT_DIR/$f" ]; then
        cp "$SCRIPT_DIR/$f" "$TARBALL_DIR/"
    else
        echo "  WARNING: $f not found, skipping"
    fi
done

# (NodeMCU firmware removed together with Sub-Vendo support — no firmware/
# directory exists anymore and nothing in the release references it.)

# Copy system/ directory (excluding .env, .git, dev artifacts)
if command -v rsync &>/dev/null; then
    rsync -a \
        --exclude='.env' \
        --exclude='__diff_tmp.txt' \
        --exclude='.git' \
        --exclude='.gitattributes' \
        --exclude='.gitignore' \
        "$SCRIPT_DIR/system/" "$TARBALL_DIR/system/"
else
    # Copy CONTENTS of system/ into the staging area.
    # Using (cd src && cp -r . dst) to avoid the system/system/ nesting
    # that cp -r src/ dst/ can produce depending on the cp implementation.
    mkdir -p "$TARBALL_DIR/system"
    (cd "$SCRIPT_DIR/system" && tar cf - .) | (cd "$TARBALL_DIR/system" && tar xf -)
    rm -rf "$TARBALL_DIR/system/.env" \
           "$TARBALL_DIR/system/__diff_tmp.txt" \
           "$TARBALL_DIR/system/.git" \
           "$TARBALL_DIR/system/.gitattributes" \
           "$TARBALL_DIR/system/.gitignore"
fi

# --- Security check ---
echo ""
echo "[3/4] Running security checks..."

# Check staging directory for .env files (excluding .env.example)
if find "$TARBALL_DIR" -name '.env' -not -name '.env.example' 2>/dev/null | grep -q .; then
    echo "ERROR: .env file found in staging directory! Aborting."
    rm -rf "$STAGING_DIR"
    rm -f "$GO_SRC_DIR/aircoins-api"
    exit 1
fi
echo "  ✓ No secrets detected"

# --- Line-ending normalization ---
# Any CRLF that slips in from Windows editing breaks Linux scripts at
# runtime: the shebang becomes "#!/bin/bash\r" ("cannot execute: required
# file not found") and "\r: command not found" errors appear in daemons.
# Strip CR from packaged Linux scripts/CGI — but NEVER from the compiled
# binaries: sed on an ELF corrupts it (it strips \r bytes and the section
# headers no longer match), which bricks the API on devices that update.
echo ""
echo "[3b/4] Normalizing line endings (strip CR from Linux scripts)..."
find "$TARBALL_DIR/system/usr/local/bin" \
     "$TARBALL_DIR/system/usr/lib/cgi-bin" \
     -type f ! -name 'aircoins-api*' \
     -exec sed -i 's/\r$//' {} + 2>/dev/null || true
for _lf in "$TARBALL_DIR/install.sh" \
           "$TARBALL_DIR/aircoins-recover.sh" \
           "$TARBALL_DIR/index.html" \
           "$TARBALL_DIR/admin.html" \
           "$TARBALL_DIR/DEPLOYMENT.md" \
           "$TARBALL_DIR/CHANGELOG.md"; do
    [ -f "$_lf" ] && sed -i 's/\r$//' "$_lf"
done
echo "  ✓ Line endings normalized (LF)"

# Verify the packaged binaries survived staging untouched. A sed-style pass
# must never transform compiled binaries: a corrupted ELF bricks every
# device that installs the OTA. Compare byte-for-byte against the freshly
# built copies and abort the build on ANY mismatch.
if ! cmp -s "$GO_SRC_DIR/aircoins-api" "$TARBALL_DIR/system/usr/local/bin/aircoins-api/aircoins-api"; then
    echo "ERROR: packaged ARM binary does not match the freshly built one — aborting."
    rm -rf "$STAGING_DIR"
    exit 1
fi
if ! cmp -s "$GO_SRC_DIR/aircoins-api-x64" "$TARBALL_DIR/system/usr/local/bin/aircoins-api/aircoins-api-x64"; then
    echo "ERROR: packaged x64 binary does not match the freshly built one — aborting."
    rm -rf "$STAGING_DIR"
    exit 1
fi
echo "  ✓ Binary integrity verified (ARM + x64 byte-identical to build)"

# --- Create tarball + checksum ---
echo ""
echo "[4/4] Creating tarball and checksum..."
OUTPUT_DIR="$SCRIPT_DIR"
TARBALL_PATH="$OUTPUT_DIR/${TARBALL_NAME}.tar.gz"
CHECKSUM_PATH="$OUTPUT_DIR/${TARBALL_NAME}.sha256"

tar czf "$TARBALL_PATH" -C "$STAGING_DIR" "$TARBALL_NAME"
sha256sum "$TARBALL_PATH" > "$CHECKSUM_PATH"

# Clean up
rm -rf "$STAGING_DIR"
rm -f "$GO_SRC_DIR/aircoins-api" "$GO_SRC_DIR/aircoins-api-x64"

TARBALL_SIZE=$(du -h "$TARBALL_PATH" | cut -f1)
FILE_COUNT=$(tar tzf "$TARBALL_PATH" | wc -l)

echo ""
echo "========================================="
echo "  Release build complete!"
echo "========================================="
echo ""
echo "  Tarball:  $TARBALL_PATH"
echo "  Size:     $TARBALL_SIZE"
echo "  Files:    $FILE_COUNT"
echo "  Checksum: $CHECKSUM_PATH"
echo ""
echo "  SHA256: $(cat "$CHECKSUM_PATH" | cut -d' ' -f1)"
echo ""
