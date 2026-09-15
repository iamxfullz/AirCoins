# AirCoins PisoNet - Deployment Guide

## Multi-Platform Setup & Installation Instructions

**Supported Platforms:**
- Orange Pi One (ARM) - Full hardware support with GPIO coin acceptor
- Ubuntu/Debian x86_64 - Development mode (API testing, no GPIO hardware)

---

## 1. Required Hardware (Orange Pi One)

| Component | Specification |
|-----------|--------------|
| **Single Board Computer** | Orange Pi One (Allwinner H3, 512MB RAM) |
| **Internet Connection** | Ethernet (eth0) connected to ISP router |
| **Coin Acceptor** | Multi-coin acceptor with pulse output (5V, programmable) |
| **Power Supply** | 5V 3A micro USB |
| **MicroSD Card** | 16GB+ Class 10 (for OS) |
| **Enclosure** | Metal/plastic case with coin slot cutout |

### GPIO Wiring (Coin Acceptor → Orange Pi)

```
Coin Acceptor          Orange Pi One
─────────────          ─────────────
Pulse Output    →      Physical Pin 7 (PA6 / wiringPi 7)
GND             →      Physical Pin 6 (GND)
VCC (5V)        →      External 5V supply (do NOT power from Orange Pi 5V pin)
```

> **Important:** Use a separate 5V supply for the coin acceptor. Drawing too much current from the Orange Pi's 5V pin can cause instability.

---

## 2. Operating System

### Option A: Orange Pi One (ARM)

#### Recommended OS

| OS | Version | Notes |
|----|---------|-------|
| **Armbian** | 23.x+ (Bookworm/Bullseye) | **RECOMMENDED** - Best community support |
| Orange Pi OS | Official image | Works but less community support |
| Raspbian (legacy) | Debian 11 | Compatible with H3-based boards |

#### Download Armbian for Orange Pi One

1. Go to: https://www.armbian.com/orange-pi-one/
2. Download the **Bookworm** (Debian 12) server image
3. Flash to MicroSD card using:
   - **Balena Etcher** (Windows/Mac/Linux): https://etcher.balena.io/
   - **Rufus** (Windows): https://rufus.akeo.ie/
   - **dd** (Linux): `sudo dd if=Armbian.img of=/dev/sdX bs=4M status=progress`

### Initial OS Setup

```bash
# 1. Boot Orange Pi from MicroSD (first boot takes 1-2 minutes)

# 2. Connect via SSH or serial console
#    Default credentials:
#    Username: root
#    Password: 1234 (will prompt change on first login)

# 3. Run initial setup
armbian-config            # Or use nmtui for network config

# 4. Set static IP for Ethernet (internet uplink)
#    Edit /etc/network/interfaces or use NetworkManager
nano /etc/network/interfaces
```

**/etc/network/interfaces** (Ethernet - internet uplink):
```
auto eth0
iface eth0 inet dhcp
```

```bash
# 5. Update system
apt-get update && apt-get upgrade -y
```

### Option B: Ubuntu/Debian x86_64 (Development Mode)

For development and testing on a regular PC or VM.

#### Requirements

| Component | Specification |
|-----------|--------------|
| **OS** | Ubuntu 20.04+ or Debian 11+ |
| **RAM** | 1GB minimum (2GB recommended) |
| **Disk** | 10GB free space |
| **Network** | Ethernet for LAN access |

#### Setup

```bash
# 1. Install Ubuntu/Debian (server or desktop)
# Download from: https://ubuntu.com/download/server

# 2. Update system
sudo apt-get update && sudo apt-get upgrade -y

# 3. Ensure SSH access (optional, for remote management)
sudo apt-get install -y openssh-server
sudo systemctl enable ssh
```

> **Note:** On x86, GPIO hardware is unavailable. The system runs in development mode where coin events can be simulated via API calls or file-based events.

---

## 3. Quick Install (Auto Installation)

The installer auto-detects your architecture (ARM or x86) and configures accordingly.

### One-Line Install from Git

```bash
# Clone and install in one command
sudo bash -c "$(wget -qO- https://raw.githubusercontent.com/iamxfullz/AirCoins/main/install.sh)"

# Or if git is already installed on the Orange Pi:
cd /opt
sudo git clone https://github.com/iamxfullz/AirCoins.git /opt/aircoins
cd /opt/aircoins
sudo bash install.sh
```

### What the Installer Does

