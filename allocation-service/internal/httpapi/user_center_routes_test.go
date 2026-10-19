package httpapi

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	allocatorsvc "allocation-service/internal/allocator"
	cardsvc "allocation-service/internal/card"
	"allocation-service/internal/credential"
	"allocation-service/internal/repository"
	"allocation-service/internal/store"
	"allocation-service/internal/usercenter"
	userquerysvc "allocation-service/internal/userquery"
	"github.com/gin-gonic/gin"
)

const (
	ucTestToken         = "3f6c1b0e9a7d24c58e1f0b2a6d9c4e7f1a3b5c7d9e0f2a4b6c8d0e1f3a5b7c9d"
	ucTicketInvalidBody = `{"error":{"code":"ticket_invalid","message":"登录状态已过期，请刷新页面后重试"},"ok":false}`
	ucPasswordSentinel  = "UC_HTTP_PASSWORD_SENTINEL"
	ucTOTPSentinel      = "JBSWY3DPEHPK3PXP"
	ucDisplayUsername   = "share-07@pool.example.test"
	ucMaskedUsername    = "share-07＠***.test"
	ucDetailOrigin      = "https://gpt.example.test"
)

var ucTicketKey = bytes.Repeat([]byte{0x6b}, 32)

type userCenterHTTPFixture struct {
	server *httptest.Server
	repo   *repository.Repository
	db     *sql.DB
	uc     *usercenter.Service
	nonce  int
}

func newUserCenterHTTPFixture(t *testing.T, enabled bool) *userCenterHTTPFixture {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "private")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	database, err := store.Open(context.Background(), filepath.Join(dir, "allocation.db"))
	if err != nil {
		t.Fatal(err)
	}
	keyring, err := credential.NewKeyring(map[string][]byte{"alloc-uc-k1": bytes.Repeat([]byte{4}, 32)}, "alloc-uc-k1")
	if err != nil {
		t.Fatal(err)
	}
	repo := repository.New(database.DB(), keyring)
	if _, err := repo.CreateAccount(context.Background(), repository.AccountSeed{
		DisplayUsername: ucDisplayUsername, DisplayPassword: ucPasswordSentinel, DisplayTOTPSecret: ucTOTPSentinel,
		AccountExpiry: time.Now().UTC().Add(25 * 24 * time.Hour), MaxConcurrentUsers: 10, MonitorStatus: "alive",
	}); err != nil {
		t.Fatal(err)
	}
	var uc *usercenter.Service
	if enabled {
		uc, err = usercenter.NewService(repo, usercenter.Config{TicketKey: ucTicketKey, InternalToken: ucTestToken, DetailOrigin: ucDetailOrigin})
		if err != nil {
			t.Fatal(err)
		}
	}
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(requestID())
	RegisterPublicAPIRoutes(router, Config{Cards: cardsvc.NewService(repo), Allocator: allocatorsvc.NewService(repo, nil), UserQuery: userquerysvc.NewService(repo), UserCenter: uc})
	RegisterUserCenterInternalRoutes(router, uc)
	server := httptest.NewServer(router)
	t.Cleanup(func() {
		server.Close()
		database.Close()
	})
	return &userCenterHTTPFixture{server: server, repo: repo, db: database.DB(), uc: uc}
}

