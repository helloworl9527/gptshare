package supervisor

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestGoManagedRecoversAndRestarts(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var attempts atomic.Int32
	runner := New(slog.New(slog.NewJSONHandler(&bytes.Buffer{}, nil)), Config{InitialBackoff: time.Millisecond, MaximumBackoff: time.Millisecond})
	if err := runner.GoManaged(ctx, "poller", "monitor", func(ctx context.Context) error {
		if attempts.Add(1) == 1 {
			panic("must not escape")
		}
		<-ctx.Done()
		return ctx.Err()
	}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return attempts.Load() >= 2 })
	if got := runner.BackgroundStatus(); got != "ok" {
		t.Fatalf("background status = %q, want ok", got)
	}
	cancel()
	runner.Wait()
}

func TestGoManagedStopsAtTerminalDegradedThreshold(t *testing.T) {
	var attempts atomic.Int32
	runner := New(slog.New(slog.NewJSONHandler(&bytes.Buffer{}, nil)), Config{PanicThreshold: 3, InitialBackoff: time.Millisecond, MaximumBackoff: time.Millisecond})
	if err := runner.GoManaged(context.Background(), "replacement", "allocation", func(context.Context) error {
		attempts.Add(1)
		panic("persistent failure")
	}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return runner.BackgroundStatus() == "degraded" })
	runner.Wait()
	if got := attempts.Load(); got != 3 {
		t.Fatalf("attempts = %d, want 3", got)
	}
}

func TestPanicLogOmitsRecoveredValueAndPII(t *testing.T) {
	var output bytes.Buffer
	runner := New(slog.New(slog.NewJSONHandler(&output, nil)), Config{PanicThreshold: 1, InitialBackoff: time.Millisecond})
	secret := "token password@example.test"
	if err := runner.GoManaged(context.Background(), "outbox", "monitor", func(context.Context) error { panic(secret) }); err != nil {
		t.Fatal(err)
	}
	runner.Wait()
	logText := output.String()
	if strings.Contains(logText, secret) || strings.Contains(logText, "@example.test") {
		t.Fatal("panic value or PII leaked into structured log")
	}
	for _, field := range []string{"\"module\"", "\"task\"", "\"panic_type\"", "\"error_code\"", "\"run_id\""} {
		if !strings.Contains(logText, field) {
			t.Fatalf("log does not contain %s", field)
		}
	}
}

func waitFor(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("condition was not met before timeout")
}