The `install.sh` script automatically:
1. Detects architecture (ARM Orange Pi or x86 Ubuntu/Debian)
2. Updates system packages
3. Installs PostgreSQL, Go, lighttpd
4. Creates PostgreSQL database and user
5. Runs database schema migrations
6. Builds and deploys the Go API server
7. Deploys all configuration files
8. Deploys the web portal (index.html, admin.html)
9. Deploys GPIO listener and session manager
10. Installs and enables systemd services

**Platform-specific behavior:**
- **ARM (Orange Pi):** Full setup with GPIO and Ethernet LAN
- **x86 (Ubuntu/Debian):** Development mode — skips GPIO hardware, uses file-based coin simulation

> **Note:** A reboot is recommended after installation before first use.

---

## 4. Manual Installation

### Step 1: Transfer Project Files

#### Option A: Git Clone (Recommended)

```bash
# On Orange Pi
cd /opt
git clone https://github.com/iamxfullz/AirCoins.git /opt/aircoins
cd /opt/aircoins
```

#### Option B: SCP from your computer

```bash
# From your Windows/Mac/Linux machine
scp -r ./AirCoins root@<orange-pi-ip>:/opt/aircoins
```

#### Option C: USB Drive

```bash
# Copy AirCoins folder to USB drive (FAT32)
# Mount on Orange Pi:
mount /dev/sda1 /mnt
cp -r /mnt/AirCoins /opt/aircoins
umount /mnt
```

### Step 2: Run Installer

```bash
# Navigate to project directory
cd /opt/aircoins

# Make install script executable
chmod +x install.sh

# Run installer (must be root)
sudo bash install.sh
```

> **Note:** If WiringOP installation fails and no GPIO hardware is detected, the GPIO listener will exit with a fatal error. Ensure WiringOP is properly installed before deployment.

---

## 5. Post-Installation

### Reboot (Recommended)

```bash
sudo reboot
```

### Start Services

```bash
# Start all PisoNet services
sudo pisowifi-ctl start

# Check status
sudo pisowifi-ctl status
```

### Admin Panel Access via Ethernet LAN

The web server listens on **all interfaces**, so after deployment the admin
panel is accessible from any device on the same LAN:

```bash
# Find the device's Ethernet IP
ip addr show eth0

# Then from any browser on the same LAN:
# http://<device-ethernet-ip>            (customer portal)
# http://<device-ethernet-ip>/admin.html (admin panel)
```

### Verify Everything Works

```bash
# 1. Check web portal is accessible
curl -I http://localhost
# Should return HTTP 200

# 2. Check GPIO listener is running
sudo pisowifi-ctl status
# GPIO Coin Listener should show: RUNNING

# 3. Check Go API is responding
curl -I http://localhost:8080/api/system/status

# 4. Check lighttpd is running
sudo systemctl status lighttpd
```

---

## 6. Testing

### Test GPIO (with real coin acceptor)

```bash
# Monitor GPIO log in real-time
tail -f /var/log/pisowifi/gpio-coin.log

# Insert a real coin - you should see:
# [2026-07-03 15:30:00] COIN DETECTED! Pulse #1 | Value: ₱1

# Monitor session log
tail -f /var/log/pisowifi/session.log
```

### Test GPIO Pin from Admin Panel

1. Go to **http://<device-ethernet-ip>/admin.html**
2. Login with **admin / admin123**
3. Select the GPIO pin in the **GPIO Pin Configuration** section
4. Click **Test Pin** to read the real pin state
5. The result shows the actual GPIO state (HIGH/LOW) and detection method

### Testing on x86 (Development Mode)

On x86 systems without GPIO hardware, you can simulate coin events:

#### Method 1: Via API (Recommended)

```bash
# Send a coin event directly to the Go API
curl -X POST http://localhost:8080/api/gpio/coin \
    -H "Content-Type: application/json" \
    -d '{"coin_value": 5}'

# Check the API is working
curl http://localhost:8080/api/system/status
```

#### Method 2: Via File-Based Event

```bash
# The GPIO listener runs in file-based test mode on x86
# Write a coin value to the event file to simulate a coin insertion
echo '5' > /var/lib/pisowifi/coin_event

# Monitor the log to see it processed
tail -f /var/log/pisowifi/gpio-coin.log
# Should show: COIN DETECTED (test mode)! Pulse #1 | Value: P5
```

#### Verify Services on x86