func (f *userCenterHTTPFixture) card(t *testing.T, code string) int64 {
	t.Helper()
	id, err := f.repo.CreateCard(context.Background(), repository.CardSeed{CodeHash: cardsvc.HashCode(code), CodeSuffix: code[len(code)-4:], CodePlaintext: code, DurationDays: 30})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func (f *userCenterHTTPFixture) count(t *testing.T, query string, args ...any) int {
	t.Helper()
	var value int
	if err := f.db.QueryRow(query, args...).Scan(&value); err != nil {
		t.Fatal(err)
	}
	return value
}

func (f *userCenterHTTPFixture) cardStatus(t *testing.T, id int64) string {
	t.Helper()
	var status string
	if err := f.db.QueryRow(`SELECT status FROM cards WHERE id=?`, id).Scan(&status); err != nil {
		t.Fatal(err)
	}
	return status
}

func (f *userCenterHTTPFixture) nextNonce() string {
	f.nonce++
	value := fmt.Sprintf("%022d", f.nonce)
	return value
}

func signUserCenterTicket(t *testing.T, key []byte, payload map[string]any) string {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	encoded := base64.RawURLEncoding.EncodeToString(raw)
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(encoded))
	return encoded + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func (f *userCenterHTTPFixture) ticket(t *testing.T, change func(map[string]any)) string {
	t.Helper()
	payload := map[string]any{"v": 1, "svc": "sharegpt", "uid": "u_9f3k2m7q", "nonce": f.nextNonce(), "exp": time.Now().Add(2 * time.Minute).Unix()}
	if change != nil {
		change(payload)
	}
	return signUserCenterTicket(t, ucTicketKey, payload)
}

func (f *userCenterHTTPFixture) redeem(t *testing.T, code string, headers map[string]string) (int, string) {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"code": code})
	request, err := http.NewRequest(http.MethodPost, f.server.URL+"/api/redeem", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	for key, value := range headers {
		request.Header.Set(key, value)
	}
	return doUserCenterRequest(t, request)
}

func (f *userCenterHTTPFixture) internal(t *testing.T, method, path string, payload any, headers map[string]string) (int, string) {
	t.Helper()
	var body io.Reader
	if payload != nil {
		raw, _ := json.Marshal(payload)
		body = bytes.NewReader(raw)
	}
	request, err := http.NewRequest(method, f.server.URL+path, body)
	if err != nil {
		t.Fatal(err)
	}
	if payload != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	for key, value := range headers {
		request.Header.Set(key, value)
	}
	return doUserCenterRequest(t, request)
}

func doUserCenterRequest(t *testing.T, request *http.Request) (int, string) {
	t.Helper()
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	return response.StatusCode, string(raw)
}

func tokenHeaders() map[string]string { return map[string]string{"X-UC-Token": ucTestToken} }

func TestRedeemWithTicketRecordsEvent(t *testing.T) {
	f := newUserCenterHTTPFixture(t, true)
	code := "UCAB-CDEF-GHJK"
	cardID := f.card(t, code)
	status, body := f.redeem(t, code, map[string]string{"X-Bind-Ticket": f.ticket(t, nil)})
	if status != http.StatusOK || !strings.Contains(body, `"allocation_state":"primary"`) {
		t.Fatalf("ticket redeem status=%d body=%s", status, body)
	}
	sum := sha256.Sum256([]byte(code))
	var kind, ref, uid, codeHash string
	if err := f.db.QueryRow(`SELECT kind,entitlement_ref,user_uid,code_hash FROM redemption_events`).Scan(&kind, &ref, &uid, &codeHash); err != nil {
		t.Fatal(err)
	}
	if kind != "new" || ref != "card:"+strconv.FormatInt(cardID, 10) || uid != "u_9f3k2m7q" || codeHash != hex.EncodeToString(sum[:]) {
		t.Fatalf("event kind=%s ref=%s uid=%s hash=%s", kind, ref, uid, codeHash)
	}
	if got := f.count(t, `SELECT count(*) FROM used_bind_nonces`); got != 1 {
		t.Fatalf("nonces=%d want 1", got)
	}
}

