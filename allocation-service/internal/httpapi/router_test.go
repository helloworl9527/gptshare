package httpapi

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type healthStub struct {
	err error
}

func (h healthStub) Health(context.Context) error { return h.err }

func TestHealth(t *testing.T) {
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	for _, tt := range []struct {
		name       string
		healthErr  error
		statusCode int
		wantBody   string
	}{
		{"ok", nil, http.StatusOK, `"status":"ok"`},
		{"degraded", errors.New("sensitive database detail"), http.StatusServiceUnavailable, `"status":"degraded"`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodGet, "/health", nil)
			NewRouter(healthStub{err: tt.healthErr}, nil, Config{}, logger).ServeHTTP(recorder, request)
			if recorder.Code != tt.statusCode || !strings.Contains(recorder.Body.String(), tt.wantBody) {
				t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
			}
			if recorder.Header().Get("X-Request-ID") == "" || !strings.Contains(recorder.Body.String(), `"request_id"`) {
				t.Fatal("request id missing")
			}
			if strings.Contains(recorder.Body.String(), "sensitive") {
				t.Fatal("health response leaked error detail")
			}
		})
	}
}

func TestAdminStaticSPA(t *testing.T) {
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	router := NewRouter(healthStub{}, nil, Config{}, logger)
	for _, path := range []string{"/admin", "/admin/accounts", "/admin/cards"} {
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodGet, path, nil)
		router.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusOK || !strings.Contains(recorder.Header().Get("Content-Type"), "text/html") || !strings.Contains(recorder.Body.String(), `id="app"`) {
			t.Fatalf("admin spa path=%s status=%d content-type=%q body=%s", path, recorder.Code, recorder.Header().Get("Content-Type"), recorder.Body.String())
		}
	}
	assets, err := fs.Glob(adminStatic, "static/admin/assets/*.js")
	if err != nil || len(assets) == 0 {
		t.Fatalf("admin js asset missing err=%v", err)
	}
	assetPath := strings.TrimPrefix(assets[0], "static")
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, assetPath, nil)
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Header().Get("Content-Type"), "javascript") {
		t.Fatalf("admin asset status=%d content-type=%q", recorder.Code, recorder.Header().Get("Content-Type"))
	}
}

func TestUserPageUsesExternalAssetsUnderStrictCSP(t *testing.T) {
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	router := NewRouter(healthStub{}, nil, Config{}, logger)
	page := httptest.NewRecorder()
	router.ServeHTTP(page, httptest.NewRequest(http.MethodGet, "/", nil))
	csp := page.Header().Get("Content-Security-Policy")
	if page.Code != http.StatusOK || !strings.Contains(page.Body.String(), `href="/static/user.css"`) || !strings.Contains(page.Body.String(), `src="/static/user.js"`) {
		t.Fatalf("user page did not reference external assets status=%d body=%s", page.Code, page.Body.String())
	}
	if strings.Contains(page.Body.String(), "有效期") || !strings.Contains(page.Body.String(), "订阅剩余时间") {
		t.Fatalf("user page subscription wording mismatch: %s", page.Body.String())
	}
	for _, label := range []string{"复制账号", "复制密码", "复制验证码"} {
		if !strings.Contains(page.Body.String(), label) {
			t.Fatalf("user page missing copy control %s", label)
		}
	}
	if strings.Contains(page.Body.String(), "<style>") || strings.Contains(page.Body.String(), "<script>") && !strings.Contains(page.Body.String(), `src="/static/user.js"`) {
		t.Fatalf("user page contains inline style or script: %s", page.Body.String())
	}
	if csp == "" || strings.Contains(csp, "unsafe-inline") {
		t.Fatalf("strict CSP missing or relaxed: %q", csp)
	}
	for _, path := range []string{"/static/user.css", "/static/user.js"} {
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
		if recorder.Code != http.StatusOK || recorder.Body.Len() == 0 {
			t.Fatalf("asset %s status=%d len=%d", path, recorder.Code, recorder.Body.Len())
		}
	}
	if !strings.Contains(userPageJS, `"/api/redeem"`) || !strings.Contains(userPageJS, `"/api/cards/query"`) {
		t.Fatal("user page script must support query and automatic redeem flow")
	}
	if !strings.Contains(userPageJS, "navigator.clipboard.writeText") {
		t.Fatal("user page script must support one-click clipboard writes")
	}
}
