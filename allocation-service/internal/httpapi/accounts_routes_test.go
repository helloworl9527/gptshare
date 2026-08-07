package httpapi

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	accountsvc "allocation-service/internal/account"
	allocatorsvc "allocation-service/internal/allocator"
	"allocation-service/internal/auth"
	cardsvc "allocation-service/internal/card"
	"allocation-service/internal/credential"
	metricssvc "allocation-service/internal/metrics"
	"allocation-service/internal/monitorclient"
	"allocation-service/internal/repository"
	"allocation-service/internal/store"
	userquerysvc "allocation-service/internal/userquery"
	"github.com/pquerna/otp"
	"github.com/pquerna/otp/hotp"
	"golang.org/x/crypto/bcrypt"
)

func TestAdminAccountCRUDSyncDegradeAndLeakRedaction(t *testing.T) {
	phaseOne := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer monitor-api-key-sentinel" {
			t.Fatalf("bad monitor auth header %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/monitor/accounts/import-for-allocation":
			var req map[string]string
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Fatal(err)
			}
			if req["token"] != "monitor-token-sentinel" {
				t.Fatalf("bad monitor token %q", req["token"])
			}
			_, _ = w.Write([]byte(`{"provider_account_id":"phase-one-123","email":"synced@example.test","status":"alive","plan":"plus","auth_expiry":"` + time.Now().UTC().Add(24*time.Hour).Format(time.RFC3339Nano) + `"}`))
		case "/api/v1/monitor/accounts/phase-one-123/status":
			_, _ = w.Write([]byte(`{"provider_account_id":"phase-one-123","status":"dead_normal"}`))
		default:
			t.Fatalf("unexpected monitor path %s", r.URL.Path)
		}
	}))
	defer phaseOne.Close()

	server := testAccountsTLSServer(t, phaseOne.URL)
	defer server.Close()
	client := authedAccountClient(t, server)
	csrf := getCSRF(t, client, server.URL)
	create := postJSON(t, client, server.URL+"/api/admin/accounts", csrf, map[string]any{
		"display_password":     "LEAK_PASSWORD_SENTINEL",
		"display_2fa_secret":   "LEAK_TOTP_SENTINEL",
		"source_url":           "https://accounts.example.test/orders/LEAK_SOURCE_SENTINEL",
		"max_concurrent_users": 2,
		"monitor_token":        "monitor-token-sentinel",
		"monitor_token_type":   "session_token",
	})
	createBody := readBody(t, create)
	if create.StatusCode != http.StatusCreated {
		t.Fatalf("create status=%d body=%s", create.StatusCode, createBody)
	}
	assertNoCredentialLeak(t, createBody)
	var created struct {
		Account struct {
			ID               int64  `json:"id"`
			MonitorAccountID string `json:"monitor_account_id"`
			MonitorStatus    string `json:"monitor_status"`
			DisplayUsername  string `json:"display_username"`
			AccountExpiry    string `json:"account_expiry"`
			SourceURL        string `json:"source_url"`
		} `json:"account"`
		Warnings []string `json:"warnings"`
	}
	if err := json.Unmarshal([]byte(createBody), &created); err != nil {
		t.Fatal(err)
	}
	if created.Account.ID == 0 || created.Account.MonitorAccountID != "phase-one-123" || created.Account.MonitorStatus != "alive" || created.Account.DisplayUsername != "synced@example.test" || created.Account.SourceURL != "https://accounts.example.test/orders/LEAK_SOURCE_SENTINEL" || len(created.Warnings) != 0 {
		t.Fatalf("bad create response: %+v", created)
	}
	if !strings.Contains(created.Account.AccountExpiry, "T") {
		t.Fatalf("account expiry was not backfilled: %+v", created.Account)
	}
	dbValue, ok := testDatabases.Load(server.URL)
	if !ok {
		t.Fatal("test database not registered")
	}
	var passwordCipher, totpCipher, sourceCipher []byte
	if err := dbValue.(*sql.DB).QueryRow(`SELECT display_password_secret, display_2fa_secret, source_url_secret FROM chatgpt_accounts WHERE id=?`, created.Account.ID).Scan(&passwordCipher, &totpCipher, &sourceCipher); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(passwordCipher, []byte("LEAK_PASSWORD_SENTINEL")) || bytes.Contains(totpCipher, []byte("LEAK_TOTP_SENTINEL")) || bytes.Contains(sourceCipher, []byte("LEAK_SOURCE_SENTINEL")) {
		t.Fatal("display credentials or source URL were not encrypted at rest")
	}
	list := getRaw(t, client, server.URL+"/api/admin/accounts")
	listBody := readBody(t, list)
	if list.StatusCode != http.StatusOK || !strings.Contains(listBody, "synced@example.test") {
		t.Fatalf("list status=%d body=%s", list.StatusCode, listBody)
	}
	assertNoCredentialLeak(t, listBody)
	update := putJSON(t, client, server.URL+"/api/admin/accounts/"+itoa(created.Account.ID), csrf, map[string]any{
		"display_username":     "updated-account",
		"display_password":     "UPDATED_PASSWORD_SENTINEL",
		"display_2fa_secret":   "UPDATED_TOTP_SENTINEL",
		"source_url":           "https://accounts.example.test/orders/UPDATED_SOURCE_SENTINEL",
		"account_expiry":       time.Now().UTC().Add(24 * time.Hour).Format(time.RFC3339Nano),
		"max_concurrent_users": 3,
		"status":               "available",
		"monitor_status":       "unknown",
		"monitor_account_id":   "phase-one-123",
	})
	updateBody := readBody(t, update)
	if update.StatusCode != http.StatusOK || !strings.Contains(updateBody, "updated-account") || !strings.Contains(updateBody, "UPDATED_SOURCE_SENTINEL") {
		t.Fatalf("update status=%d body=%s", update.StatusCode, updateBody)
	}
	assertNoCredentialLeak(t, updateBody)
	var updatedSourceCipher []byte
	if err := dbValue.(*sql.DB).QueryRow(`SELECT source_url_secret FROM chatgpt_accounts WHERE id=?`, created.Account.ID).Scan(&updatedSourceCipher); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(updatedSourceCipher, []byte("UPDATED_SOURCE_SENTINEL")) {
		t.Fatal("updated source URL was not encrypted at rest")
	}
	invalidSource := putJSON(t, client, server.URL+"/api/admin/accounts/"+itoa(created.Account.ID), csrf, map[string]any{
		"display_username":     "updated-account",
		"source_url":           "javascript:alert(1)",
		"account_expiry":       time.Now().UTC().Add(24 * time.Hour).Format(time.RFC3339Nano),
		"max_concurrent_users": 3,
		"status":               "available",
		"monitor_status":       "unknown",
		"monitor_account_id":   "phase-one-123",
	})
	if invalidSource.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("invalid source status=%d body=%s", invalidSource.StatusCode, readBody(t, invalidSource))
	}
	syncStatus := postJSON(t, client, server.URL+"/api/admin/accounts/"+itoa(created.Account.ID)+"/sync-status", csrf, map[string]any{})
	syncBody := readBody(t, syncStatus)
	if syncStatus.StatusCode != http.StatusOK || !strings.Contains(syncBody, `"monitor_status":"dead_normal"`) || !strings.Contains(syncBody, `"status":"expired"`) {
		t.Fatalf("sync status=%d body=%s", syncStatus.StatusCode, syncBody)
	}
	assertNoCredentialLeak(t, syncBody)

	phaseOne.Close()
	offline := postJSON(t, client, server.URL+"/api/admin/accounts", csrf, map[string]any{
		"display_password":     "offline-password-sentinel",
		"display_2fa_secret":   "offline-totp-sentinel",
		"max_concurrent_users": 1,
		"monitor_token":        "offline-monitor-token",
	})
	offlineBody := readBody(t, offline)
	if offline.StatusCode != http.StatusServiceUnavailable || !strings.Contains(offlineBody, "phase_one_monitor_unavailable") {
		t.Fatalf("offline create status=%d body=%s", offline.StatusCode, offlineBody)
	}
	if strings.Contains(offlineBody, "offline-password-sentinel") || strings.Contains(offlineBody, "offline-totp-sentinel") || strings.Contains(offlineBody, "offline-monitor-token") {
		t.Fatalf("offline response leaked credential: %s", offlineBody)
	}
}

