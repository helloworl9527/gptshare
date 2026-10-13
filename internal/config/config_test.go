package config

import (
	"encoding/base32"
	"encoding/base64"
	"strings"
	"testing"
	"time"
)

func TestLoadValidConfiguration(t *testing.T) {
	values := validValues()
	cfg, err := load(func(name string) (string, bool) { value, ok := values[name]; return value, ok })
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ListenAddr != "127.0.0.1:8080" {
		t.Fatalf("listen address=%q", cfg.ListenAddr)
	}
	if cfg.AppOrigin != "https://127.0.0.1:8080" {
		t.Fatalf("app origin=%q", cfg.AppOrigin)
	}
	if len(cfg.CredentialMasterKeys["active"]) != 32 {
		t.Fatal("credential master key not decoded")
	}
	if cfg.AllocationServiceAPIKey == "" {
		t.Fatal("allocation API key not loaded")
	}
}

func TestLoadRefreshSchedulingLegacyDefaultsAndBounds(t *testing.T) {
	values := validValues()
	cfg, err := load(func(name string) (string, bool) { value, ok := values[name]; return value, ok })
	if err != nil {
		t.Fatal(err)
	}
	if cfg.RefreshScanEvery != time.Hour || cfg.RefreshLead != 24*time.Hour || cfg.RefreshMaxLatency != 5*time.Minute {
		t.Fatalf("defaults = %s/%s/%s, want 1h/24h/5m", cfg.RefreshScanEvery, cfg.RefreshLead, cfg.RefreshMaxLatency)
	}
	values["VITALS_MONITOR_REFRESH_SCAN_EVERY"] = "20m"
	values["VITALS_MONITOR_REFRESH_LEAD"] = "36h"
	cfg, err = load(func(name string) (string, bool) { value, ok := values[name]; return value, ok })
	if err != nil {
		t.Fatal(err)
	}
	if cfg.RefreshScanEvery != 20*time.Minute || cfg.RefreshLead != 36*time.Hour {
		t.Fatalf("overrides = %s/%s", cfg.RefreshScanEvery, cfg.RefreshLead)
	}
	values["VITALS_MONITOR_REFRESH_SCAN_EVERY"] = "not-a-duration"
	if _, err := load(func(name string) (string, bool) { value, ok := values[name]; return value, ok }); err == nil {
		t.Fatal("expected invalid refresh duration to be rejected")
	}
}

func TestRejectsNonLoopbackBindingInDevelopmentAndProduction(t *testing.T) {
	for _, environment := range []string{"development", "production"} {
		t.Run(environment, func(t *testing.T) {
			values := validValues()
			values["APP_ENV"] = environment
			values["PORT"] = "0.0.0.0:8080"
			if _, err := load(func(name string) (string, bool) { value, ok := values[name]; return value, ok }); err == nil || !strings.Contains(err.Error(), "loopback") {
				t.Fatalf("error=%v", err)
			}
		})
	}
}

func TestRejectsMissingReusedAndMalformedKeys(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(map[string]string)
	}{
		{"missing active key", func(v map[string]string) { v["CREDENTIAL_ACTIVE_KEY_ID"] = "missing" }},
		{"short JWT key", func(v map[string]string) { v["JWT_SIGNING_KEY"] = base64.StdEncoding.EncodeToString([]byte("short")) }},
		{"oversized credential key", func(v map[string]string) {
			v["CREDENTIAL_MASTER_KEYS"] = "active:" + base64.StdEncoding.EncodeToString([]byte(strings.Repeat("x", 33)))
		}},
		{"reused responsibility key", func(v map[string]string) { v["RATE_LIMIT_KEY"] = v["JWT_SIGNING_KEY"] }},
		{"missing allocation API key", func(v map[string]string) { delete(v, "ALLOCATION_SERVICE_API_KEY") }},
		{"default allocation API key", func(v map[string]string) { v["ALLOCATION_SERVICE_API_KEY"] = "change-me" }},
		{"short allocation API key", func(v map[string]string) { v["ALLOCATION_SERVICE_API_KEY"] = "short" }},
		{"reused allocation API key", func(v map[string]string) { v["ALLOCATION_SERVICE_API_KEY"] = string([]byte(strings.Repeat("c", 32))) }},
		{"missing account event URL", func(v map[string]string) { delete(v, "ALLOCATION_ACCOUNT_EVENT_URL") }},
		{"insecure remote account event URL", func(v map[string]string) {
			v["ALLOCATION_ACCOUNT_EVENT_URL"] = "http://allocation.example/api/internal/v1/monitor-account-events"
		}},
		{"short account event API key", func(v map[string]string) { v["ALLOCATION_ACCOUNT_EVENT_API_KEY"] = "short" }},
		{"reused account event API key", func(v map[string]string) { v["ALLOCATION_ACCOUNT_EVENT_API_KEY"] = strings.Repeat("z", 32) }},
		{"malformed password hash", func(v map[string]string) { v["ADMIN_PASSWORD_HASH"] = "plaintext" }},
		{"malformed TOTP", func(v map[string]string) { v["ADMIN_TOTP_SECRET"] = "not-base32!" }},
		{"weak bcrypt cost", func(v map[string]string) {
			v["ADMIN_PASSWORD_HASH"] = "$2b$10$01234567890123456789012345678901234567890123456789012"
		}},
		{"http origin", func(v map[string]string) { v["APP_ORIGIN"] = "http://127.0.0.1:8080" }},
		{"partial dev TLS", func(v map[string]string) { v["DEV_TLS_CERT"] = "/tmp/cert.pem" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			values := validValues()
			tt.mutate(values)
			if _, err := load(func(name string) (string, bool) { value, ok := values[name]; return value, ok }); err == nil {
				t.Fatal("invalid configuration unexpectedly accepted")
			}
		})
	}
}

func validValues() map[string]string {
	key := func(fill byte) string {
		return base64.StdEncoding.EncodeToString([]byte(strings.Repeat(string(fill), 32)))
	}
	return map[string]string{
		"APP_ENV":                          "development",
		"PORT":                             "8080",
		"DB_PATH":                          "data/test.db",
		"MIGRATIONS_DIR":                   "migrations",
		"CREDENTIAL_MASTER_KEYS":           "active:" + key('a') + ",old:" + key('b'),
		"CREDENTIAL_ACTIVE_KEY_ID":         "active",
		"JWT_SIGNING_KEY":                  key('c'),
		"RATE_LIMIT_KEY":                   key('d'),
		"ALLOCATION_SERVICE_API_KEY":       strings.Repeat("z", 32),
		"ALLOCATION_ACCOUNT_EVENT_URL":     "http://127.0.0.1:9090/api/internal/v1/monitor-account-events",
		"ALLOCATION_ACCOUNT_EVENT_API_KEY": strings.Repeat("e", 32),
		"ADMIN_USER":                       "admin",
		"ADMIN_PASSWORD_HASH":              "$2b$12$01234567890123456789012345678901234567890123456789012",
		"ADMIN_TOTP_SECRET":                base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString([]byte(strings.Repeat("e", 20))),
		"APP_ORIGIN":                       "https://127.0.0.1:8080",
	}
}
