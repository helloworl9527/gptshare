package account

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"chatgpt-monitor/internal/chatgpt"
	credentialcrypto "chatgpt-monitor/internal/crypto"
)

func TestOAuthStartCompleteAndReplayProtection(t *testing.T) {
	service, database, keyring, closeDB := testService(t, "acct-oauth")
	defer closeDB()
	client := service.client.(*fakeClient)
	accessExpiry := service.now().UTC().Add(time.Hour)
	client.tokens = chatgpt.TokenSet{
		AccessToken:     "oauth-access-plaintext",
		RefreshToken:    "oauth-refresh-plaintext",
		IDToken:         "oauth-id-placeholder",
		AccessExpiresAt: &accessExpiry,
	}
	start, err := service.StartOAuthImport(context.Background(), "OAuth account")
	if err != nil {
		t.Fatal(err)
	}
	authorizationURL, err := url.Parse(start.AuthorizationURL)
	if err != nil {
		t.Fatal(err)
	}
	state := authorizationURL.Query().Get("state")
	challenge := authorizationURL.Query().Get("code_challenge")
	if state == "" || challenge == "" || strings.Contains(start.AuthorizationURL, "code_verifier") {
		t.Fatalf("authorization URL=%q", start.AuthorizationURL)
	}
	var envelope []byte
	if err := database.DB().QueryRow("SELECT enc_session FROM oauth_auth_sessions WHERE id=?", start.SessionID).Scan(&envelope); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(envelope, []byte(state)) || bytes.Contains(envelope, []byte("oauth-access-plaintext")) {
		t.Fatal("OAuth session stored plaintext")
	}
	plaintext, err := keyring.Open(envelope, credentialcrypto.OAuthSessionAAD(start.SessionID))
	if err != nil || !bytes.Contains(plaintext, []byte(state)) || !bytes.Contains(plaintext, []byte("code_verifier")) {
		t.Fatalf("encrypted OAuth payload invalid: %v", err)
	}
	zero(plaintext)

	_, err = service.CompleteOAuth(context.Background(), start.SessionID, "http://localhost:1455/auth/callback?code=code-one&state=wrong")
	var serviceErr *ServiceError
	if !errors.As(err, &serviceErr) || serviceErr.Code != "oauth_state_mismatch" {
		t.Fatalf("state mismatch error=%v", err)
	}
	accountResult, err := service.CompleteOAuth(context.Background(), start.SessionID,
		"http://localhost:1455/auth/callback?code=code-one&state="+url.QueryEscape(state))
	if err != nil {
		t.Fatal(err)
	}
	if accountResult.Credential.Type != "refresh" || client.oauthCode != "code-one" || client.oauthVerifier == "" {
		t.Fatalf("account=%+v code=%q verifier configured=%v", accountResult, client.oauthCode, client.oauthVerifier != "")
	}
	var credentialEnvelope []byte
	if err := database.DB().QueryRow("SELECT enc_credentials FROM accounts WHERE id=?", accountResult.ID).Scan(&credentialEnvelope); err != nil {
		t.Fatal(err)
	}
	credentialPlaintext, err := keyring.Open(credentialEnvelope, credentialcrypto.CredentialAAD(accountResult.ID, "refresh"))
	if err != nil {
		t.Fatal(err)
	}
	var credential credentialPayload
	if err := json.Unmarshal(credentialPlaintext, &credential); err != nil {
		t.Fatal(err)
	}
	zero(credentialPlaintext)
	if credential.IDToken != "oauth-id-placeholder" || credential.OAuthSource != "oauth" || credential.AccessExpiresAt == nil || !credential.AccessExpiresAt.Equal(accessExpiry) {
		t.Fatalf("OAuth credential metadata=%+v", credential)
	}
	var sessionState string
	var sessionLength int
	if err := database.DB().QueryRow("SELECT state,length(enc_session) FROM oauth_auth_sessions WHERE id=?", start.SessionID).Scan(&sessionState, &sessionLength); err != nil {
		t.Fatal(err)
	}
	if sessionState != "authorized" || sessionLength != 0 {
		t.Fatalf("session state=%q envelope length=%d", sessionState, sessionLength)
	}
	_, err = service.CompleteOAuth(context.Background(), start.SessionID,
		"http://localhost:1455/auth/callback?code=code-two&state="+url.QueryEscape(state))
	if !errors.As(err, &serviceErr) || serviceErr.Code != "oauth_session_used" {
		t.Fatalf("replay error=%v", err)
	}
	if _, err := database.DB().Exec("PRAGMA wal_checkpoint(FULL)"); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{databasePath(t, database), databasePath(t, database) + "-wal", databasePath(t, database) + "-shm"} {
		contents, readErr := os.ReadFile(path)
		if os.IsNotExist(readErr) {
			continue
		}
		if readErr != nil {
			t.Fatal(readErr)
		}
		for _, secret := range [][]byte{[]byte("oauth-access-plaintext"), []byte("oauth-refresh-plaintext"), []byte("code-one"), []byte(client.oauthVerifier)} {
			if len(secret) > 0 && bytes.Contains(contents, secret) {
				t.Fatalf("OAuth plaintext found in SQLite file %s", path)
			}
		}
	}
}

