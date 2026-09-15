#!/usr/bin/env bash
cd "$(dirname "$0")"
FILE="system/usr/local/bin/aircoins-api/handlers/coinslot.go"
echo "=== Handler line numbers ==="
grep -n 'func (h \*CoinslotHandler)' "$FILE" || true
echo ""
echo "=== Status handler (330-420) ==="
sed -n '330,420p' "$FILE"
echo ""
echo "=== Disarm handler (420-470) ==="
sed -n '420,470p' "$FILE"
