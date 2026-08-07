package crypto

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"strconv"
)

const (
	envelopeVersion byte = 1
	nonceSize            = 12
	maxKeyIDLength       = 255
)

var envelopeMagic = [4]byte{'C', 'G', 'C', envelopeVersion}

type Keyring struct {
	keys     map[string][]byte
	activeID string
	random   io.Reader
}

func NewKeyring(keys map[string][]byte, activeID string) (*Keyring, error) {
	if activeID == "" {
		return nil, errors.New("active credential key id is required")
	}
	copied := make(map[string][]byte, len(keys))
	for id, key := range keys {
		if id == "" || len(id) > maxKeyIDLength || len(key) != 32 {
			return nil, errors.New("credential keys must have a valid id and exactly 32 bytes")
		}
		copied[id] = append([]byte(nil), key...)
	}
	if _, ok := copied[activeID]; !ok {
		return nil, errors.New("active credential key is missing")
	}
	return &Keyring{keys: copied, activeID: activeID, random: rand.Reader}, nil
}

func (k *Keyring) ActiveKeyID() string { return k.activeID }

func (k *Keyring) Seal(plaintext, aad []byte) ([]byte, error) {
	if len(plaintext) == 0 || len(aad) == 0 {
		return nil, errors.New("plaintext and AAD are required")
	}
	aead, err := newGCM(k.keys[k.activeID])
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, nonceSize)
	if _, err := io.ReadFull(k.random, nonce); err != nil {
		return nil, errors.New("generate credential nonce")
	}
	ciphertext := aead.Seal(nil, nonce, plaintext, aad)
	result := make([]byte, 0, len(envelopeMagic)+1+len(k.activeID)+len(nonce)+len(ciphertext))
	result = append(result, envelopeMagic[:]...)
	result = append(result, byte(len(k.activeID)))
	result = append(result, k.activeID...)
	result = append(result, nonce...)
	result = append(result, ciphertext...)
	return result, nil
}

func (k *Keyring) Open(envelope, aad []byte) ([]byte, error) {
	keyID, nonce, ciphertext, err := parseEnvelope(envelope)
	if err != nil {
		return nil, err
	}
	key, ok := k.keys[keyID]
	if !ok {
		return nil, errors.New("credential envelope references an unavailable key")
	}
	aead, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	plaintext, err := aead.Open(nil, nonce, ciphertext, aad)
	if err != nil {
		return nil, errors.New("credential authentication failed")
	}
	return plaintext, nil
}

func (k *Keyring) EnvelopeKeyID(envelope []byte) (string, error) {
	keyID, _, _, err := parseEnvelope(envelope)
	return keyID, err
}

func (k *Keyring) Reencrypt(envelope, aad []byte) ([]byte, error) {
	plaintext, err := k.Open(envelope, aad)
	if err != nil {
		return nil, err
	}
	defer zero(plaintext)
	return k.Seal(plaintext, aad)
}

