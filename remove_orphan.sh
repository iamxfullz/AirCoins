#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")"

SG="system/usr/local/bin/aircoins-api/handlers/session.go"

# Show the area around the orphaned brace
echo "=== Lines 380-390 ==="
sed -n '380,390p' "$SG"

# Count total lines
echo ""
echo "Total lines: $(wc -l < "$SG")"
