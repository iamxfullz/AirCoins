#!/bin/bash
# =====================================================================
# AirCoins Emergency Recovery Script
# ---------------------------------------------------------------------
# Purpose: Restore SSH / admin access when VLAN changes broke the
#          main network interface.
#
# Usage (run on the Orange Pi console as root):
#     sudo bash aircoins-recover.sh
#
# Optional: override the main interface / IP if yours differ:
#     sudo IFACE=eth0 MAIN_IP=192.168.1.100/24 bash aircoins-recover.sh
# =====================================================================

set -u

IFACE="${IFACE:-}"
MAIN_IP="${MAIN_IP:-192.168.254.100/24}"

echo "==================================================="
echo " AirCoins Emergency Recovery"
echo "==================================================="
echo ""

if [ "$(id -u)" -ne 0 ]; then
    echo "ERROR: This script must be run as root."
    echo "       Try: sudo bash $0"
    exit 1
fi

# ---------------------------------------------------------------------
# 1. Detect the main physical interface (skip loopback + VLANs)
# ---------------------------------------------------------------------
if [ -z "$IFACE" ]; then
    echo "[1/8] Detecting main network interface..."
    for candidate in /sys/class/net/*; do
        name=$(basename "$candidate")
        # Skip loopback, VLAN sub-interfaces, virtual/bridge devices
        case "$name" in
            lo|*.*|docker*|veth*|br-*) continue ;;
        esac
        # Must be a real device (has a device symlink)
        if [ -e "$candidate/device" ]; then
            IFACE="$name"
            break
        fi
    done
fi

if [ -z "$IFACE" ]; then
    echo "      WARNING: Could not auto-detect interface. Defaulting to end0."
    IFACE="end0"
fi
echo "      Main interface: $IFACE"
echo ""

# ---------------------------------------------------------------------
# 2. Stop and disable all per-VLAN DHCP services
# ---------------------------------------------------------------------
echo "[2/8] Stopping all VLAN DHCP services..."
for unit in $(systemctl list-units --all --plain --no-legend 'dnsmasq@*' 2>/dev/null | awk '{print $1}'); do
    echo "      Stopping $unit"
    systemctl stop "$unit" 2>/dev/null
    systemctl disable "$unit" 2>/dev/null
done
systemctl stop dnsmasq 2>/dev/null
systemctl disable dnsmasq 2>/dev/null
echo "      Done."
echo ""

# ---------------------------------------------------------------------
# 3. Clean up bridge interfaces
# ---------------------------------------------------------------------
echo "[3/8] Cleaning up bridge interfaces..."
for iface in /sys/class/net/br*; do
    [ -e "$iface" ] || continue
    bridge_name=$(basename "$iface")
    echo "      Removing bridge $bridge_name"
    ip link set "$bridge_name" down 2>/dev/null
    ip link delete "$bridge_name" type bridge 2>/dev/null
done
echo "      Done."
echo ""

# ---------------------------------------------------------------------
# 4. Delete every VLAN sub-interface
# ---------------------------------------------------------------------
echo "[4/8] Removing all VLAN interfaces..."
for vlan in $(ip -o link show type vlan 2>/dev/null | awk -F': ' '{print $2}' | cut -d'@' -f1); do
    echo "      Deleting $vlan"
    ip link delete "$vlan" 2>/dev/null
done
echo "      Done."
echo ""

# ---------------------------------------------------------------------
# 5. Disable VLAN auto-restore on boot and clear its config
# ---------------------------------------------------------------------
echo "[5/8] Disabling VLAN boot restore..."
systemctl stop aircoins-vlans.service 2>/dev/null
systemctl disable aircoins-vlans.service 2>/dev/null

if [ -f /etc/pisowifi/vlans.conf ]; then
    cp /etc/pisowifi/vlans.conf "/etc/pisowifi/vlans.conf.bak.$(date +%s)" 2>/dev/null
    echo "      Backed up existing vlans.conf"
fi
rm -f /etc/pisowifi/vlans.conf 2>/dev/null

if [ -f /etc/pisowifi/bridges.conf ]; then
    cp /etc/pisowifi/bridges.conf "/etc/pisowifi/bridges.conf.bak.$(date +%s)" 2>/dev/null
    echo "      Backed up existing bridges.conf"
fi
rm -f /etc/pisowifi/bridges.conf 2>/dev/null

rm -f /etc/dnsmasq.d/*.conf 2>/dev/null
echo "      Done."
echo ""

# ---------------------------------------------------------------------
# 6. Restore the main interface IP
# ---------------------------------------------------------------------
echo "[6/8] Restoring main interface $IFACE ..."
ip link set "$IFACE" up 2>/dev/null

if ip addr show "$IFACE" 2>/dev/null | grep -q "inet ${MAIN_IP%/*}"; then
    echo "      $IFACE already has ${MAIN_IP}"
else
    ip addr add "$MAIN_IP" dev "$IFACE" 2>/dev/null
    echo "      Assigned $MAIN_IP to $IFACE"
fi

# Try DHCP as well, in case the LAN provides an address
if command -v dhclient >/dev/null 2>&1; then
    timeout 15 dhclient -1 "$IFACE" >/dev/null 2>&1 &
fi
echo "      Done."
echo ""

# ---------------------------------------------------------------------
# 7. Restart core services
# ---------------------------------------------------------------------
echo "[7/8] Restarting core services..."
systemctl daemon-reload 2>/dev/null
for svc in ssh sshd lighttpd postgresql aircoins-api; do
    if systemctl list-unit-files --plain --no-legend 2>/dev/null | grep -q "^${svc}\.service"; then
        systemctl enable "$svc" >/dev/null 2>&1
        systemctl restart "$svc" 2>/dev/null && echo "      Restarted $svc" || echo "      Could not restart $svc"
    fi
done
echo ""

# ---------------------------------------------------------------------
# 8. Report final state
# ---------------------------------------------------------------------
echo "[8/8] Current network state:"
echo "---------------------------------------------------"
ip -4 addr show 2>/dev/null | grep -E "^[0-9]+:|inet " | sed 's/^/      /'
echo "---------------------------------------------------"
echo ""
echo "Remaining VLAN interfaces (should be empty):"
ip -o link show type vlan 2>/dev/null | sed 's/^/      /' || echo "      none"
echo ""
echo "Service status:"
for svc in ssh sshd lighttpd aircoins-api; do
    if systemctl list-unit-files --plain --no-legend 2>/dev/null | grep -q "^${svc}\.service"; then
        state=$(systemctl is-active "$svc" 2>/dev/null)
        printf "      %-16s %s\n" "$svc" "$state"
    fi
done
echo ""
echo "SSH listening ports:"
if command -v ss >/dev/null 2>&1; then
    ss -tlnp 2>/dev/null | grep -E ':22\b' | sed 's/^/      /' || echo "      WARNING: SSH not listening on port 22"
fi
echo ""
echo "==================================================="
echo " Recovery complete."
echo ""
echo " Try connecting from your PC:"
echo "     ssh <user>@${MAIN_IP%/*}"
echo "     http://${MAIN_IP%/*}/admin.html"
echo "==================================================="