func (k *Keyring) ReencryptAccounts(ctx context.Context, db *sql.DB) (int, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT id,token_type,enc_credentials FROM accounts
		WHERE deleted_at IS NULL AND length(enc_credentials)>0 AND credential_key_id<>?`, k.activeID)
	if err != nil {
		return 0, err
	}
	type record struct {
		id       int64
		kind     string
		envelope []byte
	}
	var records []record
	for rows.Next() {
		var item record
		if err := rows.Scan(&item.id, &item.kind, &item.envelope); err != nil {
			rows.Close()
			return 0, err
		}
		records = append(records, item)
	}
	if err := rows.Close(); err != nil {
		return 0, err
	}
	for _, item := range records {
		aad := CredentialAAD(item.id, item.kind)
		reencrypted, err := k.Reencrypt(item.envelope, aad)
		if err != nil {
			return 0, fmt.Errorf("reencrypt account %d: %w", item.id, err)
		}
		if _, err := tx.ExecContext(ctx, "UPDATE accounts SET enc_credentials=?,credential_key_id=?,credential_generation=credential_generation+1 WHERE id=?", reencrypted, k.activeID, item.id); err != nil {
			return 0, err
		}
	}
	deviceRows, err := tx.QueryContext(ctx, `SELECT id,enc_device_code FROM device_auth_sessions
		WHERE state='pending' AND length(enc_device_code)>0 AND credential_key_id<>?`, k.activeID)
	if err != nil {
		return 0, err
	}
	type deviceRecord struct {
		id       string
		envelope []byte
	}
	var deviceRecords []deviceRecord
	for deviceRows.Next() {
		var item deviceRecord
		if err := deviceRows.Scan(&item.id, &item.envelope); err != nil {
			deviceRows.Close()
			return 0, err
		}
		deviceRecords = append(deviceRecords, item)
	}
	if err := deviceRows.Close(); err != nil {
		return 0, err
	}
	for _, item := range deviceRecords {
		reencrypted, err := k.Reencrypt(item.envelope, DeviceSessionAAD(item.id))
		if err != nil {
			return 0, fmt.Errorf("reencrypt device session: %w", err)
		}
		if _, err := tx.ExecContext(ctx, "UPDATE device_auth_sessions SET enc_device_code=?,credential_key_id=? WHERE id=?", reencrypted, k.activeID, item.id); err != nil {
			return 0, err
		}
	}
	oauthRows, err := tx.QueryContext(ctx, `SELECT id,enc_session FROM oauth_auth_sessions
		WHERE state IN ('pending','exchanging') AND length(enc_session)>0 AND credential_key_id<>?`, k.activeID)
	if err != nil {
		return 0, err
	}
	var oauthRecords []deviceRecord
	for oauthRows.Next() {
		var item deviceRecord
		if err := oauthRows.Scan(&item.id, &item.envelope); err != nil {
			oauthRows.Close()
			return 0, err
		}
		oauthRecords = append(oauthRecords, item)
	}
	if err := oauthRows.Close(); err != nil {
		return 0, err
	}
	for _, item := range oauthRecords {
		reencrypted, err := k.Reencrypt(item.envelope, OAuthSessionAAD(item.id))
		if err != nil {
			return 0, fmt.Errorf("reencrypt oauth session: %w", err)
		}
		if _, err := tx.ExecContext(ctx, "UPDATE oauth_auth_sessions SET enc_session=?,credential_key_id=? WHERE id=?", reencrypted, k.activeID, item.id); err != nil {
			return 0, err
		}
	}
	settingRows, err := tx.QueryContext(ctx, `SELECT key,value FROM settings
		WHERE is_secret=1 AND length(value)>0 AND key_id<>?`, k.activeID)
	if err != nil {
		return 0, err
	}
	type settingRecord struct {
		key      string
		envelope []byte
	}
	var settingRecords []settingRecord
	for settingRows.Next() {
		var item settingRecord
		if err := settingRows.Scan(&item.key, &item.envelope); err != nil {
			settingRows.Close()
			return 0, err
		}
		settingRecords = append(settingRecords, item)
	}
	if err := settingRows.Close(); err != nil {
		return 0, err
	}
	for _, item := range settingRecords {
		reencrypted, err := k.Reencrypt(item.envelope, SettingAAD(item.key))
		if err != nil {
			return 0, fmt.Errorf("reencrypt setting %s: %w", item.key, err)
		}
		if _, err := tx.ExecContext(ctx, "UPDATE settings SET value=?,key_id=? WHERE key=?", reencrypted, k.activeID, item.key); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return len(records) + len(deviceRecords) + len(oauthRecords) + len(settingRecords), nil
}

func CredentialAAD(accountID int64, credentialType string) []byte {
	return []byte("accounts:" + strconv.FormatInt(accountID, 10) + ":credentials:" + credentialType)
}

func DeviceSessionAAD(sessionID string) []byte {
	return []byte("device_auth_sessions:" + sessionID + ":device_code")
}

func OAuthSessionAAD(sessionID string) []byte {
	return []byte("oauth_auth_sessions:" + sessionID + ":session")
}

func SettingAAD(key string) []byte {
	return []byte("settings:" + key + ":secret")
}

func newGCM(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, errors.New("initialize credential cipher")
	}
	aead, err := cipher.NewGCMWithNonceSize(block, nonceSize)
	if err != nil {
		return nil, errors.New("initialize credential AEAD")
	}
	return aead, nil
}

func parseEnvelope(envelope []byte) (string, []byte, []byte, error) {
	minimum := len(envelopeMagic) + 1 + 1 + nonceSize + 16
	if len(envelope) < minimum || !equalPrefix(envelope, envelopeMagic[:]) {
		return "", nil, nil, errors.New("invalid credential envelope version")
	}
	keyLength := int(envelope[len(envelopeMagic)])
	offset := len(envelopeMagic) + 1
	if keyLength == 0 || offset+keyLength+nonceSize+16 > len(envelope) {
		return "", nil, nil, errors.New("invalid credential envelope")
	}
	keyID := string(envelope[offset : offset+keyLength])
	offset += keyLength
	nonce := envelope[offset : offset+nonceSize]
	ciphertext := envelope[offset+nonceSize:]
	return keyID, nonce, ciphertext, nil
}

func equalPrefix(value, prefix []byte) bool {
	if len(value) < len(prefix) {
		return false
	}
	var different byte
	for index := range prefix {
		different |= value[index] ^ prefix[index]
	}
	return different == 0
}

func zero(value []byte) {
	for index := range value {
		value[index] = 0
	}
}
