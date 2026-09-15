#!/usr/bin/env python3
"""Add sub-vendo schema fixes to models.go EnsureSchema"""
import re

fpath = r'system\usr\local\bin\aircoins-api\models\models.go'
with open(fpath, 'r') as f:
    content = f.read()

old_block = """\t\t{
\t\t\t// coin_events origin: local GPIO listener vs sub-vendo unit
\t\t\t"coin_events.source",
\t\t\t`ALTER TABLE coin_events ADD COLUMN IF NOT EXISTS source TEXT NOT NULL DEFAULT 'local_gpio';
\t\t\t CREATE INDEX IF NOT EXISTS idx_coin_events_source ON coin_events(source, processed)`,
\t\t},
\t}"""

new_block = """\t\t{
\t\t\t// v1.22.0 — sub-vendos: NodeMCU remote coin slots, 1 VLAN = 1 unit
\t\t\t"sub_vendos table",
\t\t\t`CREATE TABLE IF NOT EXISTS sub_vendos (
\t\t\t\tid SERIAL PRIMARY KEY,
\t\t\t\tname TEXT NOT NULL DEFAULT '',
\t\t\t\tsite TEXT NOT NULL DEFAULT '',
\t\t\t\tvlan_iface TEXT,
\t\t\t\tapi_token_hash VARCHAR(64) NOT NULL DEFAULT '',
\t\t\t\tclaim_code VARCHAR(12) NOT NULL DEFAULT '',
\t\t\t\tclaimed BOOLEAN NOT NULL DEFAULT FALSE,
\t\t\t\tenabled BOOLEAN NOT NULL DEFAULT TRUE,
\t\t\t\tarmed_until BIGINT NOT NULL DEFAULT 0,
\t\t\t\twindow_started_at BIGINT NOT NULL DEFAULT 0,
\t\t\t\ttotal_coins INTEGER NOT NULL DEFAULT 0,
\t\t\t\tlast_seen TIMESTAMPTZ,
\t\t\t\tcreated_at TIMESTAMPTZ DEFAULT NOW()
\t\t\t)`,
\t\t},
\t\t{
\t\t\t// v1.23.0 — sub-vendos: device-registration model
\t\t\t"sub_vendos.device_id/status/ssid",
\t\t\t`ALTER TABLE sub_vendos ADD COLUMN IF NOT EXISTS device_id VARCHAR(32);
\t\t\t\t ALTER TABLE sub_vendos ADD COLUMN IF NOT EXISTS status VARCHAR(12) NOT NULL DEFAULT 'online' CHECK (status IN ('pending','online','offline','rejected'));
\t\t\t\t ALTER TABLE sub_vendos ADD COLUMN IF NOT EXISTS ssid VARCHAR(64);
\t\t\t\t UPDATE sub_vendos SET device_id = 'legacy-' || id::text WHERE device_id IS NULL;
\t\t\t\t ALTER TABLE sub_vendos ALTER COLUMN device_id SET NOT NULL;`,
\t\t},
\t\t{
\t\t\t"sub_vendos indexes",
\t\t\t`CREATE UNIQUE INDEX IF NOT EXISTS idx_subvendos_device ON sub_vendos(device_id);
\t\t\t\t CREATE INDEX IF NOT EXISTS idx_subvendos_vlan ON sub_vendos(vlan_iface);
\t\t\t\t CREATE INDEX IF NOT EXISTS idx_subvendos_status ON sub_vendos(status);`,
\t\t},
\t\t{
\t\t\t// SSID -> VLAN iface mapping (admin-managed)
\t\t\t"ssid_vlan_map table",
\t\t\t`CREATE TABLE IF NOT EXISTS ssid_vlan_map (
\t\t\t\tid SERIAL PRIMARY KEY,
\t\t\t\tssid VARCHAR(64) NOT NULL UNIQUE,
\t\t\t\tvlan_iface VARCHAR(32) NOT NULL,
\t\t\t\tsite VARCHAR(128) NOT NULL DEFAULT '',
\t\t\t\tactive BOOLEAN NOT NULL DEFAULT TRUE,
\t\t\t\tcreated_at TIMESTAMPTZ DEFAULT NOW()
\t\t\t)`,
\t\t},
\t\t{
\t\t\t// coin_events origin: local GPIO listener vs sub-vendo unit
\t\t\t"coin_events.source",
\t\t\t`ALTER TABLE coin_events ADD COLUMN IF NOT EXISTS source TEXT NOT NULL DEFAULT 'local_gpio';
\t\t\t CREATE INDEX IF NOT EXISTS idx_coin_events_source ON coin_events(source, processed)`,
\t\t},
\t}"""

count = content.count(old_block)
print(f"Found {count} occurrence(s) of old_block")

content = content.replace(old_block, new_block)
with open(fpath, 'w') as f:
    f.write(content)
print("Patched models.go")