func TestRedeemTicketFailuresAreRejectedNotDowngraded(t *testing.T) {
	f := newUserCenterHTTPFixture(t, true)
	replayed := f.ticket(t, nil)
	f.card(t, "UCRP-AAAA-BBBB")
	if status, body := f.redeem(t, "UCRP-AAAA-BBBB", map[string]string{"X-Bind-Ticket": replayed}); status != http.StatusOK {
		t.Fatalf("first redeem status=%d body=%s", status, body)
	}
	valid := f.ticket(t, nil)
	payloadPart, sigPart, _ := strings.Cut(valid, ".")
	cases := map[string]string{
		"replayed nonce":     replayed,
		"expired":            f.ticket(t, func(p map[string]any) { p["exp"] = time.Now().Add(-31 * time.Second).Unix() }),
		"wrong service":      f.ticket(t, func(p map[string]any) { p["svc"] = "othersvc" }),
		"wrong version":      f.ticket(t, func(p map[string]any) { p["v"] = 2 }),
		"wrong key":          signUserCenterTicket(t, bytes.Repeat([]byte{0x11}, 32), map[string]any{"v": 1, "svc": "sharegpt", "uid": "u_x", "nonce": f.nextNonce(), "exp": time.Now().Add(time.Minute).Unix()}),
		"tampered payload":   base64.RawURLEncoding.EncodeToString([]byte(`{"v":1,"svc":"sharegpt","uid":"u_attacker","nonce":"`+f.nextNonce()+`","exp":9999999999}`)) + "." + sigPart,
		"tampered signature": payloadPart + "." + strings.Repeat("A", len(sigPart)),
		"garbage":            "not-a-ticket",
		"empty":              "",
	}
	index := 0
	for name, ticket := range cases {
		index++
		code := "UCTF-" + strings.Repeat(string("23456789ABCDEFGHJKMNPQRSTUVWXYZ"[index]), 4) + "-ZZZZ"
		t.Run(name, func(t *testing.T) {
			cardID := f.card(t, code)
			eventsBefore := f.count(t, `SELECT count(*) FROM redemption_events`)
			request, err := http.NewRequest(http.MethodPost, f.server.URL+"/api/redeem", strings.NewReader(`{"code":"`+code+`"}`))
			if err != nil {
				t.Fatal(err)
			}
			request.Header.Set("Content-Type", "application/json")
			request.Header["X-Bind-Ticket"] = []string{ticket}
			status, body := doUserCenterRequest(t, request)
			if status != http.StatusBadRequest || body != ucTicketInvalidBody {
				t.Fatalf("status=%d body=%s", status, body)
			}
			if got := f.cardStatus(t, cardID); got != "unused" {
				t.Fatalf("rejected ticket consumed the card: %s", got)
			}
			if got := f.count(t, `SELECT count(*) FROM redemption_events`); got != eventsBefore {
				t.Fatalf("rejected ticket wrote an event")
			}
		})
	}
	t.Run("duplicate ticket headers", func(t *testing.T) {
		cardID := f.card(t, "UCDP-HHHH-JJJJ")
		request, _ := http.NewRequest(http.MethodPost, f.server.URL+"/api/redeem", strings.NewReader(`{"code":"UCDP-HHHH-JJJJ"}`))
		request.Header.Set("Content-Type", "application/json")
		request.Header["X-Bind-Ticket"] = []string{f.ticket(t, nil), f.ticket(t, nil)}
		if status, body := doUserCenterRequest(t, request); status != http.StatusBadRequest || body != ucTicketInvalidBody {
			t.Fatalf("status=%d body=%s", status, body)
		}
		if got := f.cardStatus(t, cardID); got != "unused" {
			t.Fatalf("card=%s", got)
		}
	})
}

func TestRedeemWithoutTicketIsUnchanged(t *testing.T) {
	for _, enabled := range []bool{true, false} {
		t.Run("user center enabled="+strconv.FormatBool(enabled), func(t *testing.T) {
			f := newUserCenterHTTPFixture(t, enabled)
			cardID := f.card(t, "UCAN-MMMM-NNNN")
			status, body := f.redeem(t, "UCAN-MMMM-NNNN", nil)
			if status != http.StatusOK || !strings.Contains(body, `"allocation_state":"primary"`) || !strings.Contains(body, `"display_username":"`+ucDisplayUsername+`"`) {
				t.Fatalf("anonymous redeem status=%d body=%s", status, body)
			}
			if got := f.cardStatus(t, cardID); got != "redeemed" {
				t.Fatalf("card=%s", got)
			}
			if got := f.count(t, `SELECT count(*) FROM redemption_events`) + f.count(t, `SELECT count(*) FROM used_bind_nonces`); got != 0 {
				t.Fatalf("anonymous redeem wrote %d binding rows", got)
			}
			again, againBody := f.redeem(t, "UCAN-MMMM-NNNN", nil)
			if again != http.StatusOK || !strings.Contains(againBody, `"card_id":`+strconv.FormatInt(cardID, 10)) {
				t.Fatalf("anonymous repeat status=%d body=%s", again, againBody)
			}
		})
	}
}

