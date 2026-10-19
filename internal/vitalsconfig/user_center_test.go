package vitalsconfig

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"strings"
	"testing"
)

// testUserCenterSecret 生成确定性的「像随机」测试材料，不是任何真实密钥。
func testUserCenterSecret(label string) string {
	sum := sha256.Sum256([]byte("vitals-user-center-test:" + label))
	return hex.EncodeToString(sum[:])
}

func userCenterEnv() map[string]string {
	env := validEnv()
	env["UC_BIND_TICKET_KEY"] = testUserCenterSecret("ticket")
	env["UC_INTERNAL_TOKEN"] = testUserCenterSecret("token")
	env["UC_DETAIL_ORIGIN"] = "https://gpt.example.test"
	return env
}

func TestLoadUserCenterDisabledWhenAbsentOrEmpty(t *testing.T) {
	for name, env := range map[string]map[string]string{
		"absent": validEnv(),
		"empty": func() map[string]string {
			env := validEnv()
			env["UC_BIND_TICKET_KEY"] = ""
			env["UC_INTERNAL_TOKEN"] = ""
			env["UC_DETAIL_ORIGIN"] = ""
			return env
		}(),
	} {
		t.Run(name, func(t *testing.T) {
			cfg, err := Load(mapLookup(env))
			if err != nil {
				t.Fatalf("missing user center config must not block startup: %v", err)
			}
			if cfg.UserCenter.Enabled || cfg.UserCenter.BindTicketKey != nil || cfg.UserCenter.InternalToken != "" {
				t.Fatalf("user center must stay disabled: %+v", cfg.UserCenter)
			}
		})
	}
}

func TestLoadUserCenterEnabled(t *testing.T) {
	env := userCenterEnv()
	cfg, err := Load(mapLookup(env))
	if err != nil {
		t.Fatal(err)
	}
	wantKey, _ := hex.DecodeString(env["UC_BIND_TICKET_KEY"])
	if !cfg.UserCenter.Enabled || !bytes.Equal(cfg.UserCenter.BindTicketKey, wantKey) || cfg.UserCenter.InternalToken != env["UC_INTERNAL_TOKEN"] || cfg.UserCenter.DetailOrigin != "https://gpt.example.test" {
		t.Fatalf("unexpected user center config: enabled=%v origin=%q", cfg.UserCenter.Enabled, cfg.UserCenter.DetailOrigin)
	}
}

func TestLoadUserCenterRejectsWeakReusedOrPartialSecrets(t *testing.T) {
	strongButRepeating := strings.Repeat("0123456789abcdef", 4)
	tests := map[string]func(map[string]string){
		"only ticket key":           func(env map[string]string) { delete(env, "UC_INTERNAL_TOKEN") },
		"only internal token":       func(env map[string]string) { env["UC_BIND_TICKET_KEY"] = "" },
		"origin without secrets":    func(env map[string]string) { env["UC_BIND_TICKET_KEY"] = ""; env["UC_INTERNAL_TOKEN"] = "" },
		"missing detail origin":     func(env map[string]string) { delete(env, "UC_DETAIL_ORIGIN") },
		"http detail origin":        func(env map[string]string) { env["UC_DETAIL_ORIGIN"] = "http://gpt.example.test" },
		"detail origin with path":   func(env map[string]string) { env["UC_DETAIL_ORIGIN"] = "https://gpt.example.test/redeem" },
		"ticket key not hex":        func(env map[string]string) { env["UC_BIND_TICKET_KEY"] = strings.Repeat("z", 64) },
		"ticket key too short":      func(env map[string]string) { env["UC_BIND_TICKET_KEY"] = testUserCenterSecret("short")[:32] },
		"ticket key too long":       func(env map[string]string) { env["UC_BIND_TICKET_KEY"] = testUserCenterSecret("long") + "00" },
		"ticket key all zero":       func(env map[string]string) { env["UC_BIND_TICKET_KEY"] = strings.Repeat("0", 64) },
		"ticket key repeating hex":  func(env map[string]string) { env["UC_BIND_TICKET_KEY"] = strongButRepeating },
		"token placeholder":         func(env map[string]string) { env["UC_INTERNAL_TOKEN"] = "__REPLACE_WITH_HEX_32_BYTES__" },
		"ticket equals token":       func(env map[string]string) { env["UC_INTERNAL_TOKEN"] = env["UC_BIND_TICKET_KEY"] },
		"ticket equals token upper": func(env map[string]string) { env["UC_INTERNAL_TOKEN"] = strings.ToUpper(env["UC_BIND_TICKET_KEY"]) },
		"token reuses compat api key": func(env map[string]string) {
			env["VITALS_MONITOR_COMPAT_HTTP_ENABLED"] = "true"
			env["ALLOCATION_SERVICE_API_KEY"] = env["UC_INTERNAL_TOKEN"]
			env["VITALS_MONITOR_COMPAT_CONSUMER"] = "rollback-consumer"
			env["VITALS_MONITOR_COMPAT_SHUTDOWN_TASK"] = "remove-compat"
			env["VITALS_MONITOR_COMPAT_EXPIRES_AT"] = "2099-01-01T00:00:00Z"
		},
		"ticket reuses jwt key bytes": func(env map[string]string) {
			key := testUserCenterSecret("shared")
			raw, _ := hex.DecodeString(key)
			env["UC_BIND_TICKET_KEY"] = key
			env["JWT_SIGNING_KEY"] = base64.StdEncoding.EncodeToString(raw)
		},
		"token reuses allocation key bytes": func(env map[string]string) {
			key := testUserCenterSecret("shared-allocation")
			raw, _ := hex.DecodeString(key)
			env["UC_INTERNAL_TOKEN"] = key
			env["ALLOCATION_CREDENTIAL_MASTER_KEYS"] = "allocation-2026:" + base64.RawURLEncoding.EncodeToString(raw)
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			env := userCenterEnv()
			mutate(env)
			if _, err := Load(mapLookup(env)); err == nil {
				t.Fatal("expected configuration rejection")
			}
		})
	}
}

func TestLoadUserCenterAcceptsDistinctCompatAPIKey(t *testing.T) {
	env := userCenterEnv()
	env["VITALS_MONITOR_COMPAT_HTTP_ENABLED"] = "true"
	env["ALLOCATION_SERVICE_API_KEY"] = testUserCenterSecret("compat")
	env["VITALS_MONITOR_COMPAT_CONSUMER"] = "rollback-consumer"
	env["VITALS_MONITOR_COMPAT_SHUTDOWN_TASK"] = "remove-compat"
	env["VITALS_MONITOR_COMPAT_EXPIRES_AT"] = "2099-01-01T00:00:00Z"
	if _, err := Load(mapLookup(env)); err != nil {
		t.Fatalf("distinct compat key must be accepted (control for the reuse case): %v", err)
	}
}
