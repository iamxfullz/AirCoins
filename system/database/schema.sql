-- ============================================
-- AirCoins PisoNet - PostgreSQL Database Schema
-- ============================================
-- Database: aircoins
-- Run as (non-interactive, via postgres superuser peer auth):
--   sudo -u postgres psql -v ON_ERROR_STOP=1 -d aircoins -c 'SET ROLE aircoins;' -f schema.sql
-- (install.sh creates the database and user before running this file)
-- ============================================

-- ============================================
-- ADMIN USERS
-- ============================================
CREATE TABLE IF NOT EXISTS admin_users (
    id SERIAL PRIMARY KEY,
    username VARCHAR(50) UNIQUE NOT NULL,
    password_hash VARCHAR(255) NOT NULL,
    created_at TIMESTAMP DEFAULT NOW(),
    last_login TIMESTAMP
);

-- Insert default admin user (password: admin123)
-- Hash verified with golang.org/x/crypto/bcrypt CompareHashAndPassword
-- (the previously seeded hash did NOT match admin123 — see CHANGELOG)
INSERT INTO admin_users (username, password_hash)
VALUES ('admin', '$2a$10$PUcBM0XqvzSOG5BmRlxg9.e84jM8uMrnn5eWfYIcrbBhYOCxJ9jaG')
ON CONFLICT (username) DO NOTHING;

-- ============================================
-- SYSTEM SETTINGS
-- ============================================
CREATE TABLE IF NOT EXISTS system_settings (
    id SERIAL PRIMARY KEY,
    key VARCHAR(100) UNIQUE NOT NULL,
    value TEXT NOT NULL,
    description TEXT,
    updated_at TIMESTAMP DEFAULT NOW()
);

-- Insert default settings
INSERT INTO system_settings (key, value, description) VALUES
    ('bandwidth', '50', 'Bandwidth limit in Mbps'),
    ('max_session', '120', 'Maximum session time in minutes'),
    ('eth_interface', 'eth0', 'Ethernet interface name'),
    ('portal_ip', '', 'Portal IP address for admin access'),
    ('portal_tap_rules', '{"max_taps":5,"window_seconds":60,"ban_seconds":300}', 'INSERT COIN anti-abuse limits (per MAC, per window)'),
    ('portal_pause_rules', '{"pause_limit":0}', 'Session pause rules (pause_limit=0 means unlimited)')
ON CONFLICT (key) DO NOTHING;

-- Default captive portal appearance (dark preset). Guarded so a fresh
-- install matches migration 009 exactly and an existing operator theme
-- is never overwritten (this file also runs on updates, before
-- migrations.sql).
INSERT INTO system_settings (key, value, description) VALUES
    ('portal_appearance',
     '{"theme":"dark","colors":{"primary":"#1a1a2e","accent":"#ffd700","background":"#16213e","card":"#1f2b47","text":"#eaeaea","button":"#ffa500","button_text":"#1a1a2e"},"background_image":""}',
     'Captive portal appearance (theme, colors, background image) as a JSON document')
ON CONFLICT (key) DO NOTHING;

-- ============================================
-- GPIO CONFIGURATION
-- ============================================
CREATE TABLE IF NOT EXISTS gpio_config (
    id SERIAL PRIMARY KEY,
    pin INTEGER NOT NULL DEFAULT 7,
    coin_value INTEGER NOT NULL DEFAULT 1,
    pulse_mode VARCHAR(20) NOT NULL DEFAULT 'falling',
    board_model VARCHAR(50) DEFAULT 'auto',
    debounce_ms INTEGER NOT NULL DEFAULT 30,
    relay_enabled BOOLEAN NOT NULL DEFAULT false,
    relay_pin INTEGER NOT NULL DEFAULT 5,
    relay_intensity INTEGER NOT NULL DEFAULT 5,
    updated_at TIMESTAMP DEFAULT NOW()
);

-- Insert default GPIO config (only if table is empty)
INSERT INTO gpio_config (pin, coin_value, pulse_mode, debounce_ms)
SELECT 7, 1, 'falling', 30
WHERE NOT EXISTS (SELECT 1 FROM gpio_config LIMIT 1);