func TestRedeemWithTicketWhileDisabledIsRejected(t *testing.T) {
	f := newUserCenterHTTPFixture(t, false)
	cardID := f.card(t, "UCDS-PPPP-QQQQ")
	status, body := f.redeem(t, "UCDS-PPPP-QQQQ", map[string]string{"X-Bind-Ticket": f.ticket(t, nil)})
	if status != http.StatusBadRequest || body != ucTicketInvalidBody {
		t.Fatalf("status=%d body=%s", status, body)
	}
	if got := f.cardStatus(t, cardID); got != "unused" {
		t.Fatalf("card=%s", got)
	}
}

func TestRedeemAlreadyRedeemedWithTicketReturnsOriginalWithoutEvent(t *testing.T) {
	f := newUserCenterHTTPFixture(t, true)
	cardID := f.card(t, "UCQR-RRRR-SSSS")
	if status, body := f.redeem(t, "UCQR-RRRR-SSSS", map[string]string{"X-Bind-Ticket": f.ticket(t, nil)}); status != http.StatusOK {
		t.Fatalf("first status=%d body=%s", status, body)
	}
	secondTicket := f.ticket(t, func(p map[string]any) { p["uid"] = "u_someone_else" })
	status, body := f.redeem(t, "UCQR-RRRR-SSSS", map[string]string{"X-Bind-Ticket": secondTicket})
	if status != http.StatusOK || !strings.Contains(body, `"card_id":`+strconv.FormatInt(cardID, 10)) {
		t.Fatalf("query existing status=%d body=%s", status, body)
	}
	if got := f.count(t, `SELECT count(*) FROM redemption_events`); got != 1 {
		t.Fatalf("events=%d want 1", got)
	}
	if got := f.count(t, `SELECT count(*) FROM redemption_events WHERE user_uid='u_someone_else'`); got != 0 {
		t.Fatal("a second user must not be able to claim an already redeemed card")
	}
	if got := f.count(t, `SELECT count(*) FROM used_bind_nonces`); got != 1 {
		t.Fatalf("nonces=%d want 1 (second ticket must not be consumed)", got)
	}
}

func TestUserCenterInternalRoutesRequireTokenAndDirectLoopback(t *testing.T) {
	f := newUserCenterHTTPFixture(t, true)
	routes := []struct {
		method  string
		path    string
		payload any
	}{
		{http.MethodGet, "/internal/uc/events", nil},
		{http.MethodPost, "/internal/uc/status", map[string]any{"refs": []string{"card:1"}}},
		{http.MethodPost, "/internal/uc/detail-link", map[string]any{"ref": "card:1"}},
	}
	denied := map[string]map[string]string{
		"no token":          nil,
		"wrong token":       {"X-UC-Token": strings.Repeat("0", 64)},
		"token prefix":      {"X-UC-Token": ucTestToken[:32]},
		"x-forwarded-for":   {"X-UC-Token": ucTestToken, "X-Forwarded-For": "127.0.0.1"},
		"x-real-ip":         {"X-UC-Token": ucTestToken, "X-Real-IP": "127.0.0.1"},
		"forwarded":         {"X-UC-Token": ucTestToken, "Forwarded": "for=127.0.0.1"},
		"cf-connecting-ip":  {"X-UC-Token": ucTestToken, "CF-Connecting-IP": "203.0.113.9"},
		"x-forwarded-proto": {"X-UC-Token": ucTestToken, "X-Forwarded-Proto": "https"},
	}
	for _, route := range routes {
		for name, headers := range denied {
			status, body := f.internal(t, route.method, route.path, route.payload, headers)
			if status != http.StatusNotFound || body != "" {
				t.Fatalf("%s %s (%s) status=%d body=%s", route.method, route.path, name, status, body)
			}
		}
		// 鉴权通过后 detail-link 对未知 ref 也是 404，但带业务错误体；中间件拒绝时响应体为空。
		status, body := f.internal(t, route.method, route.path, route.payload, tokenHeaders())
		if route.path == "/internal/uc/detail-link" {
			if status != http.StatusNotFound || !strings.Contains(body, `"not_found"`) {
				t.Fatalf("%s %s with token status=%d body=%s", route.method, route.path, status, body)
			}
		} else if status != http.StatusOK {
			t.Fatalf("%s %s with token status=%d body=%s", route.method, route.path, status, body)
		}
	}
}

