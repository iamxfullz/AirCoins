package models

import (
	"database/sql"
	"fmt"
	"log"
	"time"

	_ "github.com/lib/pq"
)

var DB *sql.DB

type DBConfig struct {
	Host     string
	Port     string
	User     string
	Password string
	DBName   string
}

func InitDB(config DBConfig) error {
	connStr := fmt.Sprintf("host=%s port=%s user=%s password=%s dbname=%s sslmode=disable",
		config.Host, config.Port, config.User, config.Password, config.DBName)

	var err error
	DB, err = sql.Open("postgres", connStr)
	if err != nil {
		return fmt.Errorf("failed to open database: %w", err)
	}

	// Test connection
	err = DB.Ping()
	if err != nil {
		return fmt.Errorf("failed to ping database: %w", err)
	}

	// Set connection pool settings
	DB.SetMaxOpenConns(25)
	DB.SetMaxIdleConns(5)
	DB.SetConnMaxLifetime(5 * time.Minute)

	return nil
}

// EnsureSchema applies lightweight, idempotent schema fixes that OTA
// updates depend on but cannot get from migrations.sql — the OTA path
// (updater.go PerformUpdate) deliberately replaces files only and skips
// install.sh, so it never runs the SQL migration files. Every statement
// here MUST be safe to run repeatedly on an already-provisioned database.
// Failures are logged but not fatal: a missing column degrades one
// feature and must not prevent the whole API from starting.
func EnsureSchema() {
	fixes := []struct {
		name string
		sql  string
	}{
		{
			// v1.17.2 — GPIO pulse debounce setting (migrations.sql 015)
			"gpio_config.debounce_ms",
			`ALTER TABLE gpio_config ADD COLUMN IF NOT EXISTS debounce_ms INTEGER NOT NULL DEFAULT 30`,
		},
		{
			// v1.18.0 — pre-paid time vouchers (migrations.sql 016)
			"vouchers table",
			`CREATE TABLE IF NOT EXISTS vouchers (
				id SERIAL PRIMARY KEY,
				code VARCHAR(16) NOT NULL UNIQUE,
				duration_minutes INTEGER NOT NULL,
				plan VARCHAR(20) NOT NULL DEFAULT 'time',
				status VARCHAR(20) NOT NULL DEFAULT 'unused',
				notes TEXT NOT NULL DEFAULT '',
				redeemed_at TIMESTAMPTZ,
				redeemed_mac VARCHAR(17),
				session_id INTEGER,
				session_token VARCHAR(8),
				created_at TIMESTAMPTZ DEFAULT NOW()
			)`,
		},
		{
			"vouchers indexes",
			`CREATE INDEX IF NOT EXISTS idx_vouchers_status ON vouchers(status);
			 CREATE INDEX IF NOT EXISTS idx_vouchers_created_at ON vouchers(created_at DESC)`,
		},
		{
			// v1.18.1 — voucher price tracking (migrations.sql 017)
			"vouchers.price",
			`ALTER TABLE vouchers ADD COLUMN IF NOT EXISTS price NUMERIC(10,2) NOT NULL DEFAULT 0`,
		},
		{
			// v1.19.0 — generation batch code for print runs (migrations.sql 018)
			"vouchers.batch_code",
			`ALTER TABLE vouchers ADD COLUMN IF NOT EXISTS batch_code VARCHAR(20) NOT NULL DEFAULT '';
			 CREATE INDEX IF NOT EXISTS idx_vouchers_batch ON vouchers(batch_code)`,
		},
		{
			// v1.21.0 — voucher pause rules: pausable + pause expiry window
			// (mirrors the pricing rules; applied at first redemption only)
			"vouchers.pausable",
			`ALTER TABLE vouchers ADD COLUMN IF NOT EXISTS pausable BOOLEAN NOT NULL DEFAULT TRUE;
			 ALTER TABLE vouchers ADD COLUMN IF NOT EXISTS expiration_hours INTEGER NOT NULL DEFAULT 0`,
		},
		{
			// v1.20.0 — pricing: pausable vs consumable rates + pause expiry window
			"pricing.pausable",
			`ALTER TABLE pricing ADD COLUMN IF NOT EXISTS pausable BOOLEAN NOT NULL DEFAULT TRUE`,
		},
		{
			"pricing.expiration_hours",
			`ALTER TABLE pricing ADD COLUMN IF NOT EXISTS expiration_hours INTEGER NOT NULL DEFAULT 0`,
		},
		{
			// sessions snapshot of the purchased rate's pause behaviour
			"sessions.pausable",
			`ALTER TABLE sessions ADD COLUMN IF NOT EXISTS pausable BOOLEAN NOT NULL DEFAULT TRUE`,
		},
		{
			"sessions.expiration_hours",
			`ALTER TABLE sessions ADD COLUMN IF NOT EXISTS expiration_hours INTEGER NOT NULL DEFAULT 0`,
		},
		{
			// absolute wall-clock deadline a paused session must be resumed by
			"sessions.pause_expires_at",
			`ALTER TABLE sessions ADD COLUMN IF NOT EXISTS pause_expires_at TIMESTAMPTZ`,
		},
		{
			// Sub-Vendo (NodeMCU) support was removed. Drop the tables for
			// existing installs that still carry them; never created on fresh.
			"drop sub_vendos + ssid_vlan_map",
			`DROP TABLE IF EXISTS ssid_vlan_map;
			 DROP TABLE IF EXISTS sub_vendos`,
		},
		{
			// coin_events origin: single source is now the local GPIO listener
			"coin_events.source",
			`ALTER TABLE coin_events ADD COLUMN IF NOT EXISTS source TEXT NOT NULL DEFAULT 'local_gpio';
			 CREATE INDEX IF NOT EXISTS idx_coin_events_source ON coin_events(source, processed)`,
		},
		{
			// v1.29.0 — relay / light pin that blinks while the coin slot is
			// armed (mirrors migrations.sql 022; OTA updates skip that file)
			"gpio_config.relay",
			`ALTER TABLE gpio_config ADD COLUMN IF NOT EXISTS relay_enabled BOOLEAN NOT NULL DEFAULT false;
			 ALTER TABLE gpio_config ADD COLUMN IF NOT EXISTS relay_pin INTEGER NOT NULL DEFAULT 5;
			 ALTER TABLE gpio_config ADD COLUMN IF NOT EXISTS relay_intensity INTEGER NOT NULL DEFAULT 5`,
		},
		{
			// Wi-Fi hotspot (hostapd) configuration. One AP per wireless
			// interface; security is always OPEN (captive portal entry).
			"wifi_ap_config table",
			`CREATE TABLE IF NOT EXISTS wifi_ap_config (
				id SERIAL PRIMARY KEY,
				interface VARCHAR(32) UNIQUE NOT NULL,
				ssid VARCHAR(32) NOT NULL,
				channel INTEGER NOT NULL DEFAULT 6,
				hw_mode VARCHAR(4) NOT NULL DEFAULT 'g',
				country_code VARCHAR(4) NOT NULL DEFAULT 'PH',
				enabled BOOLEAN NOT NULL DEFAULT FALSE,
				created_at TIMESTAMPTZ DEFAULT NOW(),
				updated_at TIMESTAMPTZ DEFAULT NOW()
			)`,
		},
	}

	for _, f := range fixes {
		if _, err := DB.Exec(f.sql); err != nil {
			log.Printf("schema fix %q failed: %v", f.name, err)
		}
	}
}