```bash
# Check all services are running
sudo pisowifi-ctl status

# Check Go API is responding
curl -I http://localhost:8080/api/system/status

# Check web portal
curl -I http://localhost

# View logs
sudo pisowifi-ctl logs
```

---

## 7. Service Management

```bash
# Start all services
sudo pisowifi-ctl start

# Stop all services
sudo pisowifi-ctl stop

# Restart all services
sudo pisowifi-ctl restart

# View status
sudo pisowifi-ctl status

# View recent logs (last 50 lines)
sudo pisowifi-ctl logs

# View more logs
sudo pisowifi-ctl logs 200
```

### Individual Service Control

```bash
# Go API Server (REST API backend)
sudo systemctl start aircoins-api
sudo systemctl status aircoins-api
sudo journalctl -u aircoins-api -f

# GPIO Coin Listener
sudo systemctl start gpio-coin-listener
sudo systemctl status gpio-coin-listener
sudo journalctl -u gpio-coin-listener -f

# Session Manager
sudo systemctl start pisowifi-session
sudo systemctl status pisowifi-session
sudo journalctl -u pisowifi-session -f

# Web Server
sudo systemctl start lighttpd

# PostgreSQL Database
sudo systemctl status postgresql
```

---

## 8. Configuration

### Change Pricing