func TestUserCenterInternalRoutesRejectNonLoopbackPeer(t *testing.T) {
	f := newUserCenterHTTPFixture(t, true)
	gin.SetMode(gin.TestMode)
	router := gin.New()
	RegisterUserCenterInternalRoutes(router, f.uc)
	for _, remote := range []string{"192.0.2.10:4431", "[2001:db8::1]:4431", "10.0.0.1:80", "garbage"} {
		request := httptest.NewRequest(http.MethodGet, "/internal/uc/events", nil)
		request.RemoteAddr = remote
		request.Header.Set("X-UC-Token", ucTestToken)
		response := httptest.NewRecorder()
		router.ServeHTTP(response, request)
		if response.Code != http.StatusNotFound {
			t.Fatalf("remote %s status=%d", remote, response.Code)
		}
	}
	for _, remote := range []string{"127.0.0.1:4431", "[::1]:4431"} {
		request := httptest.NewRequest(http.MethodGet, "/internal/uc/events", nil)
		request.RemoteAddr = remote
		request.Header.Set("X-UC-Token", ucTestToken)
		response := httptest.NewRecorder()
		router.ServeHTTP(response, request)
		if response.Code != http.StatusOK {
			t.Fatalf("loopback %s status=%d body=%s", remote, response.Code, response.Body.String())
		}
	}
}

func TestUserCenterInternalRoutesAbsentWhenDisabled(t *testing.T) {
	f := newUserCenterHTTPFixture(t, false)
	for _, path := range []string{"/internal/uc/events", "/internal/uc/status", "/internal/uc/detail-link"} {
		if status, _ := f.internal(t, http.MethodGet, path, nil, tokenHeaders()); status != http.StatusNotFound {
			t.Fatalf("disabled %s status=%d", path, status)
		}
	}
}

type ucEventsResponse struct {
	Events []struct {
		ID             int64             `json:"id"`
		EventUID       string            `json:"event_uid"`
		Kind           string            `json:"kind"`
		CodeHash       string            `json:"code_hash"`
		EntitlementRef string            `json:"entitlement_ref"`
		UserUID        string            `json:"user_uid"`
		SucceededAt    string            `json:"succeeded_at"`
		Detail         map[string]string `json:"detail"`
	} `json:"events"`
	NextAfter int64 `json:"next_after"`
}

func TestUserCenterEventsPagination(t *testing.T) {
	f := newUserCenterHTTPFixture(t, true)
	var codes []string
	for index := 0; index < 5; index++ {
		code := "UCPG-" + strings.Repeat(string("ABCDE"[index]), 4) + "-WXYZ"
		codes = append(codes, code)
		f.card(t, code)
		if status, body := f.redeem(t, code, map[string]string{"X-Bind-Ticket": f.ticket(t, nil)}); status != http.StatusOK {
			t.Fatalf("redeem %s status=%d body=%s", code, status, body)
		}
	}
	f.card(t, "UCPG-FFFF-WXYZ")
	if status, _ := f.redeem(t, "UCPG-FFFF-WXYZ", nil); status != http.StatusOK {
		t.Fatal("anonymous redeem failed")
	}

	var all []int64
	after := int64(0)
	for round := 0; round < 10; round++ {
		status, body := f.internal(t, http.MethodGet, "/internal/uc/events?after="+strconv.FormatInt(after, 10)+"&limit=2", nil, tokenHeaders())
		if status != http.StatusOK {
			t.Fatalf("events status=%d body=%s", status, body)
		}
		var page ucEventsResponse
		if err := json.Unmarshal([]byte(body), &page); err != nil {
			t.Fatal(err)
		}
		if len(page.Events) > 2 {
			t.Fatalf("page exceeded limit: %d", len(page.Events))
		}
		if len(page.Events) == 0 {
			if page.NextAfter != after {
				t.Fatalf("empty page next_after=%d want %d", page.NextAfter, after)
			}
			break
		}
		for _, event := range page.Events {
			if event.ID <= after || event.Kind != "new" || event.UserUID != "u_9f3k2m7q" || event.Detail["plan"] != "30天" || event.Detail["expire_at"] == "" || !strings.HasPrefix(event.EntitlementRef, "card:") || len(event.CodeHash) != 64 || len(event.EventUID) != 36 {
				t.Fatalf("unexpected event: %+v", event)
			}
			all = append(all, event.ID)
			after = event.ID
		}
		if page.NextAfter != after {
			t.Fatalf("next_after=%d want %d", page.NextAfter, after)
		}
		for _, forbidden := range append([]string{ucPasswordSentinel, ucTOTPSentinel, "share-07"}, codes...) {
			if strings.Contains(body, forbidden) {
				t.Fatalf("events leaked %q: %s", forbidden, body)
			}
		}
	}
	if len(all) != 5 {
		t.Fatalf("paged %d events want 5 (anonymous redeem must not appear)", len(all))
	}

	status, body := f.internal(t, http.MethodGet, "/internal/uc/events?limit=100000", nil, tokenHeaders())
	if status != http.StatusOK {
		t.Fatalf("large limit status=%d", status)
	}
	var clamped ucEventsResponse
	if err := json.Unmarshal([]byte(body), &clamped); err != nil || len(clamped.Events) != 5 {
		t.Fatalf("clamped events=%d err=%v", len(clamped.Events), err)
	}
	for _, query := range []string{"after=-1", "after=abc", "limit=0", "limit=-5", "limit=x"} {
		if status, body := f.internal(t, http.MethodGet, "/internal/uc/events?"+query, nil, tokenHeaders()); status != http.StatusBadRequest {
			t.Fatalf("%s status=%d body=%s", query, status, body)
		}
	}
}

