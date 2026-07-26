package credential

import (
	"bytes"
	"testing"
)

func TestKeyringSealOpenAndRejectsTamperWrongKeyWrongAAD(t *testing.T) {
	keyring := testKeyring(t, bytes.Repeat([]byte{1}, 32), "alloc-k1")
	sealed, err := keyring.Seal(42, CredentialPassword, []byte("secret-password"))
	if err != nil {
		t.Fatal(err)
	}
	opened, err := keyring.Open(42, CredentialPassword, sealed.KeyID, sealed.Ciphertext)
	if err != nil {
		t.Fatal(err)
	}
	if string(opened) != "secret-password" {
		t.Fatalf("opened plaintext=%q", opened)
	}
	tampered := append([]byte(nil), sealed.Ciphertext...)
	tampered[len(tampered)-1] ^= 0x01
	if _, err := keyring.Open(42, CredentialPassword, sealed.KeyID, tampered); err == nil {
		t.Fatal("tampered ciphertext decrypted")
	}
	wrongKey := testKeyring(t, bytes.Repeat([]byte{2}, 32), "alloc-k1")
	if _, err := wrongKey.Open(42, CredentialPassword, sealed.KeyID, sealed.Ciphertext); err == nil {
		t.Fatal("wrong key decrypted")
	}
	if _, err := keyring.Open(42, CredentialTOTP, sealed.KeyID, sealed.Ciphertext); err == nil {
		t.Fatal("wrong AAD decrypted")
	}
}

func testKeyring(t *testing.T, key []byte, id string) *Keyring {
	t.Helper()
	keyring, err := NewKeyring(map[string][]byte{id: key}, id)
	if err != nil {
		t.Fatal(err)
	}
	return keyring
}
