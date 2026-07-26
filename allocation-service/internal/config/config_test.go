package config

import (
	"encoding/base32"
	"encoding/base64"
	"strings"
	"testing"
)

func TestLoadValidConfiguration(t *testing.T) {
	cfg, err := LoadWithLookup(lookupFrom(validValues()))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ListenAddr != "127.0.0.1:9090" {
		t.Fatalf("listen addr=%q", cfg.ListenAddr)
	}
	if cfg.MonitorBaseURL != "http://127.0.0.1:8080" {
		t.Fatalf("monitor base URL=%q", cfg.MonitorBaseURL)
	}
	if cfg.AppOrigin != "https://127.0.0.1:9090" {
		t.Fatalf("app origin=%q", cfg.AppOrigin)
	}
	if len(cfg.CredentialMasterKeys["alloc-primary"]) != 32 {
		t.Fatal("allocation credential master key not decoded")
	}
}

func TestRejectsBadSecrets(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(map[string]string)
	}{
		{"empty monitor API key", func(v map[string]string) { delete(v, "ALLOCATION_MONITOR_API_KEY") }},
		{"example monitor API key", func(v map[string]string) { v["ALLOCATION_MONITOR_API_KEY"] = "change-me" }},
		{"short monitor API key", func(v map[string]string) { v["ALLOCATION_MONITOR_API_KEY"] = "short" }},
		{"example admin password hash", func(v map[string]string) {
			v["ALLOCATION_ADMIN_PASSWORD_HASH"] = "__REPLACE_WITH_BCRYPT_COST_12_OR_HIGHER__"
		}},
		{"weak bcrypt cost", func(v map[string]string) {
			v["ALLOCATION_ADMIN_PASSWORD_HASH"] = "$2b$10$01234567890123456789012345678901234567890123456789012"
		}},
		{"empty TOTP secret", func(v map[string]string) { delete(v, "ALLOCATION_ADMIN_TOTP_SECRET") }},
		{"example TOTP secret", func(v map[string]string) { v["ALLOCATION_ADMIN_TOTP_SECRET"] = "__REPLACE_WITH_BASE32_SECRET__" }},
		{"short session key", func(v map[string]string) {
			v["ALLOCATION_SESSION_SIGNING_KEY"] = base64.StdEncoding.EncodeToString([]byte("short"))
		}},
		{"duplicate allocation material", func(v map[string]string) { v["ALLOCATION_CSRF_SIGNING_KEY"] = v["ALLOCATION_SESSION_SIGNING_KEY"] }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			values := validValues()
			tt.mutate(values)
			if _, err := LoadWithLookup(lookupFrom(values)); err == nil {
				t.Fatal("invalid configuration unexpectedly accepted")
			}
		})
	}
}

func TestRejectsBadCredentialMasterKeys(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(map[string]string)
	}{
		{"empty", func(v map[string]string) { delete(v, "ALLOCATION_CREDENTIAL_MASTER_KEYS") }},
		{"example", func(v map[string]string) {
			v["ALLOCATION_CREDENTIAL_MASTER_KEYS"] = "__REPLACE_WITH_ALLOCATION_CREDENTIAL_KEYS__"
		}},
		{"weak length", func(v map[string]string) {
			v["ALLOCATION_CREDENTIAL_MASTER_KEYS"] = "alloc-primary:" + base64.RawURLEncoding.EncodeToString([]byte(strings.Repeat("x", 16)))
		}},
		{"standard base64 rejected", func(v map[string]string) {
			v["ALLOCATION_CREDENTIAL_MASTER_KEYS"] = "alloc-primary:" + base64.StdEncoding.EncodeToString([]byte(strings.Repeat("x", 32)))
		}},
		{"active missing", func(v map[string]string) { v["ALLOCATION_CREDENTIAL_ACTIVE_KEY_ID"] = "missing" }},
		{"duplicate key id", func(v map[string]string) {
			v["ALLOCATION_CREDENTIAL_MASTER_KEYS"] = v["ALLOCATION_CREDENTIAL_MASTER_KEYS"] + "," + v["ALLOCATION_CREDENTIAL_MASTER_KEYS"]
		}},
		{"phase one key id", func(v map[string]string) { v["ALLOCATION_CREDENTIAL_MASTER_KEYS"] = "active:" + keyURL('x') }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			values := validValues()
			tt.mutate(values)
			if _, err := LoadWithLookup(lookupFrom(values)); err == nil {
				t.Fatal("invalid credential key configuration unexpectedly accepted")
			}
		})
	}
}

