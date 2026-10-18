package module

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

func openUserCenterTestModule(t *testing.T) *Module {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	opened, err := Open(context.Background(), filepath.Join(dir, "allocation.db"), map[string][]byte{"allocation-2026": []byte(strings.Repeat("a", 32))}, "allocation-2026", emptyMonitor{}, slog.New(slog.NewJSONHandler(&bytes.Buffer{}, nil)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { opened.Close() })
	return opened
}

func internalRoutesMounted(router *gin.Engine) int {
	mounted := 0
	for _, route := range router.Routes() {
		if strings.HasPrefix(route.Path, "/internal/uc/") {
			mounted++
		}
	}
	return mounted
}

func TestUserCenterRoutesMountedOnlyWhenEnabled(t *testing.T) {
	disabled := openUserCenterTestModule(t)
	router := gin.New()
	disabled.RegisterPublicRoutes(router)
	if got := internalRoutesMounted(router); got != 0 {
		t.Fatalf("disabled module mounted %d internal routes", got)
	}

	enabled := openUserCenterTestModule(t)
	if err := enabled.EnableUserCenter(UserCenterConfig{TicketKey: bytes.Repeat([]byte{3}, 32), InternalToken: strings.Repeat("ab", 32), DetailOrigin: "https://gpt.example.test"}); err != nil {
		t.Fatal(err)
	}
	router = gin.New()
	enabled.RegisterPublicRoutes(router)
	want := map[string]bool{
		http.MethodGet + " /internal/uc/events":       false,
		http.MethodPost + " /internal/uc/status":      false,
		http.MethodPost + " /internal/uc/detail-link": false,
	}
	for _, route := range router.Routes() {
		if _, ok := want[route.Method+" "+route.Path]; ok {
			want[route.Method+" "+route.Path] = true
		}
	}
	for route, mounted := range want {
		if !mounted {
			t.Fatalf("%s was not mounted", route)
		}
	}
}

func TestEnableUserCenterRejectsInvalidConfig(t *testing.T) {
	module := openUserCenterTestModule(t)
	for name, cfg := range map[string]UserCenterConfig{
		"short ticket key": {TicketKey: bytes.Repeat([]byte{3}, 16), InternalToken: strings.Repeat("ab", 32), DetailOrigin: "https://gpt.example.test"},
		"short token":      {TicketKey: bytes.Repeat([]byte{3}, 32), InternalToken: "short", DetailOrigin: "https://gpt.example.test"},
		"http origin":      {TicketKey: bytes.Repeat([]byte{3}, 32), InternalToken: strings.Repeat("ab", 32), DetailOrigin: "http://gpt.example.test"},
		"origin with path": {TicketKey: bytes.Repeat([]byte{3}, 32), InternalToken: strings.Repeat("ab", 32), DetailOrigin: "https://gpt.example.test/x"},
	} {
		if err := module.EnableUserCenter(cfg); err == nil {
			t.Fatalf("%s must be rejected", name)
		}
	}
}

func TestRunBindNonceCleanupRemovesOnlyStaleNonces(t *testing.T) {
	module := openUserCenterTestModule(t)
	db := module.store.DB()
	now := time.Now().UTC()
	if _, err := db.Exec(`INSERT INTO used_bind_nonces(nonce,exp) VALUES ('stale',?),('still-valid',?),('just-expired',?)`,
		now.Add(-time.Hour).Unix(), now.Add(time.Minute).Unix(), now.Add(-20*time.Second).Unix()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- module.RunBindNonceCleanup(ctx, time.Hour) }()
	deadline := time.Now().Add(2 * time.Second)
	for {
		var stale int
		if err := db.QueryRow(`SELECT count(*) FROM used_bind_nonces WHERE nonce='stale'`).Scan(&stale); err != nil {
			t.Fatal(err)
		}
		if stale == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("stale nonce was not purged")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done
	var remaining int
	if err := db.QueryRow(`SELECT count(*) FROM used_bind_nonces WHERE nonce IN ('still-valid','just-expired')`).Scan(&remaining); err != nil {
		t.Fatal(err)
	}
	if remaining != 2 {
		t.Fatalf("nonces still inside the replay window were purged: remaining=%d", remaining)
	}
}
