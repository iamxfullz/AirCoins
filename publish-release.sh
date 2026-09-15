#!/bin/bash
set -euo pipefail

# Usage: ./publish-release.sh <version>
# Example: ./publish-release.sh 1.8.0

VERSION="${1:-}"
if [ -z "$VERSION" ]; then
    echo "Usage: $0 <version>"
    echo "Example: $0 1.8.0"
    exit 1
fi

# Read Supabase credentials from .env or environment
if [ -f ".env" ]; then
    source .env
fi

SUPABASE_URL="${SUPABASE_URL:-}"
SUPABASE_KEY="${SUPABASE_SERVICE_ROLE_KEY:-}"
BUCKET="aircoins"

if [ -z "$SUPABASE_URL" ] || [ -z "$SUPABASE_KEY" ]; then
    echo "ERROR: SUPABASE_URL and SUPABASE_SERVICE_ROLE_KEY must be set"
    exit 1
fi

TARBALL="aircoins-v${VERSION}.tar.gz"
CHECKSUM="aircoins-v${VERSION}.sha256"

# Verify tarball exists
if [ ! -f "$TARBALL" ]; then
    echo "ERROR: $TARBALL not found in current directory"
    exit 1
fi

echo "=== Publishing AirCoins v${VERSION} to Supabase Storage ==="

# 1. Upload tarball
echo "  Uploading tarball..."
curl -s -X POST "${SUPABASE_URL}/storage/v1/object/${BUCKET}/releases/${TARBALL}" \
    -H "Authorization: Bearer ${SUPABASE_KEY}" \
    -H "Content-Type: application/gzip" \
    -H "x-upsert: true" \
    --data-binary "@${TARBALL}"
echo "  ✓ Tarball uploaded"

# 2. Upload checksum
echo "  Uploading checksum..."
curl -s -X POST "${SUPABASE_URL}/storage/v1/object/${BUCKET}/releases/${CHECKSUM}" \
    -H "Authorization: Bearer ${SUPABASE_KEY}" \
    -H "Content-Type: text/plain" \
    -H "x-upsert: true" \
    --data-binary "@${CHECKSUM}"
echo "  ✓ Checksum uploaded"

# 3. Generate changelog excerpt and upload
echo "  Generating changelog..."
CHANGELOG_FILE="changelog-v${VERSION}.txt"
# Extract the section for this version from CHANGELOG.md
# Find lines between "## v{VERSION}" and the next "## v" or "---"
sed -n "/## v${VERSION}/,/## v[0-9]/p" CHANGELOG.md | head -n -1 > "${CHANGELOG_FILE}" 2>/dev/null || echo "v${VERSION} release notes" > "${CHANGELOG_FILE}"

curl -s -X POST "${SUPABASE_URL}/storage/v1/object/${BUCKET}/changelogs/${CHANGELOG_FILE}" \
    -H "Authorization: Bearer ${SUPABASE_KEY}" \
    -H "Content-Type: text/plain" \
    -H "x-upsert: true" \
    --data-binary "@${CHANGELOG_FILE}"
echo "  ✓ Changelog uploaded"

# 4. Generate and upload manifest.json (LAST — this is the signal that the release is live)
echo "  Generating manifest..."
SHA256=$(cat "${CHECKSUM}" | awk '{print $1}')
RELEASED_AT=$(date -u +"%Y-%m-%dT%H:%M:%SZ")

# JSON-escape the changelog content: try python3 first, fall back to bash-only escaping
if command -v python3 &>/dev/null; then
    RELEASE_NOTES=$(python3 -c "import sys,json; print(json.dumps(sys.stdin.read()))" < "${CHANGELOG_FILE}")
else
    # Bash-only fallback: escape backslashes, double quotes, and newlines for valid JSON
    RELEASE_NOTES=$(sed -e 's/\\/\\\\/g' -e 's/"/\\"/g' -e ':a' -e 'N' -e '$!ba' -e 's/\n/\\n/g' "${CHANGELOG_FILE}")
    RELEASE_NOTES="\"${RELEASE_NOTES}\""
fi

# Create manifest.json
cat > manifest-tmp.json << EOF
{
    "version": "v${VERSION}",
    "released_at": "${RELEASED_AT}",
    "release_notes": ${RELEASE_NOTES},
    "tarball": "releases/${TARBALL}",
    "sha256": "${SHA256}"
}
EOF

curl -s -X POST "${SUPABASE_URL}/storage/v1/object/${BUCKET}/manifest.json" \
    -H "Authorization: Bearer ${SUPABASE_KEY}" \
    -H "Content-Type: application/json" \
    -H "x-upsert: true" \
    -H "Cache-Control: no-cache" \
    --data-binary "@manifest-tmp.json"
rm -f manifest-tmp.json
echo "  ✓ Manifest uploaded"

# 5. Clean up temp files
rm -f "${CHANGELOG_FILE}"

# 6. Verify
echo ""
echo "=== Verifying ==="
HTTP_CODE=$(curl -s -o /dev/null -w "%{http_code}" "${SUPABASE_URL}/storage/v1/object/public/${BUCKET}/manifest.json")
if [ "$HTTP_CODE" = "200" ]; then
    echo "  ✓ manifest.json is accessible (HTTP ${HTTP_CODE})"
else
    echo "  ✗ manifest.json verification failed (HTTP ${HTTP_CODE})"
    exit 1
fi

echo ""
echo "=== AirCoins v${VERSION} published successfully! ==="
echo "  Manifest: ${SUPABASE_URL}/storage/v1/object/public/${BUCKET}/manifest.json"
echo "  Tarball:  ${SUPABASE_URL}/storage/v1/object/public/${BUCKET}/releases/${TARBALL}"
