#!/usr/bin/env bash
cd "$(dirname "$0")"
echo "=== Session Start (290-470) ==="
sed -n '290,470p' system/usr/local/bin/aircoins-api/handlers/session.go
