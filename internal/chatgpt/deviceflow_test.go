package chatgpt

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestDevicePollClassifiesPendingAndSlowDown(t *testing.T) {
	responses := []struct {
		status int
		body   any
	}{
		{status: http.StatusBadRequest, body: map[string]string{"error": "authorization_pending"}},
		{status: http.StatusForbidden, body: map[string]any{"error": map[string]any{
			"message": "Device authorization is pending. Please try again.",
			"type":    "invalid_request_error",
			"code":    "deviceauth_authorization_pending",
		}}},
		{status: http.StatusForbidden, body: map[string]string{"error": "slow_down"}},
	}
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		item := responses[calls]
		calls++
		w.WriteHeader(item.status)
		_ = json.NewEncoder(w).Encode(item.body)
	}))
	defer server.Close()
	now := time.Date(2026, 7, 22, 0, 0, 0, 0, time.UTC)
	client := NewClient(Config{DevicePollURL: server.URL, Now: func() time.Time { return now }})
	authorization := DeviceAuthorization{DeviceAuthID: "device-placeholder", UserCode: "ABCD-EFGH", Interval: 5 * time.Second, ExpiresAt: now.Add(time.Minute)}
	result, err := client.PollDeviceAuthorizationResult(context.Background(), authorization)
	if err != nil || result.State != DevicePollPending || result.RetryAfter != 5*time.Second {
		t.Fatalf("legacy pending=%+v err=%v", result, err)
	}
	result, err = client.PollDeviceAuthorizationResult(context.Background(), authorization)
	if err != nil || result.State != DevicePollPending || result.RetryAfter != 5*time.Second {
		t.Fatalf("current pending=%+v err=%v", result, err)
	}
	result, err = client.PollDeviceAuthorizationResult(context.Background(), authorization)
	if err != nil || result.State != DevicePollSlowDown || result.RetryAfter != 10*time.Second {
		t.Fatalf("slow_down=%+v err=%v", result, err)
	}
}

func TestDevicePollExpiredDoesNotCallUpstream(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls++ }))
	defer server.Close()
	now := time.Date(2026, 7, 22, 0, 0, 0, 0, time.UTC)
	client := NewClient(Config{DevicePollURL: server.URL, Now: func() time.Time { return now }})
	_, err := client.PollDeviceAuthorizationResult(context.Background(), DeviceAuthorization{DeviceAuthID: "device-placeholder", UserCode: "ABCD-EFGH", ExpiresAt: now})
	var typed *TypedError
	if !errors.As(err, &typed) || typed.EvidenceCode != "device_authorization_expired" || calls != 0 {
		t.Fatalf("err=%v calls=%d", err, calls)
	}
}

func TestDeviceStartHonorsBoundedExpiresIn(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"device_auth_id": "device-placeholder", "user_code": "ABCD-EFGH", "interval": 3, "expires_in": 120})
	}))
	defer server.Close()
	now := time.Date(2026, 7, 22, 0, 0, 0, 0, time.UTC)
	client := NewClient(Config{DeviceStartURL: server.URL, Now: func() time.Time { return now }})
	authorization, err := client.StartDeviceAuthorization(context.Background())
	if err != nil || authorization.Interval != 3*time.Second || !authorization.ExpiresAt.Equal(now.Add(2*time.Minute)) {
		t.Fatalf("authorization=%+v err=%v", authorization, err)
	}
}
