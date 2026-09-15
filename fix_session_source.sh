#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")"

SG="system/usr/local/bin/aircoins-api/handlers/session.go"

# Replace lines 359-367 (the source resolution block) with the coinslot-aware version
# First delete lines 359-367, then insert the new block

sed -i '359,367d' "$SG"

# Now insert the new block at line 359
sed -i '358a\
\
	// Resolve which coinslot owns the caller'\''s VLAN. The optional coinslot\
	// param (from the portal'\''s vendo picker) lets the user override "auto"\
	// and force GPIO or a specific sub-vendo. Server-side enforcement of\
	// VLAN isolation is preserved: a subvendo:N selection is only honored\
	// if N is the unit bound to THIS caller'\''s VLAN.\
	source := "local_gpio"\
	iface := resolveVLAN(clientIP)\
	svID, _ := subvendoIDForVLAN(h.DB, iface)\
\
	switch {\
	case req.Coinslot == "gpio":\
		source = "local_gpio"\
	case strings.HasPrefix(req.Coinslot, "subvendo:"):\
		id, _ := strconv.ParseInt(strings.TrimPrefix(req.Coinslot, "subvendo:"), 10, 64)\
		if id == svID && svID > 0 {\
			source = "subvendo:" + strconv.FormatInt(id, 10)\
		}\
	default:\
		// "auto", empty, or unrecognized — prefer sub-vendo on VLAN\
		if iface != "" && svID > 0 {\
			source = "subvendo:" + strconv.FormatInt(svID, 10)\
		}\
	}' "$SG"

echo "=== Verifying session.go source block ==="
sed -n '355,385p' "$SG"
