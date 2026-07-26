package chatgpt

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const deviceRequestTimeout = 5 * time.Second

// PollDeviceAuthorizationResult performs at most one upstream poll. Scheduling,
// persistence, and client-frequency suppression belong to the account service.
func (c *Client) PollDeviceAuthorizationResult(ctx context.Context, auth DeviceAuthorization) (DevicePollResult, error) {
	if auth.DeviceAuthID == "" || auth.UserCode == "" {
		return DevicePollResult{}, newTypedError(ErrorInput, 0, "device_authorization_invalid", EvidenceUnverified, false, false, false, nil)
	}
	if !auth.ExpiresAt.IsZero() && !c.now().Before(auth.ExpiresAt) {
		return DevicePollResult{}, newTypedError(ErrorInput, 0, "device_authorization_expired", EvidenceUnverified, false, false, false, nil)
	}
	requestCtx, cancel := context.WithTimeout(ctx, deviceRequestTimeout)
	defer cancel()
	body, _ := json.Marshal(map[string]string{"device_auth_id": auth.DeviceAuthID, "user_code": auth.UserCode})
	req, err := http.NewRequestWithContext(requestCtx, http.MethodPost, c.devicePollURL, bytes.NewReader(body))
	if err != nil {
		return DevicePollResult{}, transient("device_poll_create", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	status, response, _, doErr := c.do(req)
	if doErr != nil {
		return DevicePollResult{}, doErr
	}
	code := deviceResponseCode(response)
	if code == "slow_down" {
		return DevicePollResult{State: DevicePollSlowDown, RetryAfter: auth.Interval + 5*time.Second}, nil
	}
	if code == "authorization_pending" || ((status == http.StatusForbidden || status == http.StatusNotFound) && code == "") {
		return DevicePollResult{State: DevicePollPending, RetryAfter: auth.Interval}, nil
	}
	if status < 200 || status >= 300 {
		return DevicePollResult{}, classifyHTTP(status, response)
	}
	var payload struct {
		AuthorizationCode string `json:"authorization_code"`
		CodeVerifier      string `json:"code_verifier"`
	}
	if json.Unmarshal(response, &payload) != nil || payload.AuthorizationCode == "" || payload.CodeVerifier == "" {
		return DevicePollResult{}, newTypedError(ErrorContractChanged, status, "device_poll_fields_missing", EvidenceUnverified, false, false, true, nil)
	}
	form := url.Values{
		"grant_type": {"authorization_code"}, "client_id": {c.clientID},
		"code": {payload.AuthorizationCode}, "redirect_uri": {deviceRedirectURI}, "code_verifier": {payload.CodeVerifier},
	}
	exchangeReq, err := http.NewRequestWithContext(requestCtx, http.MethodPost, c.tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return DevicePollResult{}, transient("device_exchange_create", err)
	}
	exchangeReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	exchangeReq.Header.Set("Accept", "application/json")
	exchangeStatus, exchangeBody, _, exchangeErr := c.do(exchangeReq)
	if exchangeErr != nil {
		return DevicePollResult{}, exchangeErr
	}
	if exchangeStatus < 200 || exchangeStatus >= 300 {
		return DevicePollResult{}, classifyHTTP(exchangeStatus, exchangeBody)
	}
	tokens, decodeErr := decodeTokenSet(exchangeBody, "device_exchange")
	if decodeErr != nil {
		return DevicePollResult{}, decodeErr
	}
	return DevicePollResult{State: DevicePollAuthorized, Tokens: tokens}, nil
}

func deviceResponseCode(body []byte) string {
	var payload struct {
		Code  string `json:"code"`
		Error any    `json:"error"`
	}
	if json.Unmarshal(body, &payload) != nil {
		return ""
	}
	if payload.Code != "" {
		return payload.Code
	}
	switch value := payload.Error.(type) {
	case string:
		return value
	case map[string]any:
		if code, ok := value["code"].(string); ok {
			return code
		}
	}
	return ""
}
