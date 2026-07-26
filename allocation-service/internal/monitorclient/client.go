package monitorclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"allocation-service/monitorfacade"
)

var ErrUnavailable = monitorfacade.ErrUnavailable

type Client struct {
	baseURL         string
	apiKey          string
	httpClient      *http.Client
	maxRetries      int
	circuitFailures int
	circuitCooldown time.Duration
	mu              sync.Mutex
	failures        int
	circuitUntil    time.Time
}

type ImportRequest = monitorfacade.ImportRequest
type ImportResult = monitorfacade.ImportResult
type StatusResult = monitorfacade.StatusResult

type Option func(*Client)

func WithRetries(retries int) Option {
	return func(c *Client) {
		if retries >= 0 {
			c.maxRetries = retries
		}
	}
}

func WithCircuitBreaker(failures int, cooldown time.Duration) Option {
	return func(c *Client) {
		if failures > 0 {
			c.circuitFailures = failures
		}
		if cooldown > 0 {
			c.circuitCooldown = cooldown
		}
	}
}

func New(baseURL, apiKey string, httpClient *http.Client) (*Client, error) {
	return NewWithOptions(baseURL, apiKey, httpClient)
}

func NewWithOptions(baseURL, apiKey string, httpClient *http.Client, opts ...Option) (*Client, error) {
	parsed, err := url.Parse(strings.TrimRight(baseURL, "/"))
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return nil, errors.New("invalid monitor base url")
	}
	if strings.TrimSpace(apiKey) == "" {
		return nil, errors.New("monitor api key is required")
	}
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 5 * time.Second}
	}
	client := &Client{
		baseURL:         strings.TrimRight(baseURL, "/"),
		apiKey:          apiKey,
		httpClient:      httpClient,
		maxRetries:      1,
		circuitFailures: 3,
		circuitCooldown: 30 * time.Second,
	}
	for _, opt := range opts {
		opt(client)
	}
	return client, nil
}

