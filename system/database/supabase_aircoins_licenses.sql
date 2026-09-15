-- ============================================================================
-- AirCoins License System — Supabase-side schema
-- ============================================================================
-- Run this ONCE in your Supabase SQL editor. Creates two tables dedicated
-- to AirCoins (prefixed to avoid clashing with your existing licenses tables).
--
-- The Go API on each device uses the service-role key to read/write these
-- tables. Service-role bypasses RLS; anon key has NO policies = no access.
-- ============================================================================

CREATE TABLE IF NOT EXISTS aircoins_licenses (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    license_key TEXT UNIQUE NOT NULL,
    hardware_id TEXT,
    status TEXT NOT NULL DEFAULT 'available',  -- available, active, revoked, expired
    activated_at TIMESTAMPTZ,
    expires_at TIMESTAMPTZ,
    owner_email TEXT,
    last_heartbeat_at TIMESTAMPTZ,
    device_info JSONB,
    created_at TIMESTAMPTZ DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS aircoins_license_heartbeats (
    id BIGSERIAL PRIMARY KEY,
    license_id UUID REFERENCES aircoins_licenses(id),
    hardware_id TEXT NOT NULL,
    received_at TIMESTAMPTZ DEFAULT NOW(),
    ip_address INET,
    status TEXT
);

ALTER TABLE aircoins_licenses ENABLE ROW LEVEL SECURITY;
ALTER TABLE aircoins_license_heartbeats ENABLE ROW LEVEL SECURITY;
-- Service-role key bypasses RLS (needed for the device to read/write).
-- Anon key: no policies = no access from client-side code.

-- Helpful indexes for heartbeat lookups and license-key activation:
CREATE INDEX IF NOT EXISTS idx_aircoins_licenses_key ON aircoins_licenses(license_key);
CREATE INDEX IF NOT EXISTS idx_aircoins_licenses_hw  ON aircoins_licenses(hardware_id) WHERE hardware_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_aircoins_heartbeats_license ON aircoins_license_heartbeats(license_id, received_at DESC);
