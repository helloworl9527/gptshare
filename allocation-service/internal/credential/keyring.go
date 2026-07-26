package credential

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
)

const (
	AccountTable       = "chatgpt_accounts"
	CardsTable         = "cards"
	CredentialPassword = "display_password"
	CredentialTOTP     = "display_2fa"
	CredentialCardCode = "code"
)

var (
	ErrInvalidKeyring    = errors.New("invalid credential keyring")
	ErrUnknownKey        = errors.New("credential key id is not available")
	ErrDecryptCredential = errors.New("credential decrypt failed")
)

type Keyring struct {
	keys     map[string][]byte
	activeID string
}

type Sealed struct {
	KeyID      string
	Ciphertext []byte
}

func NewKeyring(keys map[string][]byte, activeID string) (*Keyring, error) {
	if len(keys) == 0 || activeID == "" {
		return nil, ErrInvalidKeyring
	}
	copied := make(map[string][]byte, len(keys))
	for id, key := range keys {
		if id == "" || len(key) != 32 {
			return nil, ErrInvalidKeyring
		}
		copied[id] = append([]byte(nil), key...)
	}
	if _, ok := copied[activeID]; !ok {
		return nil, ErrUnknownKey
	}
	return &Keyring{keys: copied, activeID: activeID}, nil
}

func (k *Keyring) ActiveKeyID() string {
	if k == nil {
		return ""
	}
	return k.activeID
}

func (k *Keyring) Seal(accountID int64, credentialType string, plaintext []byte) (Sealed, error) {
	return k.SealWithAAD(AAD(accountID, credentialType), plaintext)
}

func (k *Keyring) SealWithAAD(aad []byte, plaintext []byte) (Sealed, error) {
	if k == nil {
		return Sealed{}, ErrInvalidKeyring
	}
	key, ok := k.keys[k.activeID]
	if !ok {
		return Sealed{}, ErrUnknownKey
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return Sealed{}, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return Sealed{}, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return Sealed{}, err
	}
	ciphertext := gcm.Seal(nil, nonce, plaintext, aad)
	payload := append(nonce, ciphertext...)
	return Sealed{KeyID: k.activeID, Ciphertext: payload}, nil
}

func (k *Keyring) Open(accountID int64, credentialType, keyID string, payload []byte) ([]byte, error) {
	return k.OpenWithAAD(AAD(accountID, credentialType), keyID, payload)
}

func (k *Keyring) OpenWithAAD(aad []byte, keyID string, payload []byte) ([]byte, error) {
	if k == nil {
		return nil, ErrInvalidKeyring
	}
	key, ok := k.keys[keyID]
	if !ok {
		return nil, ErrUnknownKey
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	if len(payload) <= gcm.NonceSize() {
		return nil, ErrDecryptCredential
	}
	nonce := payload[:gcm.NonceSize()]
	ciphertext := payload[gcm.NonceSize():]
	plaintext, err := gcm.Open(nil, nonce, ciphertext, aad)
	if err != nil {
		return nil, ErrDecryptCredential
	}
	return plaintext, nil
}

func AAD(accountID int64, credentialType string) []byte {
	return []byte(fmt.Sprintf("table=%s;account_id=%d;credential_type=%s", AccountTable, accountID, credentialType))
}

func CardAAD(cardID int64) []byte {
	return []byte(fmt.Sprintf("table=%s;card_id=%d;credential_type=%s", CardsTable, cardID, CredentialCardCode))
}