-- ============================================
-- PRICING TABLE
-- ============================================
-- Model: 1 peso coin = 1 pulse from the coin acceptor.
-- A row maps the inserted peso amount (= pulse count) to minutes
-- of internet. There is no rate-per-minute anywhere in the system.
--
-- This table intentionally starts EMPTY — the operator enters every
-- tier manually from the admin panel (Pricing section). Do NOT seed
-- default rows here.
CREATE TABLE IF NOT EXISTS pricing (
    id SERIAL PRIMARY KEY,
    coin_value INTEGER NOT NULL UNIQUE,
    minutes INTEGER NOT NULL,
    active BOOLEAN DEFAULT true,
    -- false = consumable rate (no Pause button in the portal); true = pausable.
    pausable BOOLEAN DEFAULT true,
    -- Maximum wall-clock pause window (hours) before a paused session is
    -- forcibly expired (remaining becomes 0, user must re-insert coin).
    -- 0 = frozen indefinitely while paused (legacy behaviour).
    expiration_hours INTEGER DEFAULT 0,
    created_at TIMESTAMP DEFAULT NOW(),
    updated_at TIMESTAMP DEFAULT NOW()
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_pricing_coin_value ON pricing(coin_value);

-- ============================================
-- SESSIONS (must come before coin_events — FK dependency)
-- ============================================
CREATE TABLE IF NOT EXISTS sessions (
    id SERIAL PRIMARY KEY,
    client_ip VARCHAR(45),
    client_mac VARCHAR(17),
    coins_inserted INTEGER DEFAULT 0,
    total_seconds INTEGER DEFAULT 0,
    remaining_seconds INTEGER DEFAULT 0,
    status VARCHAR(20) DEFAULT 'pending', -- pending, active, expired, cancelled
    started_at TIMESTAMP DEFAULT NOW(),
    activated_at TIMESTAMP,
    expired_at TIMESTAMP,
    -- Wall-clock expiry: a session is alive while expires_at > NOW().
    -- remaining_seconds above is only a snapshot for display/legacy rows.
    expires_at TIMESTAMP,
    -- Pause/resume: when paused the session timer and internet access
    -- are both frozen. paused_at is set on pause; remaining_seconds_at_pause
    -- is the live remaining at that moment, used on resume to rebuild expires_at.
    paused_at TIMESTAMPTZ,
    remaining_seconds_at_pause INT,
    pause_count INT DEFAULT 0,
    -- Snapshot of the rate's pause behaviour at purchase:
    -- false = consumable (no Pause button in portal), true = pausable.
    pausable BOOLEAN DEFAULT true,
    -- Max wall-clock pause window (hours) for this session (from the rate).
    expiration_hours INTEGER DEFAULT 0,
    -- Absolute wall-clock deadline a PAUSED session must be resumed by; when
    -- reached the session is forcibly expired (remaining -> 0). NULL = frozen.
    pause_expires_at TIMESTAMPTZ,
    -- Per-session speed override (Mbps). NULL = use the portal's global
    -- per_device_bw_mbps from portal_qdisc_rules. >0 = override for this session.
    shaped_mbps INT,
    -- Per-device session token for MAC-randomization roaming (migration 012).
    -- 8 hex chars, generated client-side, unique per active session.
    session_token VARCHAR(8),
    created_at TIMESTAMP DEFAULT NOW()
);

-- Indexes for faster queries
CREATE INDEX IF NOT EXISTS idx_sessions_status ON sessions(status);
CREATE INDEX IF NOT EXISTS idx_sessions_client_ip ON sessions(client_ip);
CREATE INDEX IF NOT EXISTS idx_sessions_client_mac ON sessions(client_mac);
CREATE INDEX IF NOT EXISTS idx_sessions_started_at ON sessions(started_at);
-- NOTE: the (status, expires_at) index is created in migrations.sql only:
-- this file runs BEFORE migrations on updates, and pre-expires_at
-- databases don't have the column yet at this point.

-- Compound indexes for pagination and filter queries
CREATE INDEX IF NOT EXISTS idx_sessions_started_at_desc ON sessions(started_at DESC);
CREATE INDEX IF NOT EXISTS idx_sessions_status_started_at ON sessions(status, started_at DESC);

-- One session row per device: the API upserts on client_mac, reusing the
-- expired row when the device pays again. Old databases may still hold
-- duplicate MACs when this file runs (it runs BEFORE migrations.sql), so
-- only create the index when the data allows it — migration 008 dedupes
-- and creates it otherwise.
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM sessions
        WHERE client_mac IS NOT NULL AND client_mac <> '' AND client_mac <> '-'
        GROUP BY client_mac
        HAVING COUNT(*) > 1
    ) THEN
        CREATE UNIQUE INDEX IF NOT EXISTS sessions_client_mac_uniq
        ON sessions(client_mac)
        WHERE client_mac IS NOT NULL AND client_mac <> '' AND client_mac <> '-';
    END IF;