// ============================================
// MODELS
// ============================================

type AdminUser struct {
	ID           int        `json:"id"`
	Username     string     `json:"username"`
	PasswordHash string     `json:"-"`
	CreatedAt    time.Time  `json:"created_at"`
	LastLogin    *time.Time `json:"last_login,omitempty"`
}

type SystemSetting struct {
	ID          int       `json:"id"`
	Key         string    `json:"key"`
	Value       string    `json:"value"`
	Description string    `json:"description,omitempty"`
	UpdatedAt   time.Time `json:"updated_at"`
}

type GPIOConfig struct {
	ID              int       `json:"id"`
	Pin             int       `json:"pin"`
	CoinValue       int       `json:"coin_value"`
	PulseMode       string    `json:"pulse_mode"`
	BoardModel      string    `json:"board_model"`
	DebounceMs      int       `json:"debounce_ms"`
	RelayEnabled    bool      `json:"relay_enabled"`
	RelayPin        int       `json:"relay_pin"`
	RelayIntensity  int       `json:"relay_intensity"`
	UpdatedAt       time.Time `json:"updated_at"`
}

type Pricing struct {
	ID              int       `json:"id"`
	CoinValue       int       `json:"coin_value"`
	Minutes         int       `json:"minutes"`
	Active          bool      `json:"active"`
	Pausable        bool      `json:"pausable"`          // false = consumable (no Pause button in portal)
	ExpirationHours int       `json:"expiration_hours"` // max wall-clock pause window before forced expiry (0 = frozen forever)
	CreatedAt       time.Time `json:"created_at"`
	UpdatedAt       time.Time `json:"updated_at"`
}

