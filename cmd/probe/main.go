package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"chatgpt-monitor/internal/chatgpt"
)

type deviceState struct {
	DeviceAuthID    string    `json:"device_auth_id"`
	UserCode        string    `json:"user_code"`
	VerifyURL       string    `json:"verify_url"`
	IntervalSeconds int64     `json:"interval_seconds"`
	ExpiresAt       time.Time `json:"expires_at"`
}

type safeStatus struct {
	SchemaVersion      int                   `json:"schema_version"`
	CredentialPath     string                `json:"credential_path"`
	ProviderAccountID  string                `json:"provider_account_id"`
	RawPlan            string                `json:"raw_plan"`
	Plan               chatgpt.Plan          `json:"plan"`
	SubscriptionExpiry *time.Time            `json:"subscription_expiry"`
	AccountState       chatgpt.AccountState  `json:"account_state"`
	EvidenceCode       string                `json:"evidence_code"`
	EvidenceLevel      chatgpt.EvidenceLevel `json:"evidence_level"`
	ResponseHash       string                `json:"response_hash"`
}

func main() {
	os.Exit(run())
}

func run() int {
	mode := flag.String("mode", "status", "status, device-start, or device-poll")
	kind := flag.String("kind", "", "access, refresh, or session")
	credentialFile := flag.String("credential-file", "", "0600 credential file; stdin when omitted")
	stateFile := flag.String("state-file", "", "0600 device state file")
	tokenOutput := flag.String("token-output", "", "0600 device token output file")
	flag.Parse()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	client := chatgpt.NewClient(chatgpt.Config{EvidenceLevel: chatgpt.EvidenceLiveVerified})

	switch *mode {
	case "device-start":
		if *stateFile == "" {
			return fail("state_file_required", 2)
		}
		auth, err := client.StartDeviceAuthorization(ctx)
		if err != nil {
			return failTyped(err)
		}
		state := deviceState{DeviceAuthID: auth.DeviceAuthID, UserCode: auth.UserCode, VerifyURL: auth.VerifyURL, IntervalSeconds: int64(auth.Interval / time.Second), ExpiresAt: auth.ExpiresAt}
		if err := writeSecretJSON(*stateFile, state); err != nil {
			return fail("device_state_write_failed", 2)
		}
		_ = json.NewEncoder(os.Stdout).Encode(map[string]any{"verify_url": auth.VerifyURL, "user_code": auth.UserCode, "expires_at": auth.ExpiresAt})
		return 0
	case "device-poll":
		if *stateFile == "" || *tokenOutput == "" {
			return fail("state_and_token_output_required", 2)
		}
		var state deviceState
		if err := readSecretJSON(*stateFile, &state); err != nil {
			return fail("device_state_read_failed", 2)
		}
		auth := chatgpt.DeviceAuthorization{DeviceAuthID: state.DeviceAuthID, UserCode: state.UserCode, VerifyURL: state.VerifyURL, Interval: time.Duration(state.IntervalSeconds) * time.Second, ExpiresAt: state.ExpiresAt}
		tokens, pending, err := client.PollDeviceAuthorization(ctx, auth)
		if err != nil {
			return failTyped(err)
		}
		if pending {
			_ = json.NewEncoder(os.Stdout).Encode(map[string]any{"status": "pending"})
			return 10
		}
		if err := writeSecretJSON(*tokenOutput, map[string]string{"access_token": tokens.AccessToken, "refresh_token": tokens.RefreshToken, "id_token": tokens.IDToken}); err != nil {
			return fail("token_output_write_failed", 2)
		}
		return emitStatus(ctx, client, "device", tokens.AccessToken)
	case "status":
		var tokens chatgpt.TokenSet
		var err error
		if *kind == "device" {
			tokens, err = readTokenBundle(*credentialFile)
		} else {
			var secret string
			secret, err = readCredential(*credentialFile)
			if err == nil {
				tokens, err = client.ExchangeCredential(ctx, chatgpt.CredentialKind(*kind), secret)
			}
		}
		if err != nil {
			var typed *chatgpt.Error
			if errors.As(err, &typed) {
				return failTyped(err)
			}
			return fail("credential_read_failed", 2)
		}
		if *tokenOutput != "" {
			if err := writeSecretJSON(*tokenOutput, map[string]string{"access_token": tokens.AccessToken, "refresh_token": tokens.RefreshToken, "id_token": tokens.IDToken}); err != nil {
				return fail("token_output_write_failed", 2)
			}
		}
		return emitStatus(ctx, client, *kind, tokens.AccessToken)
	default:
		return fail("unsupported_mode", 2)
	}
}

