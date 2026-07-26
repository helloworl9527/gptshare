package crypto

import (
	"bytes"
	"testing"
)

func TestSealOpenRandomnessAndMetadata(t *testing.T) {
	keyring := mustKeyring(t, map[string][]byte{"active": bytes.Repeat([]byte{1}, 32)}, "active")
	plaintext := []byte("synthetic-sensitive-credential")
	aad := CredentialAAD(42, "access")
	first, err := keyring.Seal(plaintext, aad)
	if err != nil {
		t.Fatal(err)
	}
	second, err := keyring.Seal(plaintext, aad)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(first, second) || bytes.Contains(first, plaintext) {
		t.Fatal("ciphertext is deterministic or contains plaintext")
	}
	opened, err := keyring.Open(first, aad)
	if err != nil || !bytes.Equal(opened, plaintext) {
		t.Fatalf("open=%q err=%v", opened, err)
	}
	keyID, err := keyring.EnvelopeKeyID(first)
	if err != nil || keyID != "active" {
		t.Fatalf("key id=%q err=%v", keyID, err)
	}
}

func TestTamperWrongKeyAndAADFailClosed(t *testing.T) {
	keyring := mustKeyring(t, map[string][]byte{"key-a": bytes.Repeat([]byte{1}, 32)}, "key-a")
	envelope, err := keyring.Seal([]byte("secret"), CredentialAAD(1, "access"))
	if err != nil {
		t.Fatal(err)
	}
	tampered := append([]byte(nil), envelope...)
	tampered[len(tampered)-1] ^= 1
	for name, candidate := range map[string]struct {
		candidate []byte
		aad       []byte
		ring      *Keyring
	}{
		"tamper": {tampered, CredentialAAD(1, "access"), keyring},
		"AAD":    {envelope, CredentialAAD(2, "access"), keyring},
		"key":    {envelope, CredentialAAD(1, "access"), mustKeyring(t, map[string][]byte{"other": bytes.Repeat([]byte{2}, 32)}, "other")},
	} {
		t.Run(name, func(t *testing.T) {
			if plaintext, err := candidate.ring.Open(candidate.candidate, candidate.aad); err == nil || plaintext != nil {
				t.Fatalf("plaintext=%q err=%v", plaintext, err)
			}
		})
	}
}

func TestOldKeyReadAndRotation(t *testing.T) {
	oldRing := mustKeyring(t, map[string][]byte{"old": bytes.Repeat([]byte{1}, 32)}, "old")
	aad := CredentialAAD(9, "refresh")
	oldEnvelope, err := oldRing.Seal([]byte("refresh-secret"), aad)
	if err != nil {
		t.Fatal(err)
	}
	rotating := mustKeyring(t, map[string][]byte{"old": bytes.Repeat([]byte{1}, 32), "new": bytes.Repeat([]byte{2}, 32)}, "new")
	rotated, err := rotating.Reencrypt(oldEnvelope, aad)
	if err != nil {
		t.Fatal(err)
	}
	keyID, _ := rotating.EnvelopeKeyID(rotated)
	plaintext, err := rotating.Open(rotated, aad)
	if err != nil || keyID != "new" || string(plaintext) != "refresh-secret" {
		t.Fatalf("key=%s plaintext=%q err=%v", keyID, plaintext, err)
	}
	withoutOld := mustKeyring(t, map[string][]byte{"new": bytes.Repeat([]byte{2}, 32)}, "new")
	if _, err := withoutOld.Open(oldEnvelope, aad); err == nil {
		t.Fatal("old envelope decrypted after old key removal")
	}
}

func mustKeyring(t *testing.T, keys map[string][]byte, active string) *Keyring {
	t.Helper()
	keyring, err := NewKeyring(keys, active)
	if err != nil {
		t.Fatal(err)
	}
	return keyring
}
