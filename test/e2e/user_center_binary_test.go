package e2e

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"
)

const ucBinaryTicketInvalid = `{"error":{"code":"ticket_invalid","message":"登录状态已过期，请刷新页面后重试"},"ok":false}`

// ucBinarySecret 生成确定性的「像随机」测试材料，不是任何真实密钥。
func ucBinarySecret(label string) string {
	sum := sha256.Sum256([]byte("vitals-user-center-e2e:" + label))
	return hex.EncodeToString(sum[:])
}

func ucBinaryTicket(t *testing.T, keyHex, uid, nonce string, exp time.Time) string {
	t.Helper()
	key, err := hex.DecodeString(keyHex)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(map[string]any{"v": 1, "svc": "sharegpt", "uid": uid, "nonce": nonce, "exp": exp.Unix()})
	if err != nil {
		t.Fatal(err)
	}
	encoded := base64.RawURLEncoding.EncodeToString(payload)
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(encoded))
	return encoded + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func ucBinaryRequest(t *testing.T, method, url string, payload any, headers map[string]string) binaryResponse {
	t.Helper()
	var body io.Reader
	if payload != nil {
		encoded, err := json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
		body = bytes.NewReader(encoded)
	}
	request, err := http.NewRequest(method, url, body)
	if err != nil {
		t.Fatal(err)
	}
	if payload != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	for name, value := range headers {
		request.Header.Set(name, value)
	}
	response, err := (&http.Client{Timeout: 5 * time.Second}).Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	contents, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		t.Fatal(err)
	}
	return binaryResponse{status: response.StatusCode, body: string(contents)}
}