Edit in the **Admin Panel** (http://<device-ethernet-ip>/admin.html → Admin → Login)
- Default credentials: `admin` / `admin123`
- Click **Edit Pricing** to change minutes per coin

Or edit directly:
```bash
sudo nano /var/www/html/index.html
# Find: pricing: { 1: 5, 5: 30, 10: 60 }
# Format: { coin_value: minutes }
```

### Change GPIO Pin

```bash
sudo nano /usr/local/bin/gpio-coin-listener
# Edit: COIN_PULSE_PIN=7    (change to your wiringPi pin number)
sudo systemctl restart gpio-coin-listener
```

---

## 9. Troubleshooting

### Portal Not Loading

```bash
# Check lighttpd is running
sudo systemctl status lighttpd

# Check portal files exist
ls -la /var/www/html/

# Check lighttpd error log
tail -50 /var/log/lighttpd/error.log

# Test locally on Orange Pi
curl http://localhost/
```

### GPIO Not Detecting Coins

```bash
# Check WiringOP is installed
gpio -v
gpio readall        # Show all pin states

# Check pin mode
gpio mode 7 up      # Set pull-up
gpio mode 7 input   # Set as input
gpio read 7         # Should read 1 (HIGH) when no coin

# Check log
tail -f /var/log/pisowifi/gpio-coin.log
```

---

## 10. System Architecture

### ARM (Orange Pi One) - Full Hardware

```
┌─────────────────────────────────────────────────────────────────┐
│                        Orange Pi One                            │
│                                                                  │
│  ┌──────────────────────────┐                                    │
│  │        lighttpd          │                                    │
│  │   (Web Server + Proxy)   │                                    │
│  └──────────┬───────────────┘                                    │
│             │                                                    │
│  ┌──────────┴────────────────────────────────────────────────┐   │
│  │  /api/* → Go API :8080 (reverse proxy)                    │   │
│  └──────────────────────────┬────────────────────────────────┘   │
│                              │                                   │
│  ┌──────────────────┐    ┌───┴─────────────────────────────┐    │
│  │ GPIO Coin        │    │     Go API Server (:8080)        │    │
│  │ Listener         │───▶│  REST API Endpoints:             │    │
│  │ (gpio-coin-      │    │  /api/admin/*                    │    │
│  │  listener)       │    │  /api/session/*                  │    │
│  └──────────────────┘    │  /api/gpio/*                     │    │
│                          │  /api/pricing/*                   │    │
│  ┌──────────────────┐    │  /api/system/*                    │    │
│  │ Session Manager  │───▶└──────────────────┬──────────────┘    │
│  │ (pisowifi-       │                       │                   │
│  │  session-manager)│    ┌──────────────────┴───────────────┐   │
│  └──────────────────┘    │       PostgreSQL Database         │   │
│                          │  Tables:                          │   │
│  ┌─────────────────────┐ │  - admin_users, sessions          │   │
│  │  eth0 (Ethernet)    │ │  - coin_events, gpio_config       │   │
│  │  ISP Router (LAN)   │ │  - pricing, system_settings       │   │
│  │  Clients access via │ │  - daily_stats, system_logs       │   │
│  │  Ethernet LAN       │ └───────────────────────────────────┘   │
│  └─────────────────────┘                                         │
│                                                                  │
│  ┌───────────────────────────────────────────────────────────┐   │
│  │  Frontend (HTML/JS)                                       │   │
│  │  - index.html (Customer Portal)                           │   │
│  │  - admin.html (Admin Dashboard)                           │   │
│  │  Access: http://<device-ethernet-ip>/admin.html           │   │
│  └───────────────────────────────────────────────────────────┘   │
└─────────────────────────────────────────────────────────────────┘
```

### x86 (Ubuntu/Debian) - Development Mode

```
┌─────────────────────────────────────────────────────────────────┐
│                    Ubuntu/Debian x86_64                          │
│                                                                  │
│  ┌──────────────────────────┐                                    │
│  │        lighttpd          │                                    │
│  │   (Web Server + Proxy)   │                                    │
│  └──────────┬───────────────┘                                    │
│             │                                                    │
│  ┌──────────┴────────────────────────────────────────────────┐   │
│  │  /api/* → Go API :8080 (reverse proxy)                    │   │
│  └──────────────────────────┬────────────────────────────────┘   │
│                              │                                   │
│  ┌──────────────────┐        │                                   │
│  │ GPIO Listener    │        │                                   │
│  │ (file-based test │────────│  (no real GPIO - reads from      │
│  │  mode)           │        │   /var/lib/pisowifi/coin_event)   │
│  └──────────────────┘        │                                   │
│                              │                                   │
│  ┌──────────────────┐        │                                   │
│  │ Session Manager  │────────│                                   │
│  └──────────────────┘        │                                   │
│                              │                                   │
│           ┌──────────────────┴────────────────┐                   │
│           │       PostgreSQL Database         │                   │
│           │  (all data persisted here)        │                   │
│           └───────────────────────────────────┘                   │
│                                                                  │
│           ┌───────────────────────────────────┐                   │
│           │  Frontend (HTML/JS)               │                   │
│           │  - index.html (Customer Portal)   │                   │
│           │  - admin.html (Admin Dashboard)   │                   │
│           │  Access via: http://localhost      │                   │
│           └───────────────────────────────────┘                   │
└─────────────────────────────────────────────────────────────────┘
```

> **Note:** On x86, real GPIO hardware is not available. The web portal is accessed directly via localhost or the machine's IP address.

---

## 11. File Locations Reference

| File | Path | Purpose |
|------|------|---------|
| Web Portal | `/var/www/html/index.html` | Customer portal page |
| Admin Portal | `/var/www/html/admin.html` | Admin dashboard |
| Go API binary | `/usr/local/bin/aircoins-api/aircoins-api` | REST API server |
| lighttpd config | `/etc/lighttpd/lighttpd.conf` | Web server + reverse proxy |
| GPIO listener | `/usr/local/bin/gpio-coin-listener` | Coin detection daemon |
| Session manager | `/usr/local/bin/pisowifi-session-manager` | Session + auth manager |
| API updater | `/usr/local/bin/pisowifi-api-update` | JSON status generator |
| Control script | `/usr/local/bin/pisowifi-ctl` | Service control |
| Database schema | Source: `system/database/schema.sql` | PostgreSQL tables |
| GPIO config | `/var/lib/pisowifi/gpio_config` | GPIO pin settings |
| Coin event file | `/var/lib/pisowifi/coin_event` | Coin event (test mode on x86) |
| GPIO log | `/var/log/pisowifi/gpio-coin.log` | Coin detection log |
| Session log | `/var/log/pisowifi/session.log` | Session activity log |

---

## 12. Security Notes

- **Change admin password** after first login (Admin → Settings)
- **Default credentials:** admin / admin123 (change immediately on production)
- The admin panel is accessible from any device on the same Ethernet LAN at `http://<device-ethernet-ip>/admin.html`
- Regularly update system: `apt-get update && apt-get upgrade -y`
- **x86:** For development only - do not expose to untrusted networks

---

## 13. Support

- Armbian Documentation: https://docs.armbian.com/
- Orange Pi Wiki: http://www.orangepi.org/orangepiwiki/
- WiringOP GPIO: https://github.com/orangepi-xunlong/wiringOP
- Go: https://go.dev/doc/
- PostgreSQL: https://www.postgresql.org/docs/