type CoinEvent struct {
	ID         int       `json:"id"`
	CoinValue  int       `json:"coin_value"`
	DetectedAt time.Time `json:"detected_at"`
	Processed  bool      `json:"processed"`
	SessionID  *int      `json:"session_id,omitempty"`
}

type Session struct {
	ID                      int        `json:"id"`
	ClientIP                string     `json:"client_ip,omitempty"`
	ClientMAC               string     `json:"client_mac,omitempty"`
	CoinsInserted           int        `json:"coins_inserted"`
	TotalCoinsLifetime      int        `json:"total_coins_lifetime"`
	TotalSeconds            int        `json:"total_seconds"`
	RemainingSeconds        int        `json:"remaining_seconds"`
	Status                  string     `json:"status"`
	StartedAt               time.Time  `json:"started_at"`
	ActivatedAt             *time.Time `json:"activated_at,omitempty"`
	ExpiredAt               *time.Time `json:"expired_at,omitempty"`
	ExpiresAt               *time.Time `json:"expires_at,omitempty"`
	PausedAt                *time.Time `json:"paused_at,omitempty"`
	RemainingSecondsAtPause *int       `json:"remaining_seconds_at_pause,omitempty"`
	PauseCount              int        `json:"pause_count"`
	Pausable                bool       `json:"pausable"`                 // false = consumable (no Pause button in portal)
	ExpirationHours         int        `json:"expiration_hours"`         // max wall-clock pause window (0 = frozen while paused)
	PauseExpiresAt          *time.Time `json:"pause_expires_at,omitempty"` // absolute deadline a paused session must be resumed by
	ShapedMbps              *int       `json:"shaped_mbps,omitempty"`
	SessionToken            string     `json:"session_token,omitempty"`
	QdiscInfo               *QdiscInfo `json:"qdisc_info,omitempty"`
	Hostname                string     `json:"hostname,omitempty"`
	CreatedAt               time.Time  `json:"created_at"`
}

// QdiscInfo is returned alongside each admin session row so the UI knows
// whether per-device FQ_CODEL shaping is active and what the global default is.
type QdiscInfo struct {
	Type          string `json:"type"`            // "fq_codel" | "cake" | ""
	PerDeviceMbps int    `json:"per_device_mbps"` // global per-device rate (0 = not active)
}

// Voucher is a pre-paid time code (6-char alphanumeric). plan is 'time'
// or 'monthly' (30-day subscription); status is 'unused', 'used' or
// 'disabled'. On redemption the voucher is bound to the session and its
// roaming session_token.
type Voucher struct {
	ID              int        `json:"id"`
	Code            string     `json:"code"`
	BatchCode       string     `json:"batch_code"`
	DurationMinutes int        `json:"duration_minutes"`
	Plan            string     `json:"plan"`
	Status          string     `json:"status"`
	Price           float64    `json:"price"`
	Notes           string     `json:"notes"`
	Pausable        *bool      `json:"pausable,omitempty"`   // nil = legacy row, treated as pausable
	ExpirationHours int        `json:"expiration_hours"`     // pause deadline after first use (0 = none)
	RedeemedAt      *time.Time `json:"redeemed_at,omitempty"`
	RedeemedMAC     string     `json:"redeemed_mac,omitempty"`
	SessionID       *int       `json:"session_id,omitempty"`
	SessionToken    string     `json:"session_token,omitempty"`
	CreatedAt       time.Time  `json:"created_at"`
}

type SystemLog struct {
	ID        int       `json:"id"`
	Level     string    `json:"level"`
	Component string    `json:"component,omitempty"`
	Message   string    `json:"message"`
	Metadata  string    `json:"metadata,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

type DailyStats struct {
	ID            int       `json:"id"`
	Date          string    `json:"date"`
	TotalEarnings int       `json:"total_earnings"`
	TotalCoins    int       `json:"total_coins"`
	TotalSessions int       `json:"total_sessions"`
	ActiveUsers   int       `json:"active_users"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
}

// ============================================
// VLAN MODELS
// ============================================
// A VLAN is network identity only (parent iface + 802.1Q id + name).
// The hotspot stack lives in the portal models below.

type VLANRequest struct {
	Interface   string `json:"interface"`
	VLANID      int    `json:"vlan_id"`
	Description string `json:"description"`
}

