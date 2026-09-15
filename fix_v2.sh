#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")"

CG="system/usr/local/bin/aircoins-api/handlers/coinslot.go"
SG="system/usr/local/bin/aircoins-api/handlers/session.go"

##############################################################################
# FIX 1: coinslot.go Options handler
##############################################################################
echo "=== Fixing Options handler ==="

# Show exact lines around the broken block so we can target by line number
grep -n 'var status string\|var coinVal int\|COALESCE(status\|COALESCE(coin_value\|Scan(&name, &status\|"coin_value": coinVal' "$CG"

# Use sed to:
# 1. Delete "var status string" and "var coinVal int" lines
# 2. Replace with "var lastSeen sql.NullTime"
# 3. Fix the SELECT
# 4. Fix the Scan
# 5. Replace the resp["subvendo"] block

# Step 1: delete the two var lines, replace first with lastSeen
sed -i '/var status string/c\		var lastSeen sql.NullTime' "$CG"
sed -i '/var coinVal int/d' "$CG"

# Step 2: fix SELECT
sed -i "s/SELECT name, COALESCE(status,''), COALESCE(coin_value,1)/SELECT name, last_seen/" "$CG"

# Step 3: fix Scan
sed -i 's/.Scan(&name, &status, &coinVal)/.Scan(\&name, \&lastSeen)/' "$CG"

# Step 4: replace the resp["subvendo"] block — need to add status computation
# The current block is:
#     resp["subvendo"] = map[string]interface{}{
#         "id":         svID,
#         "name":       name,
#         "status":     status,
#         "coin_value": coinVal,
#     }
# We need to insert status computation BEFORE it and fix the fields

# Insert status computation before the resp["subvendo"] line
sed -i '/resp\["subvendo"\] = map\[string\]interface{}{/i\\t\t\tstatus := "offline"\n\t\t\tif lastSeen.Valid \&\& time.Since(lastSeen.Time) < 2*time.Minute {\n\t\t\t\tstatus = "online"\n\t\t\t}' "$CG"

# Fix the status and coin_value lines inside the map
sed -i 's/"status":     status,/"status":     status,/' "$CG"
sed -i 's/"coin_value": coinVal,/"coin_value": 1, \/\/ NodeMCU: 1 pulse = 1 peso/' "$CG"

echo "=== Verifying coinslot.go Options ==="
sed -n '290,320p' "$CG"

##############################################################################
# FIX 2: session.go Start handler
##############################################################################
echo ""
echo "=== Fixing session.go Start handler ==="

# 2a: Add Coinslot to request struct
if grep -q 'Coinslot' "$SG"; then
  echo "  Coinslot field already present"
else
  sed -i '/SessionToken string \`json:"session_token"\`/a\\t\tCoinslot     string \`json:"coinslot"\`' "$SG"
  echo "  Added Coinslot field"
fi

# 2b: Replace source resolution — use line-based approach
# Find the line numbers
echo "  Locating source resolution block..."
grep -n 'source := "local_gpio"' "$SG"
grep -n 'Resolve which coinslot owns' "$SG"

echo ""
echo "DONE — verify output above"
