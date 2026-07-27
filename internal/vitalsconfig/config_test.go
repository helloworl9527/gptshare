package vitalsconfig

import (
	"encoding/base32"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLoadBadKeyConfigurationMatrix(t *testing.T) {
	monitorMaterial := strings.Repeat("m", 32)
	base := validEnv()
	tests := map[string]func(map[string]string){
		"missing monitor key":    func(env map[string]string) { delete(env, "CREDENTIAL_MASTER_KEYS") },
		"missing allocation key": func(env map[string]string) { delete(env, "ALLOCATION_CREDENTIAL_MASTER_KEYS") },
		"same key material": func(env map[string]string) {
			env["ALLOCATION_CREDENTIAL_MASTER_KEYS"] = "allocation-2026:" + base64.RawURLEncoding.EncodeToString([]byte(monitorMaterial))
		},
		"allocation reads phase one name": func(env map[string]string) { delete(env, "ALLOCATION_CREDENTIAL_MASTER_KEYS") },
		"example monitor key id": func(env map[string]string) {
			env["CREDENTIAL_MASTER_KEYS"] = "example:" + base64.StdEncoding.EncodeToString([]byte(monitorMaterial))
			env["CREDENTIAL_ACTIVE_KEY_ID"] = "example"
		},
		"weak allocation key": func(env map[string]string) {
			env["ALLOCATION_CREDENTIAL_MASTER_KEYS"] = "allocation-2026:" + base64.RawURLEncoding.EncodeToString([]byte("weak"))
		},
		"active key missing":          func(env map[string]string) { env["ALLOCATION_CREDENTIAL_ACTIVE_KEY_ID"] = "allocation-missing" },
		"missing unified auth key":    func(env map[string]string) { delete(env, "JWT_SIGNING_KEY") },
		"missing admin password hash": func(env map[string]string) { delete(env, "ADMIN_PASSWORD_HASH") },
		"malformed admin totp":        func(env map[string]string) { env["ADMIN_TOTP_SECRET"] = "not-base32!" },
		"auth keys reuse each other":  func(env map[string]string) { env["RATE_LIMIT_KEY"] = env["JWT_SIGNING_KEY"] },
		"auth reuses monitor data key": func(env map[string]string) {
			env["JWT_SIGNING_KEY"] = base64.StdEncoding.EncodeToString([]byte(monitorMaterial))
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			env := clone(base)
			mutate(env)
			if _, err := Load(mapLookup(env)); err == nil {
				t.Fatal("expected configuration rejection")
			}
		})
	}
}

func TestLoadRejectsDatabasePathAlias(t *testing.T) {
	dir := t.TempDir()
	realDir := filepath.Join(dir, "real")
	if err := os.Mkdir(realDir, 0o700); err != nil {
		t.Fatal(err)
	}
	aliasDir := filepath.Join(dir, "alias")
	if err := os.Symlink(realDir, aliasDir); err != nil {
		t.Fatal(err)
	}
	env := validEnv()
	env["MONITOR_DB_PATH"] = filepath.Join(realDir, "shared.db")
	env["ALLOCATION_DB_PATH"] = filepath.Join(aliasDir, "shared.db")
	if _, err := Load(mapLookup(env)); err == nil {
		t.Fatal("expected database path aliases to fail closed")
	}
}

func TestLoadDefaultsCompatHTTPToDisabledWithoutAPIKey(t *testing.T) {
	env := validEnv()
	delete(env, "ALLOCATION_SERVICE_API_KEY")
	cfg, err := Load(mapLookup(env))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.CompatHTTP.Enabled {
		t.Fatal("compatibility HTTP must default to disabled")
	}
	if cfg.AdminSessionTTL != 30*24*time.Hour {
		t.Fatalf("admin session TTL = %s, want 720h", cfg.AdminSessionTTL)
	}
}

func TestLoadAdminSessionTTL(t *testing.T) {
	env := validEnv()
	env["ADMIN_SESSION_TTL"] = "336h"
	cfg, err := Load(mapLookup(env))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.AdminSessionTTL != 14*24*time.Hour {
		t.Fatalf("admin session TTL = %s, want 336h", cfg.AdminSessionTTL)
	}
	for _, invalid := range []string{"not-a-duration", "30m", "2161h"} {
		env["ADMIN_SESSION_TTL"] = invalid
		if _, err := Load(mapLookup(env)); err == nil {
			t.Fatalf("expected ADMIN_SESSION_TTL=%q to be rejected", invalid)
		}
	}
}

func TestLoadPublicOriginRequiresExplicitOptIn(t *testing.T) {
	env := validEnv()
	env["APP_ORIGIN"] = "https://gpt.example.com"
	if _, err := Load(mapLookup(env)); err == nil {
		t.Fatal("expected public origin without explicit opt-in to fail")
	}
	env["VITALS_ALLOW_PUBLIC_APP_ORIGIN"] = "true"
	if _, err := Load(mapLookup(env)); err != nil {
		t.Fatal(err)
	}
}

func TestLoadCompatHTTPRequiresCompleteTemporaryException(t *testing.T) {
	env := validEnv()
	env["VITALS_MONITOR_COMPAT_HTTP_ENABLED"] = "true"
	if _, err := Load(mapLookup(env)); err == nil {
		t.Fatal("expected missing exception metadata to fail")
	}
	env["ALLOCATION_SERVICE_API_KEY"] = strings.Repeat("k", 32)
	env["VITALS_MONITOR_COMPAT_CONSUMER"] = "migration-client"
	env["VITALS_MONITOR_COMPAT_SHUTDOWN_TASK"] = "OPS-42"
	env["VITALS_MONITOR_COMPAT_EXPIRES_AT"] = time.Now().UTC().Add(time.Hour).Format(time.RFC3339)
	if _, err := Load(mapLookup(env)); err != nil {
		t.Fatal(err)
	}
}

func validEnv() map[string]string {
	return map[string]string{
		"CREDENTIAL_MASTER_KEYS":              "monitor-2026:" + base64.StdEncoding.EncodeToString([]byte(strings.Repeat("m", 32))),
		"CREDENTIAL_ACTIVE_KEY_ID":            "monitor-2026",
		"ALLOCATION_CREDENTIAL_MASTER_KEYS":   "allocation-2026:" + base64.RawURLEncoding.EncodeToString([]byte(strings.Repeat("a", 32))),
		"ALLOCATION_CREDENTIAL_ACTIVE_KEY_ID": "allocation-2026",
		"ADMIN_USER":                          "vitals-admin",
		"ADMIN_PASSWORD_HASH":                 "$2a$12$hiWAthhsWksgoB45i53TV.MxRmOiYgRZ6gbjx7qQdHVCp.A4Ka1Wi",
		"ADMIN_TOTP_SECRET":                   base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString([]byte(strings.Repeat("t", 20))),
		"JWT_SIGNING_KEY":                     base64.StdEncoding.EncodeToString([]byte(strings.Repeat("j", 32))),
		"RATE_LIMIT_KEY":                      base64.StdEncoding.EncodeToString([]byte(strings.Repeat("r", 32))),
		"APP_ORIGIN":                          "https://127.0.0.1:8080",
	}
}

func clone(source map[string]string) map[string]string {
	result := make(map[string]string, len(source))
	for key, value := range source {
		result[key] = value
	}
	return result
}

func mapLookup(values map[string]string) func(string) (string, bool) {
	return func(name string) (string, bool) {
		value, ok := values[name]
		return value, ok
	}
}
