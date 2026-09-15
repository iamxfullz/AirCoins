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

# Verify tarball exists, or build it
if [ ! -f "$TARBALL" ]; then
    echo "Tarball $TARBALL not found. Building with build-release.sh..."
    bash build-release.sh "$VERSION"
    if [ ! -f "$TARBALL" ]; then
        echo "ERROR: Failed to build $TARBALL"
        exit 1
    fi
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

if command -v python3 &>/dev/null; then
    python3 -c "
import json, urllib.request, sys

version = 'v${VERSION}'
released_at = '${RELEASED_AT}'
tarball = 'releases/${TARBALL}'
sha256 = '${SHA256}'

with open('${CHANGELOG_FILE}', 'r', encoding='utf-8') as f:
    release_notes = f.read().strip()

new_entry = {
    'version': version,
    'released_at': released_at,
    'release_notes': release_notes,
    'tarball': tarball,
    'sha256': sha256
}

existing_versions = []
try:
    url = '${SUPABASE_URL}/storage/v1/object/public/${BUCKET}/manifest.json'
    req = urllib.request.Request(url, headers={'Cache-Control': 'no-cache', 'User-Agent': 'AirCoins-Publisher'})
    with urllib.request.urlopen(req, timeout=10) as resp:
        data = json.loads(resp.read().decode('utf-8'))
        existing_versions = data.get('versions', [])
except Exception:
    pass

versions = [new_entry] + [v for v in existing_versions if v.get('version') != version][:4]

manifest = {
    'version': version,
    'released_at': released_at,
    'release_notes': release_notes,
    'tarball': tarball,
    'sha256': sha256,
    'versions': versions
}

with open('manifest-tmp.json', 'w', encoding='utf-8') as f:
    json.dump(manifest, f, indent=2)
"
else
    # Bash-only fallback: escape backslashes, double quotes, and newlines for valid JSON
    RELEASE_NOTES=$(sed -e 's/\\/\\\\/g' -e 's/"/\\"/g' -e ':a' -e 'N' -e '$!ba' -e 's/\n/\\n/g' "${CHANGELOG_FILE}")
    cat > manifest-tmp.json << EOF
{
    "version": "v${VERSION}",
    "released_at": "${RELEASED_AT}",
    "release_notes": "${RELEASE_NOTES}",
    "tarball": "releases/${TARBALL}",
    "sha256": "${SHA256}",
    "versions": [
        {
            "version": "v${VERSION}",
            "released_at": "${RELEASED_AT}",
            "release_notes": "${RELEASE_NOTES}",
            "tarball": "releases/${TARBALL}",
            "sha256": "${SHA256}"
        }
    ]
}
EOF
fi

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