func TestUserCenterStatusAndDetailLinkDoNotLeakSecrets(t *testing.T) {
	f := newUserCenterHTTPFixture(t, true)
	boundCode := "UCST-AAAA-BBBB"
	boundID := f.card(t, boundCode)
	anonymousCode := "UCST-CCCC-DDDD"
	anonymousID := f.card(t, anonymousCode)
	if status, body := f.redeem(t, boundCode, map[string]string{"X-Bind-Ticket": f.ticket(t, nil)}); status != http.StatusOK {
		t.Fatalf("bound redeem status=%d body=%s", status, body)
	}
	if status, _ := f.redeem(t, anonymousCode, nil); status != http.StatusOK {
		t.Fatal("anonymous redeem failed")
	}
	boundRef := "card:" + strconv.FormatInt(boundID, 10)
	anonymousRef := "card:" + strconv.FormatInt(anonymousID, 10)

	status, body := f.internal(t, http.MethodPost, "/internal/uc/status", map[string]any{"refs": []string{boundRef, anonymousRef, "card:99999", "account:1", "garbage"}}, tokenHeaders())
	if status != http.StatusOK {
		t.Fatalf("status endpoint=%d body=%s", status, body)
	}
	var parsed struct {
		Items map[string]struct {
			State    string            `json:"state"`
			Title    string            `json:"title"`
			ExpireAt string            `json:"expire_at"`
			Display  map[string]string `json:"display"`
		} `json:"items"`
	}
	if err := json.Unmarshal([]byte(body), &parsed); err != nil {
		t.Fatal(err)
	}
	if len(parsed.Items) != 1 {
		t.Fatalf("only the bound ref may appear: %s", body)
	}
	item, ok := parsed.Items[boundRef]
	if !ok || item.State != "active" || item.Title == "" || len(item.ExpireAt) != len("2006-01-02") || item.Display["当前账号"] != ucMaskedUsername {
		t.Fatalf("unexpected status item: %s", body)
	}
	for _, forbidden := range []string{ucPasswordSentinel, ucTOTPSentinel, ucDisplayUsername, "pool.example", boundCode, anonymousCode} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("status leaked %q: %s", forbidden, body)
		}
	}

	status, body = f.internal(t, http.MethodPost, "/internal/uc/detail-link", map[string]any{"ref": boundRef}, tokenHeaders())
	if status != http.StatusOK || body != `{"url":"`+ucDetailOrigin+`/#code=`+boundCode+`"}` {
		t.Fatalf("detail-link status=%d body=%s", status, body)
	}
	for _, forbidden := range []string{ucPasswordSentinel, ucTOTPSentinel, ucDisplayUsername, "?code=", "/" + boundCode} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("detail-link leaked %q: %s", forbidden, body)
		}
	}
	for _, ref := range []string{anonymousRef, "card:99999", "account:1", ""} {
		if status, body := f.internal(t, http.MethodPost, "/internal/uc/detail-link", map[string]any{"ref": ref}, tokenHeaders()); status == http.StatusOK || strings.Contains(body, anonymousCode) {
			t.Fatalf("detail-link for %q status=%d body=%s", ref, status, body)
		}
	}
}