func TestAdminAccountPullSyncPendingCredentialsAndFailClosed(t *testing.T) {
	var offline atomic.Bool
	phaseOne := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer monitor-api-key-sentinel" {
			t.Fatalf("bad monitor auth header %q", got)
		}
		if r.Method != http.MethodGet || r.URL.Path != "/api/v1/monitor/accounts" {
			t.Fatalf("unexpected monitor request %s %s", r.Method, r.URL.Path)
		}
		if offline.Load() {
			http.Error(w, `{"code":"temporary_unavailable"}`, http.StatusBadGateway)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"accounts":[{"provider_account_id":"phase-one-pull","email":"pulled@example.test","status":"alive","plan":"plus","auth_expiry":"` + time.Now().UTC().Add(45*24*time.Hour).Format(time.RFC3339Nano) + `"}]}`))
	}))
	defer phaseOne.Close()

	server := testAccountsTLSServer(t, phaseOne.URL)
	defer server.Close()
	client := authedAccountClient(t, server)
	csrf := getCSRF(t, client, server.URL)
	settings := putJSON(t, client, server.URL+"/api/admin/account-settings", csrf, map[string]any{"default_account_capacity": 6})
	if settings.StatusCode != http.StatusOK {
		t.Fatalf("settings status=%d body=%s", settings.StatusCode, readBody(t, settings))
	}
	pull := postJSON(t, client, server.URL+"/api/admin/accounts/pull-monitor", csrf, map[string]any{})
	pullBody := readBody(t, pull)
	if pull.StatusCode != http.StatusOK || !strings.Contains(pullBody, `"created":1`) || !strings.Contains(pullBody, `"status":"pending_credentials"`) || !strings.Contains(pullBody, `"display_username":"pulled@example.test"`) || !strings.Contains(pullBody, `"max_concurrent_users":6`) {
		t.Fatalf("pull status=%d body=%s", pull.StatusCode, pullBody)
	}
	assertNoCredentialLeak(t, pullBody)
	var parsed struct {
		Accounts []struct {
			ID int64 `json:"id"`
		} `json:"accounts"`
	}
	if err := json.Unmarshal([]byte(pullBody), &parsed); err != nil {
		t.Fatal(err)
	}
	if len(parsed.Accounts) != 1 || parsed.Accounts[0].ID == 0 {
		t.Fatalf("bad pull response: %s", pullBody)
	}
	accountID := parsed.Accounts[0].ID
	dbValue, ok := testDatabases.Load(server.URL)
	if !ok {
		t.Fatal("test database not registered")
	}
	var beforePassword []byte
	if err := dbValue.(*sql.DB).QueryRow(`SELECT display_password_secret FROM chatgpt_accounts WHERE id=?`, accountID).Scan(&beforePassword); err != nil {
		t.Fatal(err)
	}
	update := putJSON(t, client, server.URL+"/api/admin/accounts/"+itoa(accountID), csrf, map[string]any{
		"display_username":     "pulled@example.test",
		"display_password":     "PULLED_PASSWORD_SENTINEL",
		"display_2fa_secret":   "PULLED_TOTP_SENTINEL",
		"account_expiry":       time.Now().UTC().Add(45 * 24 * time.Hour).Format(time.RFC3339Nano),
		"max_concurrent_users": 6,
		"status":               "pending_credentials",
		"monitor_status":       "alive",
		"monitor_account_id":   "phase-one-pull",
	})
	updateBody := readBody(t, update)
	if update.StatusCode != http.StatusOK || !strings.Contains(updateBody, `"status":"available"`) {
		t.Fatalf("update status=%d body=%s", update.StatusCode, updateBody)
	}
	assertNoCredentialLeak(t, updateBody)
	var afterPassword []byte
	if err := dbValue.(*sql.DB).QueryRow(`SELECT display_password_secret FROM chatgpt_accounts WHERE id=?`, accountID).Scan(&afterPassword); err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(beforePassword, afterPassword) || bytes.Contains(afterPassword, []byte("PULLED_PASSWORD_SENTINEL")) {
		t.Fatal("pull credential update did not encrypt new password")
	}
	second := postJSON(t, client, server.URL+"/api/admin/accounts/pull-monitor", csrf, map[string]any{})
	secondBody := readBody(t, second)
	if second.StatusCode != http.StatusOK || !strings.Contains(secondBody, `"created":0`) || !strings.Contains(secondBody, `"updated":1`) || !strings.Contains(secondBody, `"status":"available"`) {
		t.Fatalf("second pull status=%d body=%s", second.StatusCode, secondBody)
	}
	var retainedPassword []byte
	if err := dbValue.(*sql.DB).QueryRow(`SELECT display_password_secret FROM chatgpt_accounts WHERE id=?`, accountID).Scan(&retainedPassword); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(afterPassword, retainedPassword) {
		t.Fatal("pull sync overwrote existing display credentials")
	}

	offline.Store(true)
	offlinePull := postJSON(t, client, server.URL+"/api/admin/accounts/pull-monitor", csrf, map[string]any{})
	offlineBody := readBody(t, offlinePull)
	if offlinePull.StatusCode != http.StatusServiceUnavailable || !strings.Contains(offlineBody, "phase_one_monitor_unavailable") {
		t.Fatalf("offline pull status=%d body=%s", offlinePull.StatusCode, offlineBody)
	}
	if strings.Contains(offlineBody, "pulled@example.test") {
		t.Fatalf("offline error leaked email: %s", offlineBody)
	}
}

func TestAdminAccountPullSyncMixedTerminalStatesAndItemFailure(t *testing.T) {
	now := time.Now().UTC()
	past := now.Add(-24 * time.Hour).Format(time.RFC3339Nano)
	future := now.Add(24 * time.Hour).Format(time.RFC3339Nano)
	phaseOne := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/monitor/accounts" {
			t.Fatalf("unexpected monitor path %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"accounts":[
			{"provider_account_id":"alive","email":"alive@example.test","status":"alive","auth_expiry":"` + future + `"},
			{"provider_account_id":"expired","email":"expired@example.test","status":"dead_normal","auth_expiry":"` + past + `"},
			{"provider_account_id":"conflict","email":"conflict@example.test","status":"alive","auth_expiry":"` + past + `"},
			{"provider_account_id":"banned","email":"banned@example.test","status":"dead_banned","auth_expiry":"` + past + `"},
			{"provider_account_id":"after-conflict","email":"after@example.test","status":"alive","auth_expiry":"` + future + `"}
		]}`))
	}))
	defer phaseOne.Close()

	server := testAccountsTLSServer(t, phaseOne.URL)
	defer server.Close()
	client := authedAccountClient(t, server)
	csrf := getCSRF(t, client, server.URL)
	response := postJSON(t, client, server.URL+"/api/admin/accounts/pull-monitor", csrf, map[string]any{})
	body := readBody(t, response)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("pull status=%d body=%s", response.StatusCode, body)
	}
	for _, want := range []string{
		`"total":5`, `"created":4`, `"updated":0`, `"skipped":0`, `"failed":1`,
		`"monitor_account_id":"conflict"`, `"code":"alive_expiry_conflict"`,
		`"monitor_account_id":"after-conflict"`, `"status":"expired"`, `"status":"banned"`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("pull body missing %s: %s", want, body)
		}
	}
	dbValue, ok := testDatabases.Load(server.URL)
	if !ok {
		t.Fatal("test database not registered")
	}
	var conflictCount, afterConflictCount int
	if err := dbValue.(*sql.DB).QueryRow("SELECT count(*) FROM chatgpt_accounts WHERE monitor_account_id='conflict'").Scan(&conflictCount); err != nil {
		t.Fatal(err)
	}
	if err := dbValue.(*sql.DB).QueryRow("SELECT count(*) FROM chatgpt_accounts WHERE monitor_account_id='after-conflict'").Scan(&afterConflictCount); err != nil {
		t.Fatal(err)
	}
	if conflictCount != 0 || afterConflictCount != 1 {
		t.Fatalf("persisted conflict=%d after-conflict=%d", conflictCount, afterConflictCount)
	}
}