func TestOAuthGenericAccountCheckDenialIsNonBlocking(t *testing.T) {
	service, _, _, closeDB := testService(t, "acct-oauth-denied")
	defer closeDB()
	client := service.client.(*fakeClient)
	client.tokens = chatgpt.TokenSet{AccessToken: "oauth-access", RefreshToken: "oauth-refresh", IDToken: "oauth-id"}
	client.statusErr = &chatgpt.TypedError{Kind: chatgpt.ErrorPermissionDenied, StatusCode: 401, EvidenceCode: "http_401", EvidenceLevel: chatgpt.EvidenceContractVerifiedLivePending, PreserveBusinessState: true}
	start, err := service.StartOAuthImport(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	authorizationURL, _ := url.Parse(start.AuthorizationURL)
	callback := "http://localhost:1455/auth/callback?code=denied-status&state=" + url.QueryEscape(authorizationURL.Query().Get("state"))
	accountResult, err := service.CompleteOAuth(context.Background(), start.SessionID, callback)
	if err != nil {
		t.Fatal(err)
	}
	if accountResult.Status != "alive" || accountResult.LastCheckState != "ok" {
		t.Fatalf("account=%+v", accountResult)
	}
}

func TestOAuthExpiryAndCallbackValidation(t *testing.T) {
	service, database, _, closeDB := testService(t, "acct-oauth-expired")
	defer closeDB()
	now := service.now()
	start, err := service.StartOAuthImport(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	for _, callback := range []string{
		"https://localhost:1455/auth/callback?code=x&state=y",
		"http://127.0.0.1:1455/auth/callback?code=x&state=y",
		"http://localhost:1455/other?code=x&state=y",
	} {
		if _, _, err := parseOAuthCallback(callback); err == nil {
			t.Fatalf("accepted callback %q", callback)
		}
	}
	service.now = func() time.Time { return now.Add(oauthSessionTTL + time.Second) }
	_, err = service.CompleteOAuth(context.Background(), start.SessionID,
		"http://localhost:1455/auth/callback?code=x&state=y")
	var serviceErr *ServiceError
	if !errors.As(err, &serviceErr) || serviceErr.Code != "oauth_session_expired" {
		t.Fatalf("expired completion error=%v", err)
	}
	var state string
	var length int
	if err := database.DB().QueryRow("SELECT state,length(enc_session) FROM oauth_auth_sessions WHERE id=?", start.SessionID).Scan(&state, &length); err != nil {
		t.Fatal(err)
	}
	if state != "expired" || length != 0 {
		t.Fatalf("expired session state=%q length=%d", state, length)
	}
}

func TestOAuthReauthorizationOpensNewEpoch(t *testing.T) {
	service, database, _, closeDB := testService(t, "acct-oauth-reauthorize")
	defer closeDB()
	created, err := service.ImportByToken(context.Background(), &TokenInput{AccessToken: "original"})
	if err != nil {
		t.Fatal(err)
	}
	client := service.client.(*fakeClient)
	client.tokens = chatgpt.TokenSet{AccessToken: "oauth-new-access", RefreshToken: "oauth-new-refresh"}
	newExpiry := client.status.SubscriptionExpiry.Add(30 * 24 * time.Hour)
	client.status.SubscriptionExpiry = &newExpiry
	start, err := service.StartOAuthReauthorization(context.Background(), created.ID)
	if err != nil {
		t.Fatal(err)
	}
	authorizationURL, _ := url.Parse(start.AuthorizationURL)
	callback := "http://localhost:1455/auth/callback?code=reauthorize-code&state=" +
		url.QueryEscape(authorizationURL.Query().Get("state"))
	updated, err := service.CompleteOAuth(context.Background(), start.SessionID, callback)
	if err != nil {
		t.Fatal(err)
	}
	if updated.ID != created.ID || !updated.AuthExpiry.Equal(newExpiry) || updated.Credential.Type != "refresh" {
		t.Fatalf("updated account=%+v", updated)
	}
	var epochs, ended int
	if err := database.DB().QueryRow("SELECT count(*),sum(ended_at IS NOT NULL) FROM authorization_epochs WHERE account_id=?", created.ID).Scan(&epochs, &ended); err != nil {
		t.Fatal(err)
	}
	if epochs != 2 || ended != 1 {
		t.Fatalf("epochs=%d ended=%d", epochs, ended)
	}
}

type batchClient struct {
	expiry time.Time
}

func (c *batchClient) ExchangeCredential(_ context.Context, _ chatgpt.CredentialKind, secret string) (chatgpt.TokenSet, error) {
	return chatgpt.TokenSet{AccessToken: secret}, nil
}

func (c *batchClient) FetchStatus(_ context.Context, access string) (chatgpt.StatusResult, error) {
	if access == "bad" {
		return chatgpt.StatusResult{}, &chatgpt.TypedError{Kind: chatgpt.ErrorInput, EvidenceCode: "invalid_fixture"}
	}
	return chatgpt.StatusResult{
		ProviderAccountID:  "acct-" + access,
		RawPlan:            "chatgptplusplan",
		Plan:               chatgpt.PlanPlus,
		SubscriptionExpiry: &c.expiry,
		AccountState:       chatgpt.StateActive,
		EvidenceLevel:      chatgpt.EvidenceLiveVerified,
	}, nil
}

func TestBatchImportPartialSuccessAndExactCredentialRule(t *testing.T) {
	service, _, _, closeDB := testService(t, "unused")
	defer closeDB()
	service.client = &batchClient{expiry: time.Date(2026, 8, 19, 0, 0, 0, 0, time.UTC)}
	input := &BatchTokenInput{Items: []TokenInput{
		{Label: "A", AccessToken: "a"},
		{AccessToken: "bad"},
		{AccessToken: "conflict", RefreshToken: "also-conflict"},
		{RefreshToken: "b"},
		{AccessToken: "a"},
	}}
	result, err := service.ImportTokenBatch(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if result.Total != 5 || result.Succeeded != 2 || result.Failed != 3 || len(input.Items) != 0 {
		t.Fatalf("batch result=%+v input items=%d", result, len(input.Items))
	}
	if result.Results[1].Status != "invalid" || result.Results[2].Code != "credential_type_conflict" {
		t.Fatalf("item results=%+v", result.Results)
	}
	duplicates := 0
	for _, item := range result.Results {
		if item.Status == "duplicate" {
			duplicates++
		}
		if item.Code == "bad" || strings.Contains(item.Code, "conflict") && item.Code != "credential_type_conflict" {
			t.Fatalf("credential escaped in result: %+v", item)
		}
	}
	if duplicates != 1 {
		t.Fatalf("duplicate results=%d items=%+v", duplicates, result.Results)
	}
}
