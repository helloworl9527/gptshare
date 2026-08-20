package e2e

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base32"
	"encoding/base64"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"chatgpt-monitor/internal/account"
	"chatgpt-monitor/internal/chatgpt"
	credentialcrypto "chatgpt-monitor/internal/crypto"
	"chatgpt-monitor/internal/store"
	"github.com/pquerna/otp"
	"github.com/pquerna/otp/hotp"
	"golang.org/x/crypto/bcrypt"
)

const (
	binaryAdminUser   = "vitals-binary-admin"
	binaryPassword    = "vitals binary password 2026"
	binaryDisplayTOTP = "JBSWY3DPEHPK3PXP"
)

var binaryAdminTOTP = base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(bytes.Repeat([]byte{'t'}, 20))

type synchronizedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *synchronizedBuffer) Write(value []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(value)
}

func (b *synchronizedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}

type binaryProcess struct {
	command *exec.Cmd
	logs    *synchronizedBuffer
	wait    chan error
}

type binaryClient struct {
	baseURL string
	origin  string
	cookies map[string]string
	client  *http.Client
}

type binaryResponse struct {
	status int
	body   string
}

type generatedCard struct {
	ID   int64  `json:"id"`
	Code string `json:"code"`
}

func TestVitalsBinaryUnifiedFlowAndCrashLoopMatrix(t *testing.T) {
	if testing.Short() {
		t.Skip("real binary regression is not a short test")
	}
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	privateDir := filepath.Join(t.TempDir(), "private")
	if err := os.Mkdir(privateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	monitorPath := filepath.Join(privateDir, "monitor.db")
	allocationPath := filepath.Join(privateDir, "allocation.db")
	monitorKey := bytes.Repeat([]byte{'m'}, 32)
	allocationKey := bytes.Repeat([]byte{'a'}, 32)
	jwtKey := bytes.Repeat([]byte{'j'}, 32)
	rateKey := bytes.Repeat([]byte{'r'}, 32)
	passwordHash, err := bcrypt.GenerateFromPassword([]byte(binaryPassword), 12)
	if err != nil {
		t.Fatal(err)
	}
	seedMonitorAccounts(t, root, monitorPath, monitorKey)

	binaryPath := filepath.Join(privateDir, "vitals")
	build := exec.Command("go", "build", "-trimpath", "-o", binaryPath, "./cmd/vitals")
	build.Dir = root
	if output, buildErr := build.CombinedOutput(); buildErr != nil {
		t.Fatalf("build cmd/vitals: %v\n%s", buildErr, output)
	}
	port := reservePort(t)
	baseURL := "http://127.0.0.1:" + strconv.Itoa(port)
	origin := "https://127.0.0.1:" + strconv.Itoa(port)
	env := map[string]string{
		"VITALS_PORT":                         "127.0.0.1:" + strconv.Itoa(port),
		"MONITOR_DB_PATH":                     monitorPath,
		"MONITOR_MIGRATIONS_DIR":              filepath.Join(root, "migrations"),
		"ALLOCATION_DB_PATH":                  allocationPath,
		"CREDENTIAL_MASTER_KEYS":              "monitor-binary:" + base64.StdEncoding.EncodeToString(monitorKey),
		"CREDENTIAL_ACTIVE_KEY_ID":            "monitor-binary",
		"ALLOCATION_CREDENTIAL_MASTER_KEYS":   "allocation-binary:" + base64.RawURLEncoding.EncodeToString(allocationKey),
		"ALLOCATION_CREDENTIAL_ACTIVE_KEY_ID": "allocation-binary",
		"ADMIN_USER":                          binaryAdminUser,
		"ADMIN_PASSWORD_HASH":                 string(passwordHash),
		"ADMIN_TOTP_SECRET":                   binaryAdminTOTP,
		"JWT_SIGNING_KEY":                     base64.StdEncoding.EncodeToString(jwtKey),
		"RATE_LIMIT_KEY":                      base64.StdEncoding.EncodeToString(rateKey),
		"APP_ORIGIN":                          origin,
		"TRUST_LOOPBACK_PROXY":                "false",
		"VITALS_MONITOR_COMPAT_HTTP_ENABLED":  "false",
		"VITALS_TEST_REPLACEMENT_INTERVAL":    "40ms",
	}

	process := startVitalsBinary(t, binaryPath, env, "")
	waitForHTTP(t, process, baseURL+"/health", 5*time.Second)
	admin := &binaryClient{baseURL: baseURL, origin: origin, cookies: map[string]string{}, client: &http.Client{Timeout: 5 * time.Second}}
	public := &binaryClient{baseURL: baseURL, cookies: map[string]string{}, client: &http.Client{Timeout: 5 * time.Second}}
	csrf := binaryLogin(t, admin)

	for _, path := range []string{"/admin/", "/api/accounts", "/api/settings", "/api/admin/dashboard", "/api/admin/config/security-boundaries"} {
		response := admin.request(t, http.MethodGet, path, "", nil)
		if response.status != http.StatusOK {
			t.Fatalf("authenticated GET %s = %d body=%s", path, response.status, response.body)
		}
	}
	compat := public.request(t, http.MethodGet, "/api/v1/monitor/accounts", "", nil)
	if compat.status != http.StatusNotFound {
		t.Fatalf("default compatibility route=%d, want 404", compat.status)
	}

	pull := admin.request(t, http.MethodPost, "/api/admin/accounts/pull-monitor", csrf, map[string]any{})
	if pull.status != http.StatusOK {
		t.Fatalf("monitor pull=%d body=%s", pull.status, pull.body)
	}
	var pulled struct {
		Accounts []struct {
			ID                 int64  `json:"id"`
			DisplayUsername    string `json:"display_username"`
			AccountExpiry      string `json:"account_expiry"`
			Status             string `json:"status"`
			MonitorStatus      string `json:"monitor_status"`
			MonitorAccountID   string `json:"monitor_account_id"`
			MaxConcurrentUsers int    `json:"max_concurrent_users"`
		} `json:"accounts"`
		Created int `json:"created"`
	}
	decodeBinaryJSON(t, pull.body, &pulled)
	if pulled.Created < 8 {
		t.Fatalf("pulled accounts=%d body=%s", pulled.Created, pull.body)
	}
	byMonitorID := make(map[string]int64, len(pulled.Accounts))
	for _, item := range pulled.Accounts {
		byMonitorID[item.MonitorAccountID] = item.ID
		if item.MonitorAccountID == "pending-binary" {
			if item.Status != "pending_credentials" {
				t.Fatalf("pending account status=%s", item.Status)
			}
			continue
		}
		if item.MonitorStatus == "dead_banned" {
			// 封禁账号没有存量顾客时会被换号扫描自动归档，管理端不再接受编辑。
			continue
		}
		updated := admin.request(t, http.MethodPut, "/api/admin/accounts/"+strconv.FormatInt(item.ID, 10), csrf, map[string]any{
			"display_username":     item.DisplayUsername,
			"display_password":     "DISPLAY_PASSWORD_BINARY_SENTINEL_" + item.MonitorAccountID,
			"display_2fa_secret":   binaryDisplayTOTP,
			"account_expiry":       item.AccountExpiry,
			"max_concurrent_users": 1,
			"status":               "pending_credentials",
			"monitor_status":       item.MonitorStatus,
			"monitor_account_id":   item.MonitorAccountID,
		})
		if updated.status != http.StatusOK || !strings.Contains(updated.body, `"status":"available"`) {
			t.Fatalf("complete %s=%d body=%s", item.MonitorAccountID, updated.status, updated.body)
		}
		if strings.Contains(updated.body, "DISPLAY_PASSWORD_BINARY_SENTINEL") || strings.Contains(updated.body, binaryDisplayTOTP) {
			t.Fatalf("credential leaked in update response: %s", updated.body)
		}
	}

	pausedID := monitorAccountID(t, monitorPath, "target-binary")
	monitorDB := openSQLite(t, monitorPath)
	if _, err := monitorDB.Exec(`UPDATE accounts SET polling_paused=1,pause_reason='evidence_review' WHERE id=?`, pausedID); err != nil {
		t.Fatal(err)
	}
	refresh := admin.request(t, http.MethodPost, "/api/accounts/"+strconv.FormatInt(pausedID, 10)+"/refresh", csrf, map[string]any{})
	if refresh.status != http.StatusConflict || !strings.Contains(refresh.body, "evidence_review_required") {
		t.Fatalf("monitor fail-safe refresh=%d body=%s", refresh.status, refresh.body)
	}

	generatedResponse := admin.request(t, http.MethodPost, "/api/admin/cards/generate", csrf, map[string]any{"quantity": 10, "duration_days": 7})
	if generatedResponse.status != http.StatusCreated {
		t.Fatalf("generate cards=%d body=%s", generatedResponse.status, generatedResponse.body)
	}
	var generated struct {
		Cards []generatedCard `json:"cards"`
	}
	decodeBinaryJSON(t, generatedResponse.body, &generated)
	if len(generated.Cards) != 10 {
		t.Fatalf("generated cards=%d", len(generated.Cards))
	}

	firstRedeem := public.request(t, http.MethodPost, "/api/redeem", "", map[string]string{"code": generated.Cards[0].Code})
	if firstRedeem.status != http.StatusOK || !strings.Contains(firstRedeem.body, "target-binary@example.test") || strings.Contains(firstRedeem.body, "banned-binary@example.test") || strings.Contains(firstRedeem.body, "pending-binary@example.test") {
		t.Fatalf("online exclusion redeem=%d body=%s", firstRedeem.status, firstRedeem.body)
	}
	firstQuery := public.request(t, http.MethodPost, "/api/cards/query", "", map[string]string{"code": generated.Cards[0].Code})
	if firstQuery.status != http.StatusOK || !strings.Contains(firstQuery.body, "DISPLAY_PASSWORD_BINARY_SENTINEL_target-binary") {
		t.Fatalf("public query=%d body=%s", firstQuery.status, firstQuery.body)
	}
	reveal := admin.request(t, http.MethodGet, "/api/admin/cards/"+strconv.FormatInt(generated.Cards[0].ID, 10)+"/reveal", csrf, nil)
	withoutRevealCSRF := admin.request(t, http.MethodGet, "/api/admin/cards/"+strconv.FormatInt(generated.Cards[0].ID, 10)+"/reveal", "", nil)
	if withoutRevealCSRF.status != http.StatusForbidden {
		t.Fatalf("reveal without CSRF=%d body=%s", withoutRevealCSRF.status, withoutRevealCSRF.body)
	}
	if reveal.status != http.StatusOK || !strings.Contains(reveal.body, generated.Cards[0].Code) {
		t.Fatalf("reveal=%d body=%s", reveal.status, reveal.body)
	}
	exported := admin.request(t, http.MethodPost, "/api/admin/cards/export", csrf, map[string]any{"quantity": 2, "duration_days": 7, "format": "csv"})
	if exported.status != http.StatusOK || !strings.HasPrefix(exported.body, "code,duration_days") {
		t.Fatalf("card export=%d body=%s", exported.status, exported.body)
	}

	now := time.Now().UTC()
	if _, err := monitorDB.Exec(`UPDATE accounts SET auth_expiry=?,current_expiry=? WHERE provider_account_id='target-binary'`, now.Add(12*time.Hour).Format(time.RFC3339Nano), now.Add(12*time.Hour).Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	pull = admin.request(t, http.MethodPost, "/api/admin/accounts/pull-monitor", csrf, map[string]any{})
	if pull.status != http.StatusOK {
		t.Fatalf("expiry pull=%d body=%s", pull.status, pull.body)
	}
	waitForBody(t, 5*time.Second, func() binaryResponse {
		return public.request(t, http.MethodPost, "/api/cards/query", "", map[string]string{"code": generated.Cards[0].Code})
	}, `"allocation_state":"grace"`)

	secondRedeem := public.request(t, http.MethodPost, "/api/redeem", "", map[string]string{"code": generated.Cards[1].Code})
	if secondRedeem.status != http.StatusOK {
		t.Fatalf("second redeem=%d body=%s", secondRedeem.status, secondRedeem.body)
	}
	var second struct {
		Account struct {
			ID       int64  `json:"id"`
			Username string `json:"display_username"`
		} `json:"account"`
	}
	decodeBinaryJSON(t, secondRedeem.body, &second)
	if _, err := monitorDB.Exec(`UPDATE accounts SET status='dead_banned',dead_at=?,death_type='abnormal_ban' WHERE email=?`, now.Format(time.RFC3339Nano), second.Account.Username); err != nil {
		t.Fatal(err)
	}
	pull = admin.request(t, http.MethodPost, "/api/admin/accounts/pull-monitor", csrf, map[string]any{})
	if pull.status != http.StatusOK {
		t.Fatalf("banned pull=%d body=%s", pull.status, pull.body)
	}
	bannedReplacement := waitForBodyWithout(t, 5*time.Second, func() binaryResponse {
		return public.request(t, http.MethodPost, "/api/cards/query", "", map[string]string{"code": generated.Cards[1].Code})
	}, second.Account.Username)
	if strings.Contains(bannedReplacement.body, second.Account.Username) || strings.Contains(bannedReplacement.body, `"allocation_state":"grace"`) {
		t.Fatalf("banned replacement retained grace/old account: %s", bannedReplacement.body)
	}

	thirdRedeem := public.request(t, http.MethodPost, "/api/redeem", "", map[string]string{"code": generated.Cards[2].Code})
	if thirdRedeem.status != http.StatusOK {
		t.Fatalf("third redeem=%d body=%s", thirdRedeem.status, thirdRedeem.body)
	}
	extended := admin.request(t, http.MethodPost, "/api/admin/cards/"+strconv.FormatInt(generated.Cards[2].ID, 10)+"/extend", csrf, map[string]int{"days": 14})
	if extended.status != http.StatusOK {
		t.Fatalf("extend=%d body=%s", extended.status, extended.body)
	}
	revoked := admin.request(t, http.MethodPost, "/api/admin/cards/"+strconv.FormatInt(generated.Cards[2].ID, 10)+"/revoke", csrf, map[string]any{})
	if revoked.status != http.StatusOK || !strings.Contains(revoked.body, `"status":"revoked"`) {
		t.Fatalf("revoke=%d body=%s", revoked.status, revoked.body)
	}
	if afterRevoke := public.request(t, http.MethodPost, "/api/cards/query", "", map[string]string{"code": generated.Cards[2].Code}); afterRevoke.status != http.StatusNotFound {
		t.Fatalf("revoked query=%d body=%s", afterRevoke.status, afterRevoke.body)
	}

	allocationDB := openSQLite(t, allocationPath)
	defer allocationDB.Close()
	var revealAudit, expiringReplacement, bannedHistory int
	if err := allocationDB.QueryRow(`SELECT count(*) FROM audit_events WHERE action='cards.reveal' AND target_id=?`, generated.Cards[0].ID).Scan(&revealAudit); err != nil {
		t.Fatal(err)
	}
	if err := allocationDB.QueryRow(`SELECT count(*) FROM replacement_history WHERE reason='account_expiring'`).Scan(&expiringReplacement); err != nil {
		t.Fatal(err)
	}
	if err := allocationDB.QueryRow(`SELECT count(*) FROM replacement_history WHERE reason='banned'`).Scan(&bannedHistory); err != nil {
		t.Fatal(err)
	}
	if revealAudit != 1 || expiringReplacement < 1 || bannedHistory < 1 {
		t.Fatalf("audit/history reveal=%d expiring=%d banned=%d", revealAudit, expiringReplacement, bannedHistory)
	}
	if list := admin.request(t, http.MethodGet, "/api/admin/cards?status=redeemed", "", nil); list.status != http.StatusOK || !strings.Contains(list.body, generated.Cards[0].Code[len(generated.Cards[0].Code)-4:]) {
		t.Fatalf("allocation record view inputs=%d body=%s", list.status, list.body)
	}
	t.Logf("vitals_binary_flow=pass login=single monitor_views=ok facade_pull=%d pending=preserved credential_completion=ok cards_generated=%d online_dead_banned_excluded=yes reveal_csrf=403 reveal_audit=%d expiry_grace=%d banned_immediate=%d extend_revoke=ok compat=404", pulled.Created, len(generated.Cards), revealAudit, expiringReplacement, bannedHistory)

	monitorBackup := filepath.Join(privateDir, "monitor-before-runtime-fault.db")
	if _, err := monitorDB.Exec("VACUUM INTO '" + strings.ReplaceAll(monitorBackup, "'", "''") + "'"); err != nil {
		t.Fatal(err)
	}
	if _, err := monitorDB.Exec(`ALTER TABLE accounts RENAME TO accounts_runtime_fault`); err != nil {
		t.Fatal(err)
	}
	if monitorFailure := admin.request(t, http.MethodGet, "/api/accounts", "", nil); monitorFailure.status == http.StatusOK {
		t.Fatalf("monitor account view stayed healthy after accounts table failure: %s", monitorFailure.body)
	}
	if allocationStillHealthy := admin.request(t, http.MethodGet, "/api/admin/cards", "", nil); allocationStillHealthy.status != http.StatusOK {
		t.Fatalf("allocation admin after monitor DB failure=%d body=%s", allocationStillHealthy.status, allocationStillHealthy.body)
	}
	offlineRedeem := public.request(t, http.MethodPost, "/api/redeem", "", map[string]string{"code": generated.Cards[7].Code})
	if offlineRedeem.status != http.StatusOK || !strings.Contains(offlineRedeem.body, `"warnings":["monitor_unavailable"]`) {
		t.Fatalf("offline redeem=%d body=%s", offlineRedeem.status, offlineRedeem.body)
	}
	if publicAfterFailure := public.request(t, http.MethodPost, "/api/cards/query", "", map[string]string{"code": generated.Cards[0].Code}); publicAfterFailure.status != http.StatusOK {
		t.Fatalf("public query after monitor DB failure=%d body=%s", publicAfterFailure.status, publicAfterFailure.body)
	}
	if publicPage := public.request(t, http.MethodGet, "/", "", nil); publicPage.status != http.StatusOK {
		t.Fatalf("public page after monitor DB failure=%d", publicPage.status)
	}
	t.Log("vitals_binary_db_fault=pass failed_db=monitor monitor_view=non200 allocation_admin=200 public_page=200 public_query=200 offline_redeem=200 warning=monitor_unavailable")

	monitorDB.Close()
	stopVitalsBinary(t, process)
	for _, path := range []string{monitorPath, monitorPath + "-wal", monitorPath + "-shm"} {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
	}
	if err := os.Rename(monitorBackup, monitorPath); err != nil {
		t.Fatal(err)
	}

	for index, task := range []string{"poller", "outbox", "replacement", "facade-sync"} {
		process = startVitalsBinary(t, binaryPath, env, task)
		waitForBackgroundDegraded(t, process, baseURL, 5*time.Second)
		if err := process.command.Process.Signal(syscall.Signal(0)); err != nil {
			t.Fatalf("%s process is not alive: %v", task, err)
		}
		health := public.request(t, http.MethodGet, "/health", "", nil)
		if health.status != http.StatusOK || !strings.Contains(health.body, `"background":"degraded"`) {
			t.Fatalf("%s health=%d body=%s", task, health.status, health.body)
		}
		for _, path := range []string{"/", "/api/monitor/ping", "/api/accounts", "/api/admin/accounts"} {
			client := public
			if strings.HasPrefix(path, "/api/accounts") || strings.HasPrefix(path, "/api/admin") {
				client = admin
			}
			response := client.request(t, http.MethodGet, path, "", nil)
			if response.status != http.StatusOK {
				t.Fatalf("%s GET %s=%d body=%s", task, path, response.status, response.body)
			}
		}
		query := public.request(t, http.MethodPost, "/api/cards/query", "", map[string]string{"code": generated.Cards[0].Code})
		if query.status != http.StatusOK {
			t.Fatalf("%s public query=%d body=%s", task, query.status, query.body)
		}
		redeem := public.request(t, http.MethodPost, "/api/redeem", "", map[string]string{"code": generated.Cards[3+index].Code})
		if redeem.status != http.StatusOK {
			t.Fatalf("%s public redeem=%d body=%s", task, redeem.status, redeem.body)
		}
		time.Sleep(350 * time.Millisecond)
		logs := process.logs.String()
		if got := strings.Count(logs, `"error_code":"background_task_panic"`); got != 3 {
			t.Fatalf("%s panic count=%d logs=%s", task, got, logs)
		}
		if got := strings.Count(logs, `"error_code":"background_task_terminal_degraded"`); got != 1 {
			t.Fatalf("%s terminal degraded count=%d logs=%s", task, got, logs)
		}
		for _, forbidden := range []string{"DISPLAY_PASSWORD_BINARY_SENTINEL", binaryDisplayTOTP, binaryAdminTOTP, "@example.test", generated.Cards[0].Code} {
			if strings.Contains(logs, forbidden) {
				t.Fatalf("%s log leaked forbidden value %q", task, forbidden)
			}
		}
		t.Logf("vitals_binary_crash_loop=pass task=%s pid_alive=yes health=200 background=degraded public_page=200 public_query=200 public_redeem=200 other_monitor=200 other_allocation=200 panic_events=3 terminal_degraded=1 leak_hits=0", task)
		stopVitalsBinary(t, process)
	}

	for _, dbPath := range []string{monitorPath, allocationPath} {
		database := openSQLite(t, dbPath)
		var integrity string
		if err := database.QueryRow("PRAGMA integrity_check").Scan(&integrity); err != nil {
			t.Fatal(err)
		}
		database.Close()
		if integrity != "ok" {
			t.Fatalf("integrity %s=%s", dbPath, integrity)
		}
	}
	if byMonitorID["target-binary"] == 0 {
		t.Fatal("target account was not synchronized through facade DTO")
	}
}

func seedMonitorAccounts(t *testing.T, root, dbPath string, key []byte) {
	t.Helper()
	database, err := store.Open(context.Background(), dbPath, os.DirFS(filepath.Join(root, "migrations")))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	keyring, err := credentialcrypto.NewKeyring(map[string][]byte{"monitor-binary": key}, "monitor-binary")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	fixtures := []struct {
		id     string
		expiry time.Time
		status string
	}{
		{id: "pending-binary", expiry: now.Add(7*24*time.Hour + time.Hour), status: "alive"},
		{id: "banned-binary", expiry: now.Add(7*24*time.Hour + 2*time.Hour), status: "dead_banned"},
		{id: "target-binary", expiry: now.Add(8 * 24 * time.Hour), status: "alive"},
		{id: "spare-1-binary", expiry: now.Add(10 * 24 * time.Hour), status: "alive"},
		{id: "spare-2-binary", expiry: now.Add(20 * 24 * time.Hour), status: "alive"},
		{id: "spare-3-binary", expiry: now.Add(30 * 24 * time.Hour), status: "alive"},
		{id: "spare-4-binary", expiry: now.Add(40 * 24 * time.Hour), status: "alive"},
		{id: "spare-5-binary", expiry: now.Add(50 * 24 * time.Hour), status: "alive"},
		{id: "spare-6-binary", expiry: now.Add(60 * 24 * time.Hour), status: "alive"},
		{id: "spare-7-binary", expiry: now.Add(70 * 24 * time.Hour), status: "alive"},
		{id: "spare-8-binary", expiry: now.Add(80 * 24 * time.Hour), status: "alive"},
		{id: "spare-9-binary", expiry: now.Add(90 * 24 * time.Hour), status: "alive"},
		{id: "spare-10-binary", expiry: now.Add(100 * 24 * time.Hour), status: "alive"},
	}
	for _, fixture := range fixtures {
		client := &accountClient{
			tokens: chatgpt.TokenSet{AccessToken: "MONITOR_ACCESS_BINARY_SENTINEL_" + fixture.id},
			status: chatgpt.StatusResult{
				ProviderAccountID:  fixture.id,
				Email:              fixture.id + "@example.test",
				RawPlan:            "chatgptplusplan",
				Plan:               chatgpt.PlanPlus,
				SubscriptionExpiry: &fixture.expiry,
				AccountState:       chatgpt.StateActive,
				EvidenceCode:       "binary_seed",
				EvidenceLevel:      chatgpt.EvidenceLiveVerified,
			},
		}
		service, err := account.NewService(database.DB(), client, keyring)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := service.ImportByToken(context.Background(), &account.TokenInput{Label: fixture.id, AccessToken: "MONITOR_INPUT_BINARY_SENTINEL_" + fixture.id}); err != nil {
			t.Fatal(err)
		}
		if fixture.status == "dead_banned" {
			if _, err := database.DB().Exec(`UPDATE accounts SET status='dead_banned',dead_at=?,death_type='abnormal_ban' WHERE provider_account_id=?`, now.Format(time.RFC3339Nano), fixture.id); err != nil {
				t.Fatal(err)
			}
		}
	}
}

func startVitalsBinary(t *testing.T, binary string, baseEnv map[string]string, panicTask string) *binaryProcess {
	t.Helper()
	logs := &synchronizedBuffer{}
	command := exec.Command(binary)
	command.Stdout = logs
	command.Stderr = logs
	env := make([]string, 0, len(baseEnv)+3)
	env = append(env, "PATH="+os.Getenv("PATH"), "HOME="+os.Getenv("HOME"), "TMPDIR="+os.TempDir())
	for key, value := range baseEnv {
		env = append(env, key+"="+value)
	}
	if panicTask != "" {
		env = append(env, "VITALS_TEST_PANIC_TASK="+panicTask)
	}
	command.Env = env
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	wait := make(chan error, 1)
	go func() { wait <- command.Wait() }()
	return &binaryProcess{command: command, logs: logs, wait: wait}
}

func stopVitalsBinary(t *testing.T, process *binaryProcess) {
	t.Helper()
	if process == nil || process.command.Process == nil {
		return
	}
	if err := process.command.Process.Signal(syscall.SIGTERM); err != nil && !strings.Contains(err.Error(), "process already finished") {
		t.Fatal(err)
	}
	select {
	case err := <-process.wait:
		if err != nil {
			t.Fatalf("vitals exit: %v logs=%s", err, process.logs.String())
		}
	case <-time.After(5 * time.Second):
		_ = process.command.Process.Kill()
		t.Fatal("vitals did not stop after SIGTERM")
	}
}

func reservePort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	return listener.Addr().(*net.TCPAddr).Port
}

func waitForHTTP(t *testing.T, process *binaryProcess, url string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		response, err := http.Get(url)
		if err == nil {
			response.Body.Close()
			return
		}
		select {
		case err := <-process.wait:
			t.Fatalf("vitals exited before HTTP ready: %v logs=%s", err, process.logs.String())
		default:
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("HTTP not ready: %s logs=%s", url, process.logs.String())
}

func waitForBackgroundDegraded(t *testing.T, process *binaryProcess, baseURL string, timeout time.Duration) {
	t.Helper()
	waitForHTTP(t, process, baseURL+"/health", timeout)
	client := &binaryClient{baseURL: baseURL, cookies: map[string]string{}, client: &http.Client{Timeout: time.Second}}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		response := client.request(t, http.MethodGet, "/health", "", nil)
		if response.status == http.StatusOK && strings.Contains(response.body, `"background":"degraded"`) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("background did not degrade logs=%s", process.logs.String())
}

func binaryLogin(t *testing.T, client *binaryClient) string {
	t.Helper()
	csrf := client.fetchCSRF(t)
	password := client.request(t, http.MethodPost, "/api/auth/password", csrf, map[string]string{"username": binaryAdminUser, "password": binaryPassword})
	if password.status != http.StatusOK {
		t.Fatalf("password login=%d body=%s", password.status, password.body)
	}
	var challenge struct {
		Challenge string `json:"challenge"`
	}
	decodeBinaryJSON(t, password.body, &challenge)
	code, err := hotp.GenerateCodeCustom(binaryAdminTOTP, uint64(time.Now().Unix()/30), hotp.ValidateOpts{Digits: otp.DigitsSix, Algorithm: otp.AlgorithmSHA1})
	if err != nil {
		t.Fatal(err)
	}
	totpResponse := client.request(t, http.MethodPost, "/api/auth/totp", csrf, map[string]string{"challenge": challenge.Challenge, "code": code})
	if totpResponse.status != http.StatusNoContent {
		t.Fatalf("TOTP login=%d body=%s", totpResponse.status, totpResponse.body)
	}
	return client.fetchCSRF(t)
}

func (client *binaryClient) fetchCSRF(t *testing.T) string {
	t.Helper()
	response := client.request(t, http.MethodGet, "/api/auth/csrf", "", nil)
	if response.status != http.StatusOK {
		t.Fatalf("csrf=%d body=%s", response.status, response.body)
	}
	var result struct {
		Token string `json:"csrf_token"`
	}
	decodeBinaryJSON(t, response.body, &result)
	return result.Token
}

func (client *binaryClient) request(t *testing.T, method, path, csrf string, payload any) binaryResponse {
	t.Helper()
	var body io.Reader
	if payload != nil {
		encoded, err := json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
		body = bytes.NewReader(encoded)
	}
	request, err := http.NewRequest(method, client.baseURL+path, body)
	if err != nil {
		t.Fatal(err)
	}
	if payload != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if client.origin != "" && method != http.MethodGet {
		request.Header.Set("Origin", client.origin)
	}
	if csrf != "" {
		request.Header.Set("X-CSRF-Token", csrf)
	}
	for name, value := range client.cookies {
		request.AddCookie(&http.Cookie{Name: name, Value: value})
	}
	response, err := client.client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	for _, cookie := range response.Cookies() {
		if cookie.MaxAge < 0 {
			delete(client.cookies, cookie.Name)
		} else {
			client.cookies[cookie.Name] = cookie.Value
		}
	}
	defer response.Body.Close()
	contents, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		t.Fatal(err)
	}
	return binaryResponse{status: response.StatusCode, body: string(contents)}
}

func waitForBody(t *testing.T, timeout time.Duration, request func() binaryResponse, contains string) binaryResponse {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var response binaryResponse
	for time.Now().Before(deadline) {
		response = request()
		if response.status == http.StatusOK && strings.Contains(response.body, contains) {
			return response
		}
		time.Sleep(30 * time.Millisecond)
	}
	t.Fatalf("response did not contain %q: status=%d body=%s", contains, response.status, response.body)
	return response
}

func waitForBodyWithout(t *testing.T, timeout time.Duration, request func() binaryResponse, forbidden string) binaryResponse {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var response binaryResponse
	for time.Now().Before(deadline) {
		response = request()
		if response.status == http.StatusOK && !strings.Contains(response.body, forbidden) {
			return response
		}
		time.Sleep(30 * time.Millisecond)
	}
	t.Fatalf("response still contains %q: status=%d body=%s", forbidden, response.status, response.body)
	return response
}

func monitorAccountID(t *testing.T, dbPath, providerID string) int64 {
	t.Helper()
	database := openSQLite(t, dbPath)
	defer database.Close()
	var id int64
	if err := database.QueryRow("SELECT id FROM accounts WHERE provider_account_id=?", providerID).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func openSQLite(t *testing.T, dbPath string) *sql.DB {
	t.Helper()
	database, err := sql.Open("sqlite", "file:"+filepath.ToSlash(dbPath)+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	return database
}

func decodeBinaryJSON(t *testing.T, body string, target any) {
	t.Helper()
	if err := json.Unmarshal([]byte(body), target); err != nil {
		t.Fatalf("decode JSON: %v body=%s", err, body)
	}
}