func TestRejectsPhaseOneSecretNamesAndMaterials(t *testing.T) {
	for _, name := range phaseOneSecretEnvNames {
		t.Run("phase one name "+name, func(t *testing.T) {
			values := validValues()
			values[name] = "phase-one-secret-present"
			if _, err := LoadWithLookup(lookupFrom(values)); err == nil || !strings.Contains(err.Error(), "phase one secret environment variable") {
				t.Fatalf("error=%v", err)
			}
		})
	}
	t.Run("phase one material reuse", func(t *testing.T) {
		values := validValues()
		values["CREDENTIAL_MASTER_KEYS"] = "phase1:" + base64.StdEncoding.EncodeToString([]byte(strings.Repeat("m", 32)))
		values["ALLOCATION_CREDENTIAL_MASTER_KEYS"] = "alloc-primary:" + base64.RawURLEncoding.EncodeToString([]byte(strings.Repeat("m", 32)))
		if err := rejectPhaseOneMaterialReuse(lookupFrom(values), [][]byte{[]byte(strings.Repeat("m", 32))}); err == nil {
			t.Fatal("phase one material reuse unexpectedly accepted")
		}
	})
}

func TestRejectsNonLoopbackListenAddressAndBadMonitorURL(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(map[string]string)
	}{
		{"non loopback listen", func(v map[string]string) { v["ALLOCATION_PORT"] = "0.0.0.0:9090" }},
		{"monitor URL path", func(v map[string]string) { v["ALLOCATION_MONITOR_BASE_URL"] = "https://monitor.example.test/api" }},
		{"remote http monitor URL", func(v map[string]string) { v["ALLOCATION_MONITOR_BASE_URL"] = "http://monitor.example.test" }},
		{"http app origin", func(v map[string]string) { v["ALLOCATION_APP_ORIGIN"] = "http://127.0.0.1:9090" }},
		{"remote app origin", func(v map[string]string) { v["ALLOCATION_APP_ORIGIN"] = "https://admin.example.test" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			values := validValues()
			tt.mutate(values)
			if _, err := LoadWithLookup(lookupFrom(values)); err == nil {
				t.Fatal("invalid network configuration unexpectedly accepted")
			}
		})
	}
}

func validValues() map[string]string {
	return map[string]string{
		"ALLOCATION_ENV":                      "development",
		"ALLOCATION_PORT":                     "9090",
		"ALLOCATION_DB_PATH":                  "data/test-allocation.db",
		"ALLOCATION_APP_ORIGIN":               "https://127.0.0.1:9090",
		"ALLOCATION_MONITOR_BASE_URL":         "http://127.0.0.1:8080",
		"ALLOCATION_MONITOR_API_KEY":          strings.Repeat("k", 32),
		"ALLOCATION_ADMIN_USER":               "admin",
		"ALLOCATION_ADMIN_PASSWORD_HASH":      "$2b$12$01234567890123456789012345678901234567890123456789012",
		"ALLOCATION_ADMIN_TOTP_SECRET":        base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString([]byte(strings.Repeat("t", 20))),
		"ALLOCATION_SESSION_SIGNING_KEY":      key64('s'),
		"ALLOCATION_CSRF_SIGNING_KEY":         key64('c'),
		"ALLOCATION_CREDENTIAL_MASTER_KEYS":   "alloc-primary:" + keyURL('m') + ",alloc-previous:" + keyURL('n'),
		"ALLOCATION_CREDENTIAL_ACTIVE_KEY_ID": "alloc-primary",
	}
}

func key64(fill byte) string {
	return base64.StdEncoding.EncodeToString([]byte(strings.Repeat(string(fill), 32)))
}

func keyURL(fill byte) string {
	return base64.RawURLEncoding.EncodeToString([]byte(strings.Repeat(string(fill), 32)))
}

func lookupFrom(values map[string]string) func(string) (string, bool) {
	return func(name string) (string, bool) {
		value, ok := values[name]
		return value, ok
	}
}
