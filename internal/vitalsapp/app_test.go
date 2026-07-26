package vitalsapp

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"allocation-service/platform/supervisor"
	"github.com/gin-gonic/gin"
)

type fakeHealth struct{ err error }

func (f fakeHealth) Health(context.Context) error { return f.err }

type fakeBackground string

func (f fakeBackground) BackgroundStatus() string { return string(f) }

type fakeRoutes struct{}

func (fakeRoutes) RegisterPublicRoutes(router *gin.Engine) {
	router.GET("/", func(c *gin.Context) { c.String(http.StatusOK, "public-card-page") })
	router.GET("/admin/*path", func(c *gin.Context) { c.String(http.StatusOK, "admin-spa") })
}

func TestHealthBackgroundDegradedIsNonFatal(t *testing.T) {
	app := New(fakeHealth{}, fakeHealth{}, fakeBackground("degraded"), fakeRoutes{}, testLogger())
	response := request(app.Handler(), "/health")
	if response.Code != http.StatusOK || !bytes.Contains(response.Body.Bytes(), []byte(`"background":"degraded"`)) {
		t.Fatalf("health response = %d %s", response.Code, response.Body.String())
	}
	for _, path := range []string{"/", "/admin/", "/api/monitor/ping"} {
		if got := request(app.Handler(), path).Code; got != http.StatusOK {
			t.Fatalf("GET %s = %d, want 200", path, got)
		}
	}
}

func TestHealthDatabaseFailureIsFatal(t *testing.T) {
	app := New(fakeHealth{err: errors.New("db down")}, fakeHealth{}, fakeBackground("ok"), nil, testLogger())
	if got := request(app.Handler(), "/health").Code; got != http.StatusServiceUnavailable {
		t.Fatalf("health status = %d, want 503", got)
	}
}

func TestCompatibilityRoutesDefaultToNotFound(t *testing.T) {
	app := New(fakeHealth{}, fakeHealth{}, fakeBackground("ok"), fakeRoutes{}, testLogger())
	if got := request(app.Handler(), "/api/v1/monitor/accounts").Code; got != http.StatusNotFound {
		t.Fatalf("compatibility route status = %d, want 404", got)
	}
}

func TestSecurityHeadersUseStrictCSPWithoutInlineExecution(t *testing.T) {
	app := New(fakeHealth{}, fakeHealth{}, fakeBackground("ok"), fakeRoutes{}, testLogger())
	response := request(app.Handler(), "/")
	csp := response.Header().Get("Content-Security-Policy")
	for _, directive := range []string{"default-src 'self'", "script-src 'self'", "style-src 'self'", "connect-src 'self'", "object-src 'none'", "frame-ancestors 'none'"} {
		if !strings.Contains(csp, directive) {
			t.Fatalf("CSP missing %q: %s", directive, csp)
		}
	}
	if strings.Contains(csp, "unsafe-inline") || strings.Contains(csp, "unsafe-eval") {
		t.Fatalf("CSP permits inline execution: %s", csp)
	}
}

func TestBackgroundPanicLoopDoesNotBreakPublicOrOtherModuleRoutes(t *testing.T) {
	for _, test := range []struct {
		name       string
		task       string
		module     string
		otherRoute string
	}{
		{name: "monitor poller", task: "poller", module: "monitor", otherRoute: "/"},
		{name: "allocation replacement", task: "replacement", module: "allocation", otherRoute: "/api/monitor/ping"},
		{name: "allocation facade sync", task: "facade-sync", module: "allocation", otherRoute: "/api/monitor/ping"},
	} {
		t.Run(test.name, func(t *testing.T) {
			var logs bytes.Buffer
			logger := slog.New(slog.NewJSONHandler(&logs, nil))
			runner := supervisor.New(logger, supervisor.Config{PanicThreshold: 2, InitialBackoff: time.Millisecond, MaximumBackoff: time.Millisecond})
			if err := runner.GoManaged(context.Background(), test.task, test.module, func(context.Context) error {
				panic("token secret@example.test")
			}); err != nil {
				t.Fatal(err)
			}
			deadline := time.Now().Add(time.Second)
			for runner.BackgroundStatus() != "degraded" && time.Now().Before(deadline) {
				time.Sleep(time.Millisecond)
			}
			runner.Wait()
			app := New(fakeHealth{}, fakeHealth{}, runner, fakeRoutes{}, logger)
			for _, path := range []string{"/health", "/", test.otherRoute} {
				if got := request(app.Handler(), path).Code; got != http.StatusOK {
					t.Fatalf("GET %s = %d, want 200", path, got)
				}
			}
			if body := request(app.Handler(), "/health").Body.String(); !strings.Contains(body, `"background":"degraded"`) {
				t.Fatalf("health does not report degraded background: %s", body)
			}
			if strings.Contains(logs.String(), "token secret@example.test") || strings.Contains(logs.String(), "@example.test") {
				t.Fatal("panic value leaked into logs")
			}
			for _, field := range []string{"panic_type", "error_code", "run_id", test.module, test.task} {
				if !strings.Contains(logs.String(), field) {
					t.Fatalf("panic log missing %q", field)
				}
			}
		})
	}
}

func request(handler http.Handler, path string) *httptest.ResponseRecorder {
	result := httptest.NewRecorder()
	handler.ServeHTTP(result, httptest.NewRequest(http.MethodGet, path, nil))
	return result
}

func testLogger() *slog.Logger { return slog.New(slog.NewJSONHandler(&bytes.Buffer{}, nil)) }