type VLANInfo struct {
	Interface     string `json:"interface"`
	VLANID        int    `json:"vlan_id"`
	Name          string `json:"name"`
	IP            string `json:"ip"` // live IP, informational (owned by a portal server, if any)
	Description   string `json:"description"`
	Active        bool   `json:"active"`
	HasPortal     bool   `json:"has_portal"`
	PortalEnabled bool   `json:"portal_enabled"`
}

type InterfaceInfo struct {
	Name  string `json:"name"`
	IP    string `json:"ip"`
	State string `json:"state"`
}

// ============================================
// BRIDGE MODELS
// ============================================
// A bridge groups multiple interfaces (VLANs, physical ports) into a
// single L2 broadcast domain. The portal stack (portal_servers) can be
// provisioned on a bridge just like on a VLAN interface.

type BridgeConfig struct {
	ID          int       `json:"id"`
	Name        string    `json:"name"`
	Description string    `json:"description"`
	CreatedAt   time.Time `json:"created_at"`
}

type BridgeInfo struct {
	Name          string   `json:"name"`
	Description   string   `json:"description"`
	Members       []string `json:"members"`
	IPCIDR        string   `json:"ip_cidr,omitempty"`
	HasPortal     bool     `json:"has_portal"`
	PortalEnabled bool     `json:"portal_enabled"`
	Active        bool     `json:"active"`
}

type BridgeRequest struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

type BridgeMemberRequest struct {
	BridgeName  string `json:"bridge_name"`
	MemberIface string `json:"member_iface"`
}

// ============================================
// WIFI AP (HOSTAPD) MODELS
// ============================================
// A wireless adapter can be turned into an open access point (hotspot)
// managed by hostapd. The hotspot is intentionally passwordless — it is
// a captive-portal entry interface; the portal stack (portal_servers,
// dnsmasq) is provisioned on it separately, exactly like a VLAN or bridge.

// WiFiAPConfig is the persisted hotspot configuration for one interface.
// Security is always OPEN (no WPA) by design.
type WiFiAPConfig struct {
	Interface   string `json:"interface"`
	SSID        string `json:"ssid"`
	Channel     int    `json:"channel"`
	HwMode      string `json:"hw_mode"`
	CountryCode string `json:"country_code"`
	Enabled     bool   `json:"enabled"`
}

// WiFiAPRequest is the save payload from the admin UI.
type WiFiAPRequest struct {
	Interface   string `json:"interface"`
	SSID        string `json:"ssid"`
	Channel     int    `json:"channel"`
	CountryCode string `json:"country_code,omitempty"`
}

// WiFiChannel describes one channel an adapter supports.
type WiFiChannel struct {
	Channel int    `json:"channel"`
	FreqMHz int    `json:"freq_mhz"`
	Band    string `json:"band"` // "2.4GHz" or "5GHz"
	Active  bool   `json:"active"`
}

// WiFiAdapter describes a detected wireless interface and its capabilities.
type WiFiAdapter struct {
	Name       string        `json:"name"`
	SupportsAP bool          `json:"supports_ap"`
	Active     bool          `json:"active"`
	Channels   []WiFiChannel `json:"channels"`
}

// ============================================
// PORTAL SERVER MODELS
// ============================================

type PortalRequest struct {
	Interface   string `json:"interface"`
	IPCIDR      string `json:"ip_cidr"`
	DHCPStart   string `json:"dhcp_start"`
	DHCPEnd     string `json:"dhcp_end"`
	DHCPLease   string `json:"dhcp_lease"`
	AntiHotspot bool   `json:"anti_hotspot"`
}

type PortalInfo struct {
	Interface     string `json:"interface"`
	IPCIDR        string `json:"ip_cidr"`
	DHCPStart     string `json:"dhcp_start"`
	DHCPEnd       string `json:"dhcp_end"`
	DHCPLease     string `json:"dhcp_lease"`
	Enabled       bool   `json:"enabled"`
	AntiHotspot   bool   `json:"anti_hotspot"`
	IfaceExists   bool   `json:"iface_exists"`
	IfaceUp       bool   `json:"iface_up"`
	IPOK          bool   `json:"ip_ok"`
	DHCPActive    bool   `json:"dhcp_active"`
	CaptiveActive bool   `json:"captive_active"`
}

// ============================================
// PORTAL APPEARANCE MODELS
// ============================================
// The whole portal look is ONE JSON document stored in system_settings
// under key 'portal_appearance' (seeded by migration 009). The same
// shape is served by GET /api/portal/appearance and accepted by
// POST /api/admin/portal/appearance.

