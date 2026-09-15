#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")"

FILE="system/usr/local/bin/aircoins-api/handlers/coinslot.go"

echo "=== Current Options handler ==="
sed -n '281,321p' "$FILE"

echo ""
echo "=== Current session Start handler ==="
grep -n 'func.*Start' system/usr/local/bin/aircoins-api/handlers/session.go | head -5

echo ""
echo "=== Does Start read coinslot? ==="
grep -n 'coinslot\|Coinslot' system/usr/local/bin/aircoins-api/handlers/session.go || echo "NO coinslot in session.go"
