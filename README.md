# AirCoins PisoNet

**Turn any Orange Pi into a coin-operated WiFi hotspot.**

AirCoins is a complete pay-per-use WiFi solution in a single system image. Drop it on a MicroSD card, power on, and start earning. No coding. No complex setup. Just plug and play.

---

## Download

> **[Download AirCoins System Image](https://drive.google.com/file/d/1CX2qv3Ecuf7YAQFG4x7xitP2uARuEvv3/view?usp=drive_link)**

---

## Why AirCoins?

Run a fully automated coin-operated WiFi business on hardware that costs less than a meal. AirCoins handles everything — from the moment a customer inserts a coin to the second their session expires.

---

## Features

### Coin-Powered Sessions
Connect any standard coin acceptor to the Orange Pi GPIO pins. AirCoins detects every pulse, tracks credits in real time, and instantly grants WiFi access — no app, no login, no friction for your customers.

### Captive Portal That Just Works
Automatic detection for **iOS, Android, Windows, macOS, and Linux**. The moment a customer connects, their device pops up your branded splash page. No manual configuration needed on their end.

### Fully Customizable Portal
Make it yours. Choose from built-in themes, pick your own color scheme (7 independent colors), upload a custom background image, and add personalized audio feedback for coin insert, coin drop, and payment complete events.

### Multi-VLAN Network Segmentation
Run multiple isolated networks on a single device. Each VLAN gets its own DHCP server, captive portal, splash page, and traffic shaping rules — perfect for separating premium and standard tiers.

### Network Bridges
Create Layer-2 bridges from the admin panel. Combine VLAN interfaces as bridge members and provision portals on bridge interfaces for flexible network topologies.

### Per-Session Traffic Shaping
Set upload and download speed limits per session and per VLAN. Give premium customers more bandwidth, keep standard users fair — all powered by Linux `tc/htb` under the hood.

### Smart Session Management
Time-based sessions with automatic expiry, idle timeout detection, extend/renew support, and real-time bandwidth tracking. Every session is tied to a MAC address and enforced through iptables.

### Admin Dashboard
A single-page command center. Monitor active sessions in real time, adjust pricing on the fly, manage GPIO pins, configure VLANs, view system stats, and control every aspect of your hotspot — all from one clean interface.

### Earnings & Reports
Track your revenue with aggregated earnings summaries. View total earnings, coin counts, and session counts over any date range. All data is stored in PostgreSQL for reliable historical analysis.

### Over-the-Air Updates
Stay current without touching the device. The built-in updater downloads new releases, verifies SHA256 integrity, and installs — all from the admin panel with a progress bar and self-recovery if anything goes wrong.

### ZeroTier Remote Management
Manage your hotspot from anywhere in the world. Join ZeroTier networks directly from the admin panel for secure, encrypted remote access — no port forwarding, no public IP, no hassle.

### WAN Auto-Detection
The system automatically detects your upstream internet interface and configures itself. Switch between DHCP and static modes, manage VLAN-based WAN tagging — all without editing config files.

### License System with Free Trial
Every device starts with a **7-day free trial** — no account, no configuration. After that, cloud-backed licensing keeps your deployment secure with hardware-ID binding, 24-hour heartbeat verification, and automatic lockdown for invalid licenses.

### Emergency Recovery
One script restores your network if anything goes wrong. `aircoins-recover.sh` strips all VLANs and brings back the main interface — your hotspot is never down for long.

### Remote Reboot
Restart the device from the admin panel with a single click. Includes an auto-reconnect countdown so you're never locked out.

### Change Admin Password
Update your admin credentials directly from the Settings page. Keep your hotspot secure with a strong password.

### Permanent Device Tokens
Sessions persist across reboots with soft-delete and cookie-based token backup. Your customers stay connected even after a service restart.

---

## Supported Hardware

| Board | RAM | CPU | Status |
|-------|-----|-----|--------|
| Orange Pi PC | 1GB | Allwinner H3 (Cortex-A7) | Supported |
| Orange Pi One | 512MB | Allwinner H3 (Cortex-A7) | Supported |

---

## Built With

Go · lighttpd · dnsmasq · hostapd · Linux tc/htb · PostgreSQL · WiringOP · ZeroTier

---

## License

This project is proprietary software. See [LICENSE](LICENSE) for details.