func readTokenBundle(path string) (chatgpt.TokenSet, error) {
	var payload struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		IDToken      string `json:"id_token"`
	}
	if err := readSecretJSON(path, &payload); err != nil {
		return chatgpt.TokenSet{}, err
	}
	if strings.TrimSpace(payload.AccessToken) == "" {
		return chatgpt.TokenSet{}, fmt.Errorf("token bundle missing access token")
	}
	return chatgpt.TokenSet{AccessToken: payload.AccessToken, RefreshToken: payload.RefreshToken, IDToken: payload.IDToken}, nil
}

func emitStatus(ctx context.Context, client *chatgpt.Client, path, access string) int {
	status, err := client.FetchStatus(ctx, access)
	if err != nil {
		var typed *chatgpt.Error
		if errors.As(err, &typed) {
			_ = json.NewEncoder(os.Stdout).Encode(map[string]any{
				"schema_version": 1, "credential_path": path, "account_state": status.AccountState,
				"evidence_code": typed.EvidenceCode, "evidence_level": typed.EvidenceLevel,
				"banned_candidate": typed.BannedCandidate, "preserve_business_state": typed.PreserveBusinessState,
				"response_hash": status.ResponseHash,
			})
			if typed.Retryable {
				return 10
			}
		}
		return 20
	}
	sum := sha256.Sum256([]byte(status.ProviderAccountID))
	safe := safeStatus{SchemaVersion: 1, CredentialPath: path, ProviderAccountID: "sha256:" + hex.EncodeToString(sum[:8]), RawPlan: status.RawPlan, Plan: status.Plan, SubscriptionExpiry: status.SubscriptionExpiry, AccountState: status.AccountState, EvidenceCode: status.EvidenceCode, EvidenceLevel: status.EvidenceLevel, ResponseHash: status.ResponseHash}
	_ = json.NewEncoder(os.Stdout).Encode(safe)
	return 0
}

func readCredential(path string) (string, error) {
	var data []byte
	var err error
	if path == "" {
		data, err = io.ReadAll(io.LimitReader(os.Stdin, 16<<10))
	} else {
		if err = require0600(path); err != nil {
			return "", err
		}
		data, err = os.ReadFile(path)
	}
	if err != nil {
		return "", err
	}
	secret := strings.TrimSpace(string(data))
	if secret == "" {
		return "", fmt.Errorf("empty credential")
	}
	return secret, nil
}

func require0600(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		return fmt.Errorf("credential file must be regular and 0600")
	}
	return nil
}

func readSecretJSON(path string, dst any) error {
	if err := require0600(path); err != nil {
		return err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, dst)
}

func writeSecretJSON(path string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	defer file.Close()
	if _, err := file.Write(data); err != nil {
		return err
	}
	return file.Chmod(0o600)
}

func failTyped(err error) int {
	var typed *chatgpt.Error
	if errors.As(err, &typed) {
		_ = json.NewEncoder(os.Stdout).Encode(map[string]any{
			"schema_version": 1, "account_state": stateForKind(typed.Kind),
			"evidence_code": typed.EvidenceCode, "evidence_level": typed.EvidenceLevel,
			"banned_candidate": typed.BannedCandidate, "preserve_business_state": typed.PreserveBusinessState,
		})
		if typed.Retryable {
			return 10
		}
		return 20
	}
	return fail("internal_error", 20)
}

func stateForKind(kind chatgpt.ErrorKind) chatgpt.AccountState {
	switch kind {
	case chatgpt.ErrorCredentialRevoked:
		return chatgpt.StateCredentialRevoked
	case chatgpt.ErrorAccountDisabled:
		return chatgpt.StateAccountDisabled
	case chatgpt.ErrorPermissionDenied:
		return chatgpt.StatePermissionDenied
	case chatgpt.ErrorRateLimited:
		return chatgpt.StateRateLimited
	case chatgpt.ErrorUpstreamTransient:
		return chatgpt.StateUpstreamTransient
	default:
		return chatgpt.StateContractChanged
	}
}

func fail(code string, exit int) int {
	_ = json.NewEncoder(os.Stdout).Encode(map[string]any{"schema_version": 1, "evidence_code": code})
	return exit
}