func TestBatchMonitorSyncFailSafeAndRecovery(t *testing.T) {
	var offline atomic.Bool
	phaseOne := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer monitor-api-key-sentinel" {
			t.Fatalf("bad monitor auth header %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		if offline.Load() {
			http.Error(w, `{"code":"provider_unavailable"}`, http.StatusBadGateway)
			return
		}
		switch r.URL.Path {
		case "/api/v1/monitor/accounts/import-for-allocation":
			var req map[string]string
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Fatal(err)
			}
			id := strings.TrimSuffix(req["label"], "-account")
			if id == "" {
				id = strings.TrimSuffix(req["token"], "-monitor-token")
			}
			_, _ = w.Write([]byte(`{"provider_account_id":"` + id + `","email":"` + id + `@example.test","status":"alive","auth_expiry":"` + time.Now().UTC().Add(24*time.Hour).Format(time.RFC3339Nano) + `"}`))
		case "/api/v1/monitor/accounts/batch-status":
			_, _ = w.Write([]byte(`{"items":[
				{"provider_account_id":"alive","status":"alive"},
				{"provider_account_id":"banned","status":"dead_banned"},
				{"provider_account_id":"missing","error":{"code":"not_found","message":"missing"}}
			]}`))
		default:
			t.Fatalf("unexpected monitor path %s", r.URL.Path)
		}
	}))
	defer phaseOne.Close()

	server := testAccountsTLSServer(t, phaseOne.URL)
	defer server.Close()
	client := authedAccountClient(t, server)
	csrf := getCSRF(t, client, server.URL)
	for _, name := range []string{"alive-account", "banned-account", "missing-account"} {
		createSyncedAccountForHTTP(t, client, server.URL, csrf, name)
	}

	offline.Store(true)
	offlineSync := postJSON(t, client, server.URL+"/api/admin/accounts/sync-status", csrf, map[string]any{})
	offlineBody := readBody(t, offlineSync)
	if offlineSync.StatusCode != http.StatusOK || !strings.Contains(offlineBody, "phase_one_monitor_unavailable") {
		t.Fatalf("offline sync status=%d body=%s", offlineSync.StatusCode, offlineBody)
	}
	if strings.Contains(offlineBody, `"monitor_status":"dead_banned"`) {
		t.Fatalf("offline sync misclassified a ban: %s", offlineBody)
	}
	if countOccurrences(offlineBody, `"monitor_status":"unknown_monitor"`) != 3 {
		t.Fatalf("offline sync did not mark all accounts unknown_monitor: %s", offlineBody)
	}
	listOffline := getRaw(t, client, server.URL+"/api/admin/accounts")
	listOfflineBody := readBody(t, listOffline)
	if listOffline.StatusCode != http.StatusOK || !strings.Contains(listOfflineBody, "phase_one_monitor_unavailable") {
		t.Fatalf("offline list warning missing status=%d body=%s", listOffline.StatusCode, listOfflineBody)
	}

	offline.Store(false)
	recovered := postJSON(t, client, server.URL+"/api/admin/accounts/sync-status", csrf, map[string]any{})
	recoveredBody := readBody(t, recovered)
	if recovered.StatusCode != http.StatusOK {
		t.Fatalf("recovered sync status=%d body=%s", recovered.StatusCode, recoveredBody)
	}
	for _, want := range []string{`"monitor_status":"alive"`, `"monitor_status":"dead_banned"`, `"monitor_status":"not_found"`} {
		if !strings.Contains(recoveredBody, want) {
			t.Fatalf("recovered sync missing %s body=%s", want, recoveredBody)
		}
	}
}