type PortalColors struct {
	Primary    string `json:"primary"`
	Accent     string `json:"accent"`
	Background string `json:"background"`
	Card       string `json:"card"`
	Text       string `json:"text"`
	Button     string `json:"button"`
	ButtonText string `json:"button_text"`
}

type PortalAppearance struct {
	Theme           string       `json:"theme"`
	Colors          PortalColors `json:"colors"`
	BackgroundImage string       `json:"background_image"`
	HeaderImage     string       `json:"header_image,omitempty"`
	// RedirectURL is optional: when set, the portal offers to send the
	// client to this link right after a successful coin payment / session
	// start. Empty = no post-payment redirect.
	RedirectURL string `json:"redirect_url,omitempty"`
	UpdatedAt   string `json:"updated_at,omitempty"`
}

// ============================================
// REQUEST/RESPONSE TYPES
// ============================================

type LoginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

type LoginResponse struct {
	Success bool   `json:"success"`
	Token   string `json:"token,omitempty"`
	Message string `json:"message,omitempty"`
}

type ChangePasswordRequest struct {
	CurrentPassword string `json:"current_password"`
	NewPassword     string `json:"new_password"`
}

type StatsGroup struct {
	Earnings float64 `json:"earnings"`
	Coins    int     `json:"coins"`
	Sessions int     `json:"sessions"`
}

type WeeklyStat struct {
	Date     string `json:"date"`
	Earnings int    `json:"earnings"`
	Coins    int    `json:"coins"`
	Sessions int    `json:"sessions"`
}

type StatsResponse struct {
	Today          StatsGroup   `json:"today"`
	Total          StatsGroup   `json:"total"`
	ActiveSessions int          `json:"active_sessions"`
	SystemOnline   bool         `json:"system_online"`
	WeeklyStats    []WeeklyStat `json:"weekly_stats,omitempty"`
}

type SessionStatusResponse struct {
	HasActive        bool     `json:"has_active"`
	Session          *Session `json:"session,omitempty"`
	RemainingSeconds int      `json:"remaining_seconds"`
	CoinsInserted    int      `json:"coins_inserted"`
}

type GPIOConfigRequest struct {
	Pin            int    `json:"pin"`
	CoinValue      int    `json:"coin_value"`
	PulseMode      string `json:"pulse_mode"`
	BoardModel     string `json:"board_model"`
	DebounceMs     int    `json:"debounce_ms"`
	RelayEnabled   bool   `json:"relay_enabled"`
	RelayPin       int    `json:"relay_pin"`
	RelayIntensity int    `json:"relay_intensity"`
}

type CoinEventRequest struct {
	CoinValue int `json:"coin_value"`
}

type PricingRequest struct {
	CoinValue       int   `json:"coin_value"`
	Minutes         int   `json:"minutes"`
	Active          *bool `json:"active,omitempty"`
	Pausable        *bool `json:"pausable,omitempty"`
	ExpirationHours int   `json:"expiration_hours,omitempty"`
}

type SettingsRequest struct {
	Settings map[string]string `json:"settings"`
}

// ============================================
// WAN CONFIG MODELS
// ============================================

type WANStaticConfig struct {
	IP      string `json:"ip"`
	Subnet  string `json:"subnet"`
	Gateway string `json:"gateway"`
	DNS1    string `json:"dns1"`
	DNS2    string `json:"dns2"`
}

type WANConfig struct {
	Mode         string           `json:"mode"` // "dhcp", "static", "vlan_dhcp"
	StaticConfig *WANStaticConfig `json:"static_config,omitempty"`
	VLANID       *int             `json:"vlan_id,omitempty"`
}

type WANRequest struct {
	Mode         string           `json:"mode"`
	StaticConfig *WANStaticConfig `json:"static_config,omitempty"`
	VLANID       *int             `json:"vlan_id,omitempty"`
	ApplyToOS    bool             `json:"apply_to_os"`
}

type WANAvailableVLAN struct {
	VLANID int    `json:"vlan_id"`
	Iface  string `json:"iface"`
}

type APIResponse struct {
	Success bool        `json:"success"`
	Message string      `json:"message,omitempty"`
	Data    interface{} `json:"data,omitempty"`
}

type PaginatedResponse struct {
	Success bool        `json:"success"`
	Data    interface{} `json:"data"`
	Total   int         `json:"total"`
	Limit   int         `json:"limit"`
	Offset  int         `json:"offset"`
}