func (c *Client) ImportForAllocation(ctx context.Context, request ImportRequest) (ImportResult, error) {
	if c == nil {
		return ImportResult{}, monitorfacade.NewFault(monitorfacade.FaultUnavailable)
	}
	payload := map[string]string{
		"token":      request.Token,
		"token_type": request.TokenType,
		"label":      request.Label,
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return ImportResult{}, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/api/v1/monitor/accounts/import-for-allocation", bytes.NewReader(encoded))
	if err != nil {
		return ImportResult{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+c.apiKey)
	resp, err := c.do(httpReq)
	if err != nil {
		return ImportResult{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return ImportResult{}, c.unavailableStatus(resp.StatusCode)
	}
	result, err := decodeStatus(resp, "")
	if err != nil {
		return ImportResult{}, monitorfacade.NewFault(monitorfacade.FaultContractChanged)
	}
	if result.MonitorAccountID == "" {
		return ImportResult{}, monitorfacade.NewFault(monitorfacade.FaultContractChanged)
	}
	return ImportResult{
		MonitorAccountID: result.MonitorAccountID,
		MonitorStatus:    result.MonitorStatus,
		Email:            result.Email,
		AccountExpiry:    result.AccountExpiry,
		Plan:             result.Plan,
	}, nil
}

func (c *Client) Status(ctx context.Context, monitorAccountID string) (ImportResult, error) {
	if c == nil || strings.TrimSpace(monitorAccountID) == "" {
		return ImportResult{}, monitorfacade.NewFault(monitorfacade.FaultUnavailable)
	}
	endpoint := c.baseURL + "/api/v1/monitor/accounts/" + url.PathEscape(monitorAccountID) + "/status"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return ImportResult{}, err
	}
	httpReq.Header.Set("Authorization", "Bearer "+c.apiKey)
	resp, err := c.do(httpReq)
	if err != nil {
		return ImportResult{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return ImportResult{MonitorAccountID: monitorAccountID, MonitorStatus: "not_found"}, nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return ImportResult{}, c.unavailableStatus(resp.StatusCode)
	}
	result, err := decodeStatus(resp, monitorAccountID)
	if err != nil {
		return ImportResult{}, monitorfacade.NewFault(monitorfacade.FaultContractChanged)
	}
	return ImportResult{
		MonitorAccountID: result.MonitorAccountID,
		MonitorStatus:    result.MonitorStatus,
		Email:            result.Email,
		AccountExpiry:    result.AccountExpiry,
		Plan:             result.Plan,
	}, nil
}

func (c *Client) ListAccounts(ctx context.Context) ([]StatusResult, error) {
	if c == nil {
		return nil, monitorfacade.NewFault(monitorfacade.FaultUnavailable)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/api/v1/monitor/accounts", nil)
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Authorization", "Bearer "+c.apiKey)
	resp, err := c.do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, c.unavailableStatus(resp.StatusCode)
	}
	var body struct {
		Accounts []struct {
			ProviderAccountID  string `json:"provider_account_id"`
			AccountID          string `json:"account_id"`
			Email              string `json:"email"`
			Status             string `json:"status"`
			Plan               string `json:"plan"`
			AuthExpiry         string `json:"auth_expiry"`
			SubscriptionExpiry string `json:"subscription_expiry"`
			MonitorStatus      string `json:"monitor_status"`
		} `json:"accounts"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, monitorfacade.NewFault(monitorfacade.FaultContractChanged)
	}
	results := make([]StatusResult, 0, len(body.Accounts))
	for _, item := range body.Accounts {
		id := defaultString(item.ProviderAccountID, item.AccountID)
		expiry := parseTimeOrZero(defaultString(item.AuthExpiry, item.SubscriptionExpiry))
		if id == "" || expiry.IsZero() {
			return nil, monitorfacade.NewFault(monitorfacade.FaultContractChanged)
		}
		status := defaultString(item.MonitorStatus, mapPhaseOneStatus(item.Status))
		results = append(results, StatusResult{
			MonitorAccountID: id,
			MonitorStatus:    normalizeMonitorStatus(status),
			Email:            item.Email,
			AccountExpiry:    expiry,
			Plan:             item.Plan,
		})
	}
	return results, nil
}

func (c *Client) BatchStatus(ctx context.Context, monitorAccountIDs []string) (map[string]StatusResult, error) {
	if c == nil || len(monitorAccountIDs) == 0 {
		return nil, monitorfacade.NewFault(monitorfacade.FaultUnavailable)
	}
	payload := map[string][]string{"provider_account_ids": monitorAccountIDs}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/api/v1/monitor/accounts/batch-status", bytes.NewReader(encoded))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+c.apiKey)
	resp, err := c.do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, c.unavailableStatus(resp.StatusCode)
	}
	var body struct {
		Items []struct {
			ProviderAccountID  string `json:"provider_account_id"`
			AccountID          string `json:"account_id"`
			Email              string `json:"email"`
			Status             string `json:"status"`
			Plan               string `json:"plan"`
			AuthExpiry         string `json:"auth_expiry"`
			SubscriptionExpiry string `json:"subscription_expiry"`
			MonitorStatus      string `json:"monitor_status"`
			Error              *struct {
				Code string `json:"code"`
			} `json:"error"`
		} `json:"items"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, monitorfacade.NewFault(monitorfacade.FaultContractChanged)
	}
	results := make(map[string]StatusResult, len(body.Items))
	for _, item := range body.Items {
		id := defaultString(item.ProviderAccountID, item.AccountID)
		if id == "" {
			continue
		}
		status := defaultString(item.MonitorStatus, mapPhaseOneStatus(item.Status))
		if item.Error != nil && item.Error.Code == "not_found" {
			status = "not_found"
		}
		results[id] = StatusResult{
			MonitorAccountID: id,
			MonitorStatus:    normalizeMonitorStatus(status),
			Email:            item.Email,
			AccountExpiry:    parseTimeOrZero(defaultString(item.AuthExpiry, item.SubscriptionExpiry)),
			Plan:             item.Plan,
		}
	}
	return results, nil
}

func (c *Client) Available(ctx context.Context) bool {
	if c == nil {
		return false
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/health", nil)
	if err != nil {
		return false
	}
	resp, err := c.do(httpReq)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode >= 200 && resp.StatusCode < 300
}

func (c *Client) do(req *http.Request) (*http.Response, error) {
	if c.circuitOpen(time.Now()) {
		return nil, monitorfacade.NewFault(monitorfacade.FaultUnavailable)
	}
	var lastErr error
	attempts := c.maxRetries + 1
	for attempt := 0; attempt < attempts; attempt++ {
		cloned := req.Clone(req.Context())
		if req.Body != nil && req.GetBody != nil {
			body, err := req.GetBody()
			if err != nil {
				return nil, err
			}
			cloned.Body = body
		}
		resp, err := c.httpClient.Do(cloned)
		if err == nil && resp.StatusCode < 500 {
			c.recordSuccess()
			return resp, nil
		}
		if resp != nil {
			resp.Body.Close()
			lastErr = c.unavailableStatus(resp.StatusCode)
		} else {
			lastErr = err
		}
	}
	c.recordFailure()
	if lastErr == nil {
		lastErr = monitorfacade.NewFault(monitorfacade.FaultUnavailable)
	}
	if errors.Is(lastErr, context.DeadlineExceeded) {
		return nil, monitorfacade.NewFault(monitorfacade.FaultTimeout)
	}
	type timeout interface{ Timeout() bool }
	var timeoutErr timeout
	if errors.As(lastErr, &timeoutErr) && timeoutErr.Timeout() {
		return nil, monitorfacade.NewFault(monitorfacade.FaultTimeout)
	}
	if errors.Is(lastErr, ErrUnavailable) {
		return nil, lastErr
	}
	return nil, monitorfacade.NewFault(monitorfacade.FaultUnavailable)
}

func (c *Client) circuitOpen(now time.Time) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return !c.circuitUntil.IsZero() && now.Before(c.circuitUntil)
}

func (c *Client) recordSuccess() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.failures = 0
	c.circuitUntil = time.Time{}
}

func (c *Client) recordFailure() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.failures++
	if c.failures >= c.circuitFailures {
		c.circuitUntil = time.Now().Add(c.circuitCooldown)
	}
}

func (c *Client) unavailableStatus(status int) error {
	return fmt.Errorf("%w: status %d", monitorfacade.NewFault(monitorfacade.FaultUnavailable), status)
}

func decodeStatus(resp *http.Response, fallbackID string) (StatusResult, error) {
	var body struct {
		ProviderAccountID  string `json:"provider_account_id"`
		AccountID          string `json:"account_id"`
		Email              string `json:"email"`
		Status             string `json:"status"`
		Plan               string `json:"plan"`
		AuthExpiry         string `json:"auth_expiry"`
		SubscriptionExpiry string `json:"subscription_expiry"`
		MonitorStatus      string `json:"monitor_status"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return StatusResult{}, err
	}
	id := defaultString(body.ProviderAccountID, body.AccountID)
	if id == "" {
		id = fallbackID
	}
	status := defaultString(body.MonitorStatus, mapPhaseOneStatus(body.Status))
	return StatusResult{
		MonitorAccountID: id,
		MonitorStatus:    normalizeMonitorStatus(status),
		Email:            body.Email,
		AccountExpiry:    parseTimeOrZero(defaultString(body.AuthExpiry, body.SubscriptionExpiry)),
		Plan:             body.Plan,
	}, nil
}

func parseTimeOrZero(value string) time.Time {
	if strings.TrimSpace(value) == "" {
		return time.Time{}
	}
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Time{}
	}
	return parsed.UTC()
}

func mapPhaseOneStatus(status string) string {
	switch status {
	case "alive":
		return "alive"
	case "dead_normal", "dead_banned", "not_found":
		return status
	case "unknown", "":
		return "unknown"
	default:
		return "unknown"
	}
}

func normalizeMonitorStatus(status string) string {
	switch status {
	case "alive", "unknown", "dead_normal", "dead_banned", "not_found", "unknown_monitor":
		return status
	default:
		return "unknown"
	}
}

func defaultString(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}
