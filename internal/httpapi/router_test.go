package httpapi

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type healthStub struct{ err error }

func (h healthStub) Health(context.Context) error { return h.err }

func TestHealthz(t *testing.T) {
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	for _, tt := range []struct {
		name       string
		healthErr  error
		statusCode int
		body       string
	}{
		{"ok", nil, http.StatusOK, `"status":"ok"`},
		{"degraded", errors.New("sensitive database detail"), http.StatusServiceUnavailable, `"status":"degraded"`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodGet, "/healthz", nil)
			NewRouter(healthStub{err: tt.healthErr}, nil, nil, Config{}, logger).ServeHTTP(recorder, request)
			if recorder.Code != tt.statusCode || !strings.Contains(recorder.Body.String(), tt.body) {
				t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
			}
			if recorder.Header().Get("X-Request-ID") == "" || !strings.Contains(recorder.Body.String(), `"request_id"`) {
				t.Fatal("request id missing")
			}
			if strings.Contains(recorder.Body.String(), "sensitive") {
				t.Fatal("health response leaked database detail")
			}
		})
	}
}