func TestUserCenterStatusStates(t *testing.T) {
	f := newUserCenterHTTPFixture(t, true)
	code := "UCSS-EEEE-FFFF"
	cardID := f.card(t, code)
	if status, _ := f.redeem(t, code, map[string]string{"X-Bind-Ticket": f.ticket(t, nil)}); status != http.StatusOK {
		t.Fatal("redeem failed")
	}
	ref := "card:" + strconv.FormatInt(cardID, 10)
	stateFor := func() string {
		t.Helper()
		status, body := f.internal(t, http.MethodPost, "/internal/uc/status", map[string]any{"refs": []string{ref}}, tokenHeaders())
		if status != http.StatusOK {
			t.Fatalf("status=%d body=%s", status, body)
		}
		var parsed struct {
			Items map[string]struct {
				State   string            `json:"state"`
				Display map[string]string `json:"display"`
			} `json:"items"`
		}
		if err := json.Unmarshal([]byte(body), &parsed); err != nil {
			t.Fatal(err)
		}
		if parsed.Items[ref].State == "expired" && len(parsed.Items[ref].Display) != 0 {
			t.Fatalf("expired entitlement must not display an account: %s", body)
		}
		return parsed.Items[ref].State
	}
	if got := stateFor(); got != "active" {
		t.Fatalf("state=%s want active", got)
	}
	if _, err := f.db.Exec(`UPDATE cards SET expires_at=? WHERE id=?`, time.Now().UTC().Add(48*time.Hour).Format(time.RFC3339Nano), cardID); err != nil {
		t.Fatal(err)
	}
	if got := stateFor(); got != "expiring" {
		t.Fatalf("state=%s want expiring", got)
	}
	if _, err := f.db.Exec(`UPDATE cards SET expires_at=? WHERE id=?`, time.Now().UTC().Add(-time.Hour).Format(time.RFC3339Nano), cardID); err != nil {
		t.Fatal(err)
	}
	if got := stateFor(); got != "expired" {
		t.Fatalf("state=%s want expired", got)
	}
}

func TestUserCenterDistinctCustomersOnSameSharedAccountStaySeparate(t *testing.T) {
	f := newUserCenterHTTPFixture(t, true)
	first := f.card(t, "UCSH-AAAA-2222")
	second := f.card(t, "UCSH-BBBB-3333")
	if status, _ := f.redeem(t, "UCSH-AAAA-2222", map[string]string{"X-Bind-Ticket": f.ticket(t, func(p map[string]any) { p["uid"] = "u_alice" })}); status != http.StatusOK {
		t.Fatal("first redeem failed")
	}
	if status, _ := f.redeem(t, "UCSH-BBBB-3333", map[string]string{"X-Bind-Ticket": f.ticket(t, func(p map[string]any) { p["uid"] = "u_bob" })}); status != http.StatusOK {
		t.Fatal("second redeem failed")
	}
	var sameAccount int
	if err := f.db.QueryRow(`SELECT count(DISTINCT account_id) FROM allocations`).Scan(&sameAccount); err != nil || sameAccount != 1 {
		t.Fatalf("fixture expects both customers on the same shared account: %d %v", sameAccount, err)
	}
	rows := map[string]string{}
	result, err := f.db.Query(`SELECT user_uid,entitlement_ref FROM redemption_events ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer result.Close()
	for result.Next() {
		var uid, ref string
		if err := result.Scan(&uid, &ref); err != nil {
			t.Fatal(err)
		}
		rows[uid] = ref
	}
	if rows["u_alice"] != "card:"+strconv.FormatInt(first, 10) || rows["u_bob"] != "card:"+strconv.FormatInt(second, 10) {
		t.Fatalf("entitlements must follow each customer's own card: %+v", rows)
	}
}
