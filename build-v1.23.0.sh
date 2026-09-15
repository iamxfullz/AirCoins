#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")"

VER="1.23.0"
NAME="aircoins-v${VER}"

STAGING=$(mktemp -d)
TBDIR="${STAGING}/${NAME}"
mkdir -p "${TBDIR}/system"

# Root files
for f in install.sh aircoins-recover.sh .env.example index.html admin.html DEPLOYMENT.md CHANGELOG.md; do
  if [ -f "$f" ]; then
    cp "$f" "${TBDIR}/"
  else
    echo "WARN: $f missing, skipping"
  fi
done

# Firmware
if [ -d firmware ]; then
  mkdir -p "${TBDIR}/firmware"
  (cd firmware && tar cf - .) | (cd "${TBDIR}/firmware" && tar xf -)
  echo "  firmware included"
fi

# System directory (exclude secrets)
mkdir -p "${TBDIR}/system"
(cd system && tar cf - --exclude='.env' --exclude='__diff_tmp.txt' --exclude='.git' --exclude='.gitattributes' --exclude='.gitignore' .) | (cd "${TBDIR}/system" && tar xf -)

# Tarball + checksum
TARBALL="${NAME}.tar.gz"
CHECKSUM="${NAME}.sha256"
tar czf "${TARBALL}" -C "${STAGING}" "${NAME}"
sha256sum "${TARBALL}" > "${CHECKSUM}"
rm -rf "${STAGING}"

SIZE=$(du -h "${TARBALL}" | cut -f1)
CNT=$(tar tzf "${TARBALL}" | wc -l)
echo "OK: ${TARBALL} (${SIZE}, ${CNT} files)"
echo "SHA: $(cat "${CHECKSUM}")"
