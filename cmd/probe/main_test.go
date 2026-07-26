package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"chatgpt-monitor/internal/chatgpt"
)

func TestCredentialFilePermissionGate(t *testing.T) {
	dir := t.TempDir()
	secure := filepath.Join(dir, "secure")
	if err := os.WriteFile(secure, []byte("placeholder-secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(secure, 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := readCredential(secure); err != nil || got != "placeholder-secret" {
		t.Fatalf("secure read: got=%q err=%v", got, err)
	}

	insecure := filepath.Join(dir, "insecure")
	if err := os.WriteFile(insecure, []byte("placeholder-secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(insecure, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := readCredential(insecure); err == nil {
		t.Fatal("0644 credential file was accepted")
	}
}

func TestSecretJSONIsCreated0600AndExclusive(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tokens.json")
	value := map[string]string{"access_token": "placeholder-token"}
	if err := writeSecretJSON(path, value); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode=%o", info.Mode().Perm())
	}
	if err := writeSecretJSON(path, value); err == nil {
		t.Fatal("existing secret file was overwritten")
	}
}

func TestSafeStatusIncludesEvidenceLevel(t *testing.T) {
	payload, err := json.Marshal(safeStatus{
		SchemaVersion:  1,
		CredentialPath: "access",
		EvidenceCode:   "access_claim+accounts_check_2xx",
		EvidenceLevel:  chatgpt.EvidenceLiveVerified,
	})
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(payload, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded["evidence_level"] != string(chatgpt.EvidenceLiveVerified) {
		t.Fatalf("payload=%s", payload)
	}
}
