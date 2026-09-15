#!/usr/bin/env bash
cd "$(dirname "$0")"
echo "=== session.go 355-370 ==="
sed -n '355,370p' system/usr/local/bin/aircoins-api/handlers/session.go