END
$$;

-- ============================================
-- VOUCHERS
-- ============================================
-- Pre-paid time vouchers (6-char alphanumeric codes) generated by the
-- admin. Redemption grants duration_minutes to the calling device and
-- binds the voucher to the resulting session + session_token so the
-- subscriber can roam between SSIDs (portals). plan='monthly' marks
-- 30-day subscription vouchers.
CREATE TABLE IF NOT EXISTS vouchers (
    id SERIAL PRIMARY KEY,
    code VARCHAR(16) NOT NULL UNIQUE,
    batch_code VARCHAR(20) NOT NULL DEFAULT '',    -- generation batch (print runs)
    duration_minutes INTEGER NOT NULL,
    plan VARCHAR(20) NOT NULL DEFAULT 'time',      -- 'time' | 'monthly'
    status VARCHAR(20) NOT NULL DEFAULT 'unused',  -- unused | used | disabled
    price NUMERIC(10,2) NOT NULL DEFAULT 0,        -- sale price in ₱ per voucher
    notes TEXT NOT NULL DEFAULT '',
    pausable BOOLEAN NOT NULL DEFAULT TRUE,        -- consumable vouchers cannot pause
    expiration_hours INTEGER NOT NULL DEFAULT 0,   -- pause deadline after FIRST use (0 = none)
    redeemed_at TIMESTAMPTZ,
    redeemed_mac VARCHAR(17),
    session_id INTEGER,
    session_token VARCHAR(8),
    created_at TIMESTAMPTZ DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_vouchers_status ON vouchers(status);
CREATE INDEX IF NOT EXISTS idx_vouchers_created_at ON vouchers(created_at DESC);
CREATE INDEX IF NOT EXISTS idx_vouchers_batch ON vouchers(batch_code);

-- Per-device session token for MAC-randomization roaming (migration 012).
-- Unique partial index so a token identifies exactly one active session.
CREATE UNIQUE INDEX IF NOT EXISTS idx_sessions_session_token
  ON sessions(session_token)
  WHERE session_token IS NOT NULL AND session_token <> '';

-- ============================================
-- TAP ANTI-ABUSE + SESSION PAUSE (migration 010)
-- ============================================
-- Per-device ban table for tap-spamming INSERT COIN, and a sliding-window
-- tap-activity counter per MAC. Both are ephemeral and self-clearing.
CREATE TABLE IF NOT EXISTS client_bans (
    id SERIAL PRIMARY KEY,
    client_mac VARCHAR(17) UNIQUE NOT NULL,
    banned_until TIMESTAMPTZ NOT NULL,
    reason VARCHAR(32),
    attempts_in_window INT,
    created_at TIMESTAMPTZ DEFAULT NOW(),
    updated_at TIMESTAMPTZ DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_client_bans_until ON client_bans(banned_until);

CREATE TABLE IF NOT EXISTS tap_activity (
    client_mac VARCHAR(17) PRIMARY KEY,
    counter INT NOT NULL DEFAULT 0,
    window_started_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ DEFAULT NOW()
);

-- ============================================
-- COIN EVENTS (raw coin detections)
-- ============================================
CREATE TABLE IF NOT EXISTS coin_events (
    id SERIAL PRIMARY KEY,
    coin_value INTEGER NOT NULL,
    detected_at TIMESTAMP DEFAULT NOW(),
    processed BOOLEAN DEFAULT false,
    session_id INTEGER REFERENCES sessions(id),
    source TEXT NOT NULL DEFAULT 'local_gpio'  -- credited coinslot origin
);

-- ============================================
-- SYSTEM LOGS
-- ============================================
CREATE TABLE IF NOT EXISTS system_logs (
    id SERIAL PRIMARY KEY,
    level VARCHAR(10) NOT NULL DEFAULT 'INFO', -- INFO, WARN, ERROR, DEBUG
    component VARCHAR(50), -- gpio, session, admin, system
    message TEXT NOT NULL,
    metadata JSONB,
    created_at TIMESTAMP DEFAULT NOW()
);

-- Indexes for log queries
CREATE INDEX IF NOT EXISTS idx_logs_level ON system_logs(level);
CREATE INDEX IF NOT EXISTS idx_logs_created_at ON system_logs(created_at);
CREATE INDEX IF NOT EXISTS idx_logs_component ON system_logs(component);

-- ============================================
-- DAILY STATS (aggregated)
-- ============================================
CREATE TABLE IF NOT EXISTS daily_stats (
    id SERIAL PRIMARY KEY,
    date DATE UNIQUE NOT NULL,
    total_earnings INTEGER DEFAULT 0,
    total_coins INTEGER DEFAULT 0,
    total_sessions INTEGER DEFAULT 0,
    active_users INTEGER DEFAULT 0,
    created_at TIMESTAMP DEFAULT NOW(),
    updated_at TIMESTAMP DEFAULT NOW()
);

-- Index for date queries
CREATE INDEX IF NOT EXISTS idx_daily_stats_date ON daily_stats(date);

-- ============================================
-- VLAN CONFIGURATION
-- ============================================
-- Network identity ONLY (parent iface, 802.1Q id, name). The hotspot
-- stack (portal IP, DHCP, DNS hijack, captive rules) lives in
-- portal_servers below. Old databases with ip_address/start_ip/
-- is_portal columns are converted by migration 007 in migrations.sql.
CREATE TABLE IF NOT EXISTS vlan_config (
    id SERIAL PRIMARY KEY,
    interface VARCHAR(20) NOT NULL,
    vlan_id INTEGER NOT NULL,
    description VARCHAR(100) DEFAULT '',
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    UNIQUE(interface, vlan_id)
);

-- ============================================
-- PORTAL SERVERS
-- ============================================
-- One row provisions the full hotspot portal stack on ONE interface:
-- portal IP, dnsmasq DHCP + wildcard DNS hijack, captive iptables
-- rules. interface is usually a VLAN from vlan_config (e.g. end0.22)
-- but any interface name is accepted. VLANs without a portal_servers
-- row are plain networks (uplinks/management) — no IP, no DHCP.
CREATE TABLE IF NOT EXISTS portal_servers (
    id SERIAL PRIMARY KEY,
    interface VARCHAR(32) UNIQUE NOT NULL,
    portal_ip_cidr VARCHAR(18) NOT NULL,
    dhcp_start VARCHAR(15) NOT NULL DEFAULT '',
    dhcp_end VARCHAR(15) NOT NULL DEFAULT '',
    dhcp_lease VARCHAR(20) NOT NULL DEFAULT '12h',
    enabled BOOLEAN DEFAULT true,
    anti_hotspot BOOLEAN DEFAULT false,
    created_at TIMESTAMP DEFAULT NOW(),
    updated_at TIMESTAMP DEFAULT NOW()
);

-- Default portal VLAN setting
INSERT INTO system_settings (key, value, description) VALUES
    ('portal_vlan', '', 'Primary VLAN ID for portal access')
ON CONFLICT (key) DO NOTHING;

-- ============================================
-- BRIDGE MANAGEMENT
-- ============================================
-- Linux bridge interfaces for grouping VLANs (or other interfaces) into
-- a single L2 broadcast domain. bridge_config stores the bridge identity;
-- bridge_members tracks which interfaces are enslaved to each bridge.
-- The portal stack (portal_servers) can be provisioned on a bridge just
-- like on a VLAN interface.
CREATE TABLE IF NOT EXISTS bridge_config (
    id SERIAL PRIMARY KEY,
    name VARCHAR(32) UNIQUE NOT NULL,
    description VARCHAR(100) DEFAULT '',
    created_at TIMESTAMP DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS bridge_members (
    id SERIAL PRIMARY KEY,
    bridge_id INT REFERENCES bridge_config(id) ON DELETE CASCADE,
    member_iface VARCHAR(32) NOT NULL,
    UNIQUE(bridge_id, member_iface)
);

-- ============================================
-- WIFI HOTSPOT (HOSTAPD)
-- ============================================
-- A wireless adapter can be turned into an OPEN access point (hotspot)
-- managed by hostapd. Security is intentionally passwordless — the
-- hotspot is a captive-portal entry interface; the portal stack
-- (portal_servers, dnsmasq) is provisioned on the interface separately.
CREATE TABLE IF NOT EXISTS wifi_ap_config (
    id SERIAL PRIMARY KEY,
    interface VARCHAR(32) UNIQUE NOT NULL,
    ssid VARCHAR(32) NOT NULL,
    channel INTEGER NOT NULL DEFAULT 6,
    hw_mode VARCHAR(4) NOT NULL DEFAULT 'g',
    country_code VARCHAR(4) NOT NULL DEFAULT 'PH',
    enabled BOOLEAN NOT NULL DEFAULT FALSE,
    created_at TIMESTAMPTZ DEFAULT NOW(),
    updated_at TIMESTAMPTZ DEFAULT NOW()
);

-- ============================================
-- FUNCTIONS
-- ============================================

-- Function to update daily stats
CREATE OR REPLACE FUNCTION update_daily_stats()
RETURNS TRIGGER AS $$
BEGIN
    INSERT INTO daily_stats (date, total_earnings, total_coins, total_sessions)
    VALUES (
        CURRENT_DATE,
        COALESCE(NEW.coins_inserted, 0),
        CASE WHEN NEW.coins_inserted > 0 THEN 1 ELSE 0 END,
        CASE WHEN NEW.status = 'active' THEN 1 ELSE 0 END
    )
    ON CONFLICT (date) DO UPDATE SET
        total_earnings = daily_stats.total_earnings + COALESCE(NEW.coins_inserted, 0),
        total_coins = daily_stats.total_coins + (CASE WHEN NEW.coins_inserted > 0 THEN 1 ELSE 0 END),
        total_sessions = daily_stats.total_sessions + (CASE WHEN NEW.status = 'active' THEN 1 ELSE 0 END),
        updated_at = NOW();
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

-- Trigger to auto-update daily stats when session changes
-- Drop first since CREATE TRIGGER has no IF NOT EXISTS in older PG versions
DROP TRIGGER IF EXISTS trigger_update_daily_stats ON sessions;
CREATE TRIGGER trigger_update_daily_stats
AFTER INSERT OR UPDATE ON sessions
FOR EACH ROW
EXECUTE FUNCTION update_daily_stats();

-- ============================================
-- VIEWS
-- ============================================

-- View for today's stats
CREATE OR REPLACE VIEW today_stats AS
SELECT 
    COALESCE(SUM(total_earnings), 0) as earnings,
    COALESCE(SUM(total_coins), 0) as coins,
    COALESCE(SUM(total_sessions), 0) as sessions
FROM daily_stats
WHERE date = CURRENT_DATE;

-- View for active sessions.
-- Deliberately references only pre-expires_at columns: this file runs
-- BEFORE migrations.sql on updates, where old databases don't have
-- expires_at yet. Migration 006 recreates it wall-clock based right after
-- adding the column, so the final definition always wins.
CREATE OR REPLACE VIEW active_sessions AS
SELECT * FROM sessions
WHERE status = 'active' AND remaining_seconds > 0;

-- ============================================
-- PERMISSIONS
-- ============================================
-- Grant permissions to www-data for CGI scripts
GRANT SELECT, INSERT, UPDATE ON ALL TABLES IN SCHEMA public TO "www-data";
GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA public TO "www-data";

-- ============================================
-- COMPLETION
-- ============================================
\echo 'Database schema created successfully!'
\echo 'Default admin credentials: admin / admin123'