func TestAccountValidationAndAllocatedDeleteHTTP(t *testing.T) {
	phaseOne := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"provider_account_id":"local-phase-one","email":"local@example.test","status":"alive","auth_expiry":"` + time.Now().UTC().Add(24*time.Hour).Format(time.RFC3339Nano) + `"}`))
	}))
	defer phaseOne.Close()
	server := testAccountsTLSServer(t, phaseOne.URL)
	defer server.Close()
	client := authedAccountClient(t, server)
	csrf := getCSRF(t, client, server.URL)
	missingToken := postJSON(t, client, server.URL+"/api/admin/accounts", csrf, map[string]any{
		"display_password":     "secret-password",
		"display_2fa_secret":   "secret-totp",
		"max_concurrent_users": 1,
	})
	if missingToken.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("missing token status=%d body=%s", missingToken.StatusCode, readBody(t, missingToken))
	}
	accountID := createLocalAccountForHTTP(t, client, server.URL, csrf)
	deleted := deleteRaw(t, client, server.URL+"/api/admin/accounts/"+itoa(accountID), csrf)
	if deleted.StatusCode != http.StatusNoContent {
		t.Fatalf("delete status=%d body=%s", deleted.StatusCode, readBody(t, deleted))
	}
}

func TestAccountDefaultCapacitySettingsAndApplyAllHTTP(t *testing.T) {
	phaseOne := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var req map[string]string
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatal(err)
		}
		id := strings.TrimSuffix(req["token"], "-monitor-token")
		_, _ = w.Write([]byte(`{"provider_account_id":"` + id + `","email":"` + id + `@example.test","status":"alive","auth_expiry":"` + time.Now().UTC().Add(24*time.Hour).Format(time.RFC3339Nano) + `"}`))
	}))
	defer phaseOne.Close()
	server := testAccountsTLSServer(t, phaseOne.URL)
	defer server.Close()
	client := authedAccountClient(t, server)
	csrf := getCSRF(t, client, server.URL)
	settings := getRaw(t, client, server.URL+"/api/admin/account-settings")
	settingsBody := readBody(t, settings)
	if settings.StatusCode != http.StatusOK || !strings.Contains(settingsBody, `"default_account_capacity":3`) {
		t.Fatalf("initial settings status=%d body=%s", settings.StatusCode, settingsBody)
	}
	updated := putJSON(t, client, server.URL+"/api/admin/account-settings", csrf, map[string]any{"default_account_capacity": 5})
	updatedBody := readBody(t, updated)
	if updated.StatusCode != http.StatusOK || !strings.Contains(updatedBody, `"default_account_capacity":5`) {
		t.Fatalf("update settings status=%d body=%s", updated.StatusCode, updatedBody)
	}
	created := postJSON(t, client, server.URL+"/api/admin/accounts", csrf, map[string]any{
		"display_password":   "secret-password",
		"display_2fa_secret": "secret-totp",
		"monitor_token":      "defaulted-monitor-token",
	})
	createdBody := readBody(t, created)
	if created.StatusCode != http.StatusCreated || !strings.Contains(createdBody, `"max_concurrent_users":5`) {
		t.Fatalf("defaulted create status=%d body=%s", created.StatusCode, createdBody)
	}
	if response := putJSON(t, client, server.URL+"/api/admin/account-settings", csrf, map[string]any{"default_account_capacity": 2}); response.StatusCode != http.StatusOK {
		t.Fatalf("downshift default status=%d body=%s", response.StatusCode, readBody(t, response))
	}
	applied := postJSON(t, client, server.URL+"/api/admin/accounts/apply-default-capacity", csrf, map[string]any{})
	appliedBody := readBody(t, applied)
	if applied.StatusCode != http.StatusOK || !strings.Contains(appliedBody, `"default_account_capacity":2`) || !strings.Contains(appliedBody, `"updated_accounts":1`) {
		t.Fatalf("apply default status=%d body=%s", applied.StatusCode, appliedBody)
	}
	list := getRaw(t, client, server.URL+"/api/admin/accounts")
	listBody := readBody(t, list)
	if list.StatusCode != http.StatusOK || !strings.Contains(listBody, `"max_concurrent_users":2`) {
		t.Fatalf("list after apply status=%d body=%s", list.StatusCode, listBody)
	}
}

func createSyncedAccountForHTTP(t *testing.T, client *http.Client, baseURL, csrf, name string) int64 {
	t.Helper()
	resp := postJSON(t, client, baseURL+"/api/admin/accounts", csrf, map[string]any{
		"display_password":     "secret-password",
		"display_2fa_secret":   "secret-totp",
		"max_concurrent_users": 1,
		"monitor_token":        strings.TrimSuffix(name, "-account") + "-monitor-token",
	})
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create synced status=%d body=%s", resp.StatusCode, body)
	}
	var parsed struct {
		Account struct {
			ID int64 `json:"id"`
		} `json:"account"`
	}
	if err := json.Unmarshal([]byte(body), &parsed); err != nil {
		t.Fatal(err)
	}
	return parsed.Account.ID
}

func countOccurrences(value, needle string) int {
	count := 0
	for {
		index := strings.Index(value, needle)
		if index < 0 {
			return count
		}
		count++
		value = value[index+len(needle):]
	}
}

func testAccountsTLSServer(t *testing.T, monitorBaseURL string) *httptest.Server {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "private")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	database, err := store.Open(context.Background(), filepath.Join(dir, "accounts.db"))
	if err != nil {
		t.Fatal(err)
	}
	hash, err := bcrypt.GenerateFromPassword([]byte("correct horse battery staple"), 12)
	if err != nil {
		t.Fatal(err)
	}
	manager, err := auth.New(auth.Config{
		DB: database.DB(), Username: "admin", PasswordHash: string(hash),
		TOTPSecret: []byte("synthetic-test-secret"), SessionKey: []byte(strings.Repeat("j", 32)), CSRFSigningKey: []byte(strings.Repeat("r", 32)),
	})
	if err != nil {
		t.Fatal(err)
	}
	keyring, err := credential.NewKeyring(map[string][]byte{"alloc-http-k1": bytes.Repeat([]byte{9}, 32)}, "alloc-http-k1")
	if err != nil {
		t.Fatal(err)
	}
	repo := repository.New(database.DB(), keyring)
	monitor, err := monitorclient.New(monitorBaseURL, "monitor-api-key-sentinel", &http.Client{Timeout: 100 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	accounts := accountsvc.NewService(repo, monitor)
	cards := cardsvc.NewService(repo)
	allocator := allocatorsvc.NewService(repo, monitor)
	userQuery := userquerysvc.NewService(repo)
	metrics := metricssvc.NewService(repo)
	router := NewRouter(database, manager, Config{Origin: "https://127.0.0.1", Accounts: accounts, Cards: cards, Allocator: allocator, UserQuery: userQuery, Metrics: metrics}, slog.New(slog.NewJSONHandler(io.Discard, nil)))
	server := httptest.NewTLSServer(router)
	testRepositories.Store(server.URL, repo)
	testDatabases.Store(server.URL, database.DB())
	t.Cleanup(func() {
		testRepositories.Delete(server.URL)
		testDatabases.Delete(server.URL)
		server.Close()
		database.Close()
	})
	return server
}

func authedAccountClient(t *testing.T, server *httptest.Server) *http.Client {
	t.Helper()
	client := server.Client()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	client.Jar = jar
	csrf := getCSRF(t, client, server.URL)
	challenge := postPassword(t, client, server.URL, csrf, "admin", "correct horse battery staple", http.StatusOK)
	code := accountCodeAt(t, testTOTPSecret(), time.Now())
	resp := postJSON(t, client, server.URL+"/api/auth/totp", csrf, map[string]string{"challenge": challenge, "code": code})
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("totp status=%d body=%s", resp.StatusCode, readBody(t, resp))
	}
	return client
}

func createLocalAccountForHTTP(t *testing.T, client *http.Client, baseURL, csrf string) int64 {
	t.Helper()
	_ = client
	_ = csrf
	value, ok := testRepositories.Load(baseURL)
	if !ok {
		t.Fatal("test repository not registered")
	}
	id, err := value.(*repository.Repository).CreateAccount(context.Background(), repository.AccountSeed{
		DisplayUsername:    "local-account",
		DisplayPassword:    "secret-password",
		DisplayTOTPSecret:  "secret-totp",
		AccountExpiry:      time.Now().UTC().Add(24 * time.Hour),
		MaxConcurrentUsers: 1,
		MonitorStatus:      "unknown_monitor",
		Status:             "available",
	})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func putJSON(t *testing.T, client *http.Client, url, csrf string, payload any) *http.Response {
	t.Helper()
	return requestJSON(t, client, http.MethodPut, url, csrf, payload)
}

func deleteRaw(t *testing.T, client *http.Client, url, csrf string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodDelete, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Origin", "https://127.0.0.1")
	req.Header.Set(csrfHeader, csrf)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func requestJSON(t *testing.T, client *http.Client, method, url, csrf string, payload any) *http.Response {
	t.Helper()
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(method, url, bytes.NewReader(encoded))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "https://127.0.0.1")
	req.Header.Set(csrfHeader, csrf)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func accountCodeAt(t *testing.T, secret string, at time.Time) string {
	t.Helper()
	code, err := hotp.GenerateCodeCustom(secret, uint64(at.Unix()/30), hotp.ValidateOpts{Digits: otp.DigitsSix, Algorithm: otp.AlgorithmSHA1})
	if err != nil {
		t.Fatal(err)
	}
	return code
}

func assertNoCredentialLeak(t *testing.T, body string) {
	t.Helper()
	for _, needle := range []string{"LEAK_PASSWORD_SENTINEL", "LEAK_TOTP_SENTINEL", "UPDATED_PASSWORD_SENTINEL", "UPDATED_TOTP_SENTINEL", "monitor-token-sentinel", "display_password", "display_2fa_secret"} {
		if strings.Contains(body, needle) {
			t.Fatalf("response leaked %q: %s", needle, body)
		}
	}
}

func itoa(value int64) string {
	return strconvFormatInt(value)
}

func strconvFormatInt(value int64) string {
	return strconv.FormatInt(value, 10)
}