func TestVitalsBinaryUserCenterBinding(t *testing.T) {
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
	ticketKey := ucBinarySecret("ticket")
	internalToken := ucBinarySecret("token")
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
		"JWT_SIGNING_KEY":                     base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{'j'}, 32)),
		"RATE_LIMIT_KEY":                      base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{'r'}, 32)),
		"APP_ORIGIN":                          origin,
		"TRUST_LOOPBACK_PROXY":                "false",
		"VITALS_MONITOR_COMPAT_HTTP_ENABLED":  "false",
		"UC_BIND_TICKET_KEY":                  ticketKey,
		"UC_INTERNAL_TOKEN":                   internalToken,
		"UC_DETAIL_ORIGIN":                    "https://gpt.example.test",
	}

	// 启动门禁 fail-closed：弱值 / 复用 / 只配一半都必须拒绝启动。
	for name, mutate := range map[string]func(map[string]string){
		"weak ticket key":         func(e map[string]string) { e["UC_BIND_TICKET_KEY"] = strings.Repeat("0", 64) },
		"ticket equals token":     func(e map[string]string) { e["UC_INTERNAL_TOKEN"] = e["UC_BIND_TICKET_KEY"] },
		"token without ticket":    func(e map[string]string) { delete(e, "UC_BIND_TICKET_KEY") },
		"missing detail origin":   func(e map[string]string) { delete(e, "UC_DETAIL_ORIGIN") },
		"placeholder token value": func(e map[string]string) { e["UC_INTERNAL_TOKEN"] = "__REPLACE_WITH_HEX_32_BYTES__" },
	} {
		rejected := make(map[string]string, len(env))
		for key, value := range env {
			rejected[key] = value
		}
		mutate(rejected)
		process := startVitalsBinary(t, binaryPath, rejected, "")
		select {
		case err := <-process.wait:
			if err == nil || !strings.Contains(process.logs.String(), "configuration_rejected") {
				t.Fatalf("%s: expected configuration rejection, err=%v logs=%s", name, err, process.logs.String())
			}
			for _, secret := range []string{ticketKey, internalToken} {
				if strings.Contains(process.logs.String(), secret) {
					t.Fatalf("%s: rejection log leaked a secret", name)
				}
			}
		case <-time.After(5 * time.Second):
			_ = process.command.Process.Kill()
			t.Fatalf("%s: vitals started with a rejected user center configuration", name)
		}
	}

	process := startVitalsBinary(t, binaryPath, env, "")
	defer stopVitalsBinary(t, process)
	waitForHTTP(t, process, baseURL+"/health", 5*time.Second)
	admin := &binaryClient{baseURL: baseURL, origin: origin, cookies: map[string]string{}, client: &http.Client{Timeout: 5 * time.Second}}
	csrf := binaryLogin(t, admin)

	pull := admin.request(t, http.MethodPost, "/api/admin/accounts/pull-monitor", csrf, map[string]any{})
	if pull.status != http.StatusOK {
		t.Fatalf("monitor pull=%d body=%s", pull.status, pull.body)
	}
	var pulled struct {
		Accounts []struct {
			ID               int64  `json:"id"`
			DisplayUsername  string `json:"display_username"`
			AccountExpiry    string `json:"account_expiry"`
			MonitorStatus    string `json:"monitor_status"`
			MonitorAccountID string `json:"monitor_account_id"`
		} `json:"accounts"`
	}
	decodeBinaryJSON(t, pull.body, &pulled)
	completed := 0
	for _, item := range pulled.Accounts {
		if item.MonitorAccountID == "pending-binary" || item.MonitorStatus == "dead_banned" {
			continue
		}
		updated := admin.request(t, http.MethodPut, "/api/admin/accounts/"+strconv.FormatInt(item.ID, 10), csrf, map[string]any{
			"display_username":     item.DisplayUsername,
			"display_password":     "DISPLAY_PASSWORD_BINARY_SENTINEL_" + item.MonitorAccountID,
			"display_2fa_secret":   binaryDisplayTOTP,
			"account_expiry":       item.AccountExpiry,
			"max_concurrent_users": 3,
			"status":               "pending_credentials",
			"monitor_status":       item.MonitorStatus,
			"monitor_account_id":   item.MonitorAccountID,
		})
		if updated.status != http.StatusOK {
			t.Fatalf("complete %s=%d body=%s", item.MonitorAccountID, updated.status, updated.body)
		}
		completed++
	}
	if completed == 0 {
		t.Fatal("no allocatable accounts were prepared")
	}
	generatedResponse := admin.request(t, http.MethodPost, "/api/admin/cards/generate", csrf, map[string]any{"quantity": 4, "duration_days": 30})
	if generatedResponse.status != http.StatusCreated {
		t.Fatalf("generate cards=%d body=%s", generatedResponse.status, generatedResponse.body)
	}
	var generated struct {
		Cards []generatedCard `json:"cards"`
	}
	decodeBinaryJSON(t, generatedResponse.body, &generated)
	if len(generated.Cards) != 4 {
		t.Fatalf("generated cards=%d", len(generated.Cards))
	}
	anonymousCard, boundCard, replayCard := generated.Cards[0], generated.Cards[1], generated.Cards[2]
	token := map[string]string{"X-UC-Token": internalToken}
	eventsURL := baseURL + "/internal/uc/events?after=0&limit=200"

	// 无票据：匿名兑换照旧，不写事件。
	anonymous := ucBinaryRequest(t, http.MethodPost, baseURL+"/api/redeem", map[string]string{"code": anonymousCard.Code}, nil)
	if anonymous.status != http.StatusOK || !strings.Contains(anonymous.body, `"allocation_state":"primary"`) {
		t.Fatalf("anonymous redeem=%d body=%s", anonymous.status, anonymous.body)
	}
	if events := ucBinaryRequest(t, http.MethodGet, eventsURL, nil, token); events.status != http.StatusOK || !strings.Contains(events.body, `"events":[]`) {
		t.Fatalf("anonymous redeem must not write events: %d %s", events.status, events.body)
	}

	// 带有效票据：扣码同时写事件。
	ticket := ucBinaryTicket(t, ticketKey, "u_9f3k2m7q", "e2eNonce00000000000001", time.Now().Add(2*time.Minute))
	bound := ucBinaryRequest(t, http.MethodPost, baseURL+"/api/redeem", map[string]string{"code": boundCard.Code}, map[string]string{"X-Bind-Ticket": ticket})
	if bound.status != http.StatusOK {
		t.Fatalf("ticket redeem=%d body=%s", bound.status, bound.body)
	}
	boundRef := "card:" + strconv.FormatInt(boundCard.ID, 10)

	// 重放 / 过期 / 错签名：400 ticket_invalid，卡密不被消耗。
	for name, rejected := range map[string]string{
		"replayed":  ticket,
		"expired":   ucBinaryTicket(t, ticketKey, "u_9f3k2m7q", "e2eNonce00000000000002", time.Now().Add(-time.Minute)),
		"wrong key": ucBinaryTicket(t, ucBinarySecret("forged"), "u_9f3k2m7q", "e2eNonce00000000000003", time.Now().Add(time.Minute)),
	} {
		response := ucBinaryRequest(t, http.MethodPost, baseURL+"/api/redeem", map[string]string{"code": replayCard.Code}, map[string]string{"X-Bind-Ticket": rejected})
		if response.status != http.StatusBadRequest || response.body != ucBinaryTicketInvalid {
			t.Fatalf("%s ticket=%d body=%s", name, response.status, response.body)
		}
	}
	if query := ucBinaryRequest(t, http.MethodPost, baseURL+"/api/cards/query", map[string]string{"code": replayCard.Code}, nil); query.status == http.StatusOK {
		t.Fatalf("rejected tickets must not consume the card: %s", query.body)
	}

	// 查询已兑换卡密（再带一张新票据）：返回原结果，不写事件。
	again := ucBinaryRequest(t, http.MethodPost, baseURL+"/api/redeem", map[string]string{"code": boundCard.Code},
		map[string]string{"X-Bind-Ticket": ucBinaryTicket(t, ticketKey, "u_other", "e2eNonce00000000000004", time.Now().Add(time.Minute))})
	if again.status != http.StatusOK || !strings.Contains(again.body, `"card_id":`+strconv.FormatInt(boundCard.ID, 10)) {
		t.Fatalf("existing redeem=%d body=%s", again.status, again.body)
	}

	events := ucBinaryRequest(t, http.MethodGet, eventsURL, nil, token)
	var eventPage struct {
		Events []struct {
			ID             int64             `json:"id"`
			Kind           string            `json:"kind"`
			EntitlementRef string            `json:"entitlement_ref"`
			UserUID        string            `json:"user_uid"`
			CodeHash       string            `json:"code_hash"`
			Detail         map[string]string `json:"detail"`
		} `json:"events"`
		NextAfter int64 `json:"next_after"`
	}
	decodeBinaryJSON(t, events.body, &eventPage)
	codeSum := sha256.Sum256([]byte(boundCard.Code))
	if events.status != http.StatusOK || len(eventPage.Events) != 1 || eventPage.Events[0].Kind != "new" || eventPage.Events[0].EntitlementRef != boundRef ||
		eventPage.Events[0].UserUID != "u_9f3k2m7q" || eventPage.Events[0].CodeHash != hex.EncodeToString(codeSum[:]) || eventPage.Events[0].Detail["plan"] != "30天" || eventPage.NextAfter != eventPage.Events[0].ID {
		t.Fatalf("events=%d body=%s", events.status, events.body)
	}

	// 内部接口：无令牌 / 带转发头 → 404。
	for name, headers := range map[string]map[string]string{
		"no token":        nil,
		"wrong token":     {"X-UC-Token": ucBinarySecret("wrong")},
		"x-forwarded-for": {"X-UC-Token": internalToken, "X-Forwarded-For": "198.51.100.7"},
		"x-real-ip":       {"X-UC-Token": internalToken, "X-Real-IP": "198.51.100.7"},
	} {
		if response := ucBinaryRequest(t, http.MethodGet, eventsURL, nil, headers); response.status != http.StatusNotFound {
			t.Fatalf("%s internal events=%d body=%s", name, response.status, response.body)
		}
	}

	status := ucBinaryRequest(t, http.MethodPost, baseURL+"/internal/uc/status", map[string]any{"refs": []string{boundRef, "card:" + strconv.FormatInt(anonymousCard.ID, 10), "card:999999"}}, token)
	if status.status != http.StatusOK || !strings.Contains(status.body, `"`+boundRef+`":{"state":"active"`) || !strings.Contains(status.body, "＠***.test") ||
		strings.Contains(status.body, "card:"+strconv.FormatInt(anonymousCard.ID, 10)) {
		t.Fatalf("status=%d body=%s", status.status, status.body)
	}
	link := ucBinaryRequest(t, http.MethodPost, baseURL+"/internal/uc/detail-link", map[string]any{"ref": boundRef}, token)
	if link.status != http.StatusOK || link.body != `{"url":"https://gpt.example.test/#code=`+boundCard.Code+`"}` {
		t.Fatalf("detail-link=%d body=%s", link.status, link.body)
	}
	for _, body := range []string{events.body, status.body, link.body} {
		for _, forbidden := range []string{"DISPLAY_PASSWORD_BINARY_SENTINEL", binaryDisplayTOTP, "@example.test", anonymousCard.Code, replayCard.Code} {
			if strings.Contains(body, forbidden) {
				t.Fatalf("internal response leaked %q: %s", forbidden, body)
			}
		}
	}
	logs := process.logs.String()
	for _, forbidden := range []string{ticket, ticketKey, internalToken, boundCard.Code, "DISPLAY_PASSWORD_BINARY_SENTINEL"} {
		if strings.Contains(logs, forbidden) {
			t.Fatalf("logs leaked forbidden value")
		}
	}
	t.Logf("vitals_binary_user_center=pass config_gate=fail_closed anonymous=no_event ticket=event replay=400 expired=400 forged=400 existing=no_event internal_forwarded=404 status=masked detail_link=fragment")
}
