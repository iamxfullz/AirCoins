package main

import (
	"fmt"
	"os"
	"strings"
)

func main() {
	fpath := "/c/Users/CITYCONNECT/Documents/GitHub/AirCoins/system/usr/local/bin/aircoins-api/models/models.go"
	data, err := os.ReadFile(fpath)
	if err != nil {
		fmt.Println("Error reading:", err)
		os.Exit(1)
	}

	content := string(data)

	// The exact text we're looking for (tab-indented) - the v1.22.0 block + coin_events
	oldBlock := "\t\t{\n" +
		"\t\t\t// v1.22.0 — sub-vendos: NodeMCU remote coin slots, 1 VLAN = 1 unit\n" +
		"\t\t\t\"sub_vendos table\",\n" +
		"\t\t\t`CREATE TABLE IF NOT EXISTS sub_vendos (\n" +
		"\t\t\t\tid SERIAL PRIMARY KEY,\n" +
		"\t\t\t\tname TEXT NOT NULL,\n" +
		"\t\t\t\tsite TEXT NOT NULL DEFAULT '',\n" +
		"\t\t\t\tvlan_iface TEXT NOT NULL UNIQUE,\n" +
		"\t\t\t\tapi_token_hash VARCHAR(64) NOT NULL DEFAULT '',\n" +
		"\t\t\t\tclaim_code VARCHAR(12) NOT NULL DEFAULT '',\n" +
		"\t\t\t\tclaimed BOOLEAN NOT NULL DEFAULT FALSE,\n" +
		"\t\t\t\tenabled BOOLEAN NOT NULL DEFAULT TRUE,\n" +
		"\t\t\t\tarmed_until BIGINT NOT NULL DEFAULT 0,\n" +
		"\t\t\t\twindow_started_at BIGINT NOT NULL DEFAULT 0,\n" +
		"\t\t\t\ttotal_coins INTEGER NOT NULL DEFAULT 0,\n" +
		"\t\t\t\tlast_seen TIMESTAMPTZ,\n" +
		"\t\t\t\tcreated_at TIMESTAMPTZ DEFAULT NOW()\n" +
		"\t\t\t)`,\n" +
		"\t\t},\n" +
		"\t\t{\n" +
		"\t\t\t// coin_events origin: local GPIO listener vs sub-vendo unit\n" +
		"\t\t\t\"coin_events.source\",\n" +
		"\t\t\t`ALTER TABLE coin_events ADD COLUMN IF NOT EXISTS source TEXT NOT NULL DEFAULT 'local_gpio';\n" +
		"\t\t\t CREATE INDEX IF NOT EXISTS idx_coin_events_source ON coin_events(source, processed)`,\n" +
		"\t\t},\n" +
		"\t}"

	count := strings.Count(content, oldBlock)
	fmt.Printf("Found %d occurrence(s)\n", count)

	if count != 1 {
		fmt.Println("Expected exactly 1 occurrence, aborting")
		os.Exit(1)
	}

	// New block: v1.22.0 entry updated + new v1.23.0 entries + ssid_vlan_map + coin_events
	newBlock := "\t\t{\n" +
		"\t\t\t// v1.22.0 — sub-vendos: NodeMCU remote coin slots, 1 VLAN = 1 unit\n" +
		"\t\t\t\"sub_vendos table\",\n" +
		"\t\t\t`CREATE TABLE IF NOT EXISTS sub_vendos (\n" +
		"\t\t\t\tid SERIAL PRIMARY KEY,\n" +
		"\t\t\t\tname TEXT NOT NULL DEFAULT '',\n" +
		"\t\t\t\tsite TEXT NOT NULL DEFAULT '',\n" +
		"\t\t\t\tvlan_iface TEXT,\n" +
		"\t\t\t\tapi_token_hash VARCHAR(64) NOT NULL DEFAULT '',\n" +
		"\t\t\t\tclaim_code VARCHAR(12) NOT NULL DEFAULT '',\n" +
		"\t\t\t\tclaimed BOOLEAN NOT NULL DEFAULT FALSE,\n" +
		"\t\t\t\tenabled BOOLEAN NOT NULL DEFAULT TRUE,\n" +
		"\t\t\t\tarmed_until BIGINT NOT NULL DEFAULT 0,\n" +
		"\t\t\t\twindow_started_at BIGINT NOT NULL DEFAULT 0,\n" +
		"\t\t\t\ttotal_coins INTEGER NOT NULL DEFAULT 0,\n" +
		"\t\t\t\tlast_seen TIMESTAMPTZ,\n" +
		"\t\t\t\tcreated_at TIMESTAMPTZ DEFAULT NOW()\n" +
		"\t\t\t)`,\n" +
		"\t\t},\n" +
		"\t\t{\n" +
		"\t\t\t// v1.23.0 — sub-vendos: device-registration model\n" +
		"\t\t\t\"sub_vendos.device_id/status/ssid\",\n" +
		"\t\t\t`ALTER TABLE sub_vendos ADD COLUMN IF NOT EXISTS device_id VARCHAR(32);\n" +
		"\t\t\t\t ALTER TABLE sub_vendos ADD COLUMN IF NOT EXISTS status VARCHAR(12) NOT NULL DEFAULT 'online' CHECK (status IN ('pending','online','offline','rejected'));\n" +
		"\t\t\t\t ALTER TABLE sub_vendos ADD COLUMN IF NOT EXISTS ssid VARCHAR(64);\n" +
		"\t\t\t\t UPDATE sub_vendos SET device_id = 'legacy-' || id::text WHERE device_id IS NULL;\n" +
		"\t\t\t\t ALTER TABLE sub_vendos ALTER COLUMN device_id SET NOT NULL;`,\n" +
		"\t\t},\n" +
		"\t\t{\n" +
		"\t\t\t\"sub_vendos indexes\",\n" +
		"\t\t\t`CREATE UNIQUE INDEX IF NOT EXISTS idx_subvendos_device ON sub_vendos(device_id);\n" +
		"\t\t\t\t CREATE INDEX IF NOT EXISTS idx_subvendos_vlan ON sub_vendos(vlan_iface);\n" +
		"\t\t\t\t CREATE INDEX IF NOT EXISTS idx_subvendos_status ON sub_vendos(status);`,\n" +
		"\t\t},\n" +
		"\t\t{\n" +
		"\t\t\t// SSID -> VLAN iface mapping (admin-managed)\n" +
		"\t\t\t\"ssid_vlan_map table\",\n" +
		"\t\t\t`CREATE TABLE IF NOT EXISTS ssid_vlan_map (\n" +
		"\t\t\t\tid SERIAL PRIMARY KEY,\n" +
		"\t\t\t\tssid VARCHAR(64) NOT NULL UNIQUE,\n" +
		"\t\t\t\tvlan_iface VARCHAR(32) NOT NULL,\n" +
		"\t\t\t\tsite VARCHAR(128) NOT NULL DEFAULT '',\n" +
		"\t\t\t\tactive BOOLEAN NOT NULL DEFAULT TRUE,\n" +
		"\t\t\t\tcreated_at TIMESTAMPTZ DEFAULT NOW()\n" +
		"\t\t\t)`,\n" +
		"\t\t},\n" +
		"\t\t{\n" +
		"\t\t\t// coin_events origin: local GPIO listener vs sub-vendo unit\n" +
		"\t\t\t\"coin_events.source\",\n" +
		"\t\t\t`ALTER TABLE coin_events ADD COLUMN IF NOT EXISTS source TEXT NOT NULL DEFAULT 'local_gpio';\n" +
		"\t\t\t CREATE INDEX IF NOT EXISTS idx_coin_events_source ON coin_events(source, processed)`,\n" +
		"\t\t},\n" +
		"\t}"

	content = strings.Replace(content, oldBlock, newBlock, 1)

	err = os.WriteFile(fpath, []byte(content), 0644)
	if err != nil {
		fmt.Println("Error writing:", err)
		os.Exit(1)
	}
	fmt.Println("Patched models.go successfully")
}