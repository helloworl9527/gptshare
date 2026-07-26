package notify

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"time"

	credentialcrypto "chatgpt-monitor/internal/crypto"
)

const (
	DefaultPollInterval = 3600
	MinPollInterval     = 900
	MaxPollInterval     = 86400
	DefaultNearExpiry   = 3
)

var channelNames = map[string]bool{"telegram": true, "wecom": true, "feishu": true}

type SettingsCipher interface {
	ActiveKeyID() string
	Seal([]byte, []byte) ([]byte, error)
}

type ChannelState struct {
	Enabled    bool `json:"enabled"`
	Configured bool `json:"configured"`
	Connected  bool `json:"connected"`
}

type Settings struct {
	PollInterval   int                     `json:"poll_interval"`
	NearExpiryDays int                     `json:"near_expiry_days"`
	Channels       map[string]ChannelState `json:"channels"`
}

type ChannelUpdate struct {
	Enabled *bool  `json:"enabled,omitempty"`
	Secret  string `json:"secret,omitempty"`
}

type Update struct {
	PollInterval   *int                     `json:"poll_interval,omitempty"`
	NearExpiryDays *int                     `json:"near_expiry_days,omitempty"`
	Channels       map[string]ChannelUpdate `json:"channels,omitempty"`
}

type SettingsError struct{ Code string }

func (e *SettingsError) Error() string { return "settings: " + e.Code }

type SettingsService struct {
	db     *sql.DB
	cipher SettingsCipher
	now    func() time.Time
}

func NewSettingsService(db *sql.DB, cipher SettingsCipher) (*SettingsService, error) {
	if db == nil || cipher == nil {
		return nil, errors.New("settings dependencies are required")
	}
	return &SettingsService{db: db, cipher: cipher, now: time.Now}, nil
}

func (s *SettingsService) Get(ctx context.Context) (Settings, error) {
	result := Settings{PollInterval: DefaultPollInterval, NearExpiryDays: DefaultNearExpiry, Channels: defaultChannels()}
	rows, err := s.db.QueryContext(ctx, `SELECT key,value,is_secret FROM settings
		WHERE key IN ('poll_interval','near_expiry_days','channel.telegram.secret','channel.wecom.secret','channel.feishu.secret')`)
	if err != nil {
		return Settings{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var key string
		var value []byte
		var secret int
		if err := rows.Scan(&key, &value, &secret); err != nil {
			return Settings{}, err
		}
		switch key {
		case "poll_interval":
			if parsed, parseErr := strconv.Atoi(string(value)); parseErr == nil {
				result.PollInterval = parsed
			}
		case "near_expiry_days":
			if parsed, parseErr := strconv.Atoi(string(value)); parseErr == nil {
				result.NearExpiryDays = parsed
			}
		default:
			if secret == 1 && len(value) > 0 {
				channel := key[len("channel.") : len(key)-len(".secret")]
				result.Channels[channel] = ChannelState{Enabled: false, Configured: true, Connected: false}
			}
		}
	}
	return result, rows.Err()
}

func (s *SettingsService) Update(ctx context.Context, update Update, actor string) (Settings, error) {
	if update.PollInterval != nil && (*update.PollInterval < MinPollInterval || *update.PollInterval > MaxPollInterval) {
		return Settings{}, &SettingsError{Code: "poll_interval_out_of_range"}
	}
	if update.NearExpiryDays != nil && (*update.NearExpiryDays < 1 || *update.NearExpiryDays > 30) {
		return Settings{}, &SettingsError{Code: "near_expiry_days_out_of_range"}
	}
	for channel, item := range update.Channels {
		if !channelNames[channel] {
			return Settings{}, &SettingsError{Code: "unknown_channel"}
		}
		if item.Enabled != nil && *item.Enabled {
			return Settings{}, &SettingsError{Code: "channels_not_connected"}
		}
		if len(item.Secret) > 4096 {
			return Settings{}, &SettingsError{Code: "secret_too_large"}
		}
	}
	if actor == "" {
		actor = "admin"
	}
	now := s.now().UTC().Format(time.RFC3339Nano)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Settings{}, err
	}
	defer tx.Rollback()
	writePlain := func(key string, value int) error {
		if _, err := tx.ExecContext(ctx, `INSERT INTO settings(key,value,is_secret,key_id,updated_at) VALUES (?,?,0,NULL,?)
			ON CONFLICT(key) DO UPDATE SET value=excluded.value,is_secret=0,key_id=NULL,updated_at=excluded.updated_at`, key, strconv.Itoa(value), now); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, "INSERT INTO settings_audit(at,actor,action,setting_key,configured) VALUES (?,?,?,?,1)", now, actor, "update", key)
		return err
	}
	if update.PollInterval != nil {
		if err := writePlain("poll_interval", *update.PollInterval); err != nil {
			return Settings{}, err
		}
	}
	if update.NearExpiryDays != nil {
		if err := writePlain("near_expiry_days", *update.NearExpiryDays); err != nil {
			return Settings{}, err
		}
	}
	for channel, item := range update.Channels {
		if item.Secret == "" {
			continue
		}
		key := channelKey(channel)
		envelope, err := s.cipher.Seal([]byte(item.Secret), credentialcrypto.SettingAAD(key))
		if err != nil {
			return Settings{}, fmt.Errorf("encrypt channel setting: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO settings(key,value,is_secret,key_id,updated_at) VALUES (?,?,1,?,?)
			ON CONFLICT(key) DO UPDATE SET value=excluded.value,is_secret=1,key_id=excluded.key_id,updated_at=excluded.updated_at`, key, envelope, s.cipher.ActiveKeyID(), now); err != nil {
			return Settings{}, err
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO settings_audit(at,actor,action,setting_key,configured) VALUES (?,?,?,?,1)", now, actor, "secret_set", key); err != nil {
			return Settings{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return Settings{}, err
	}
	return s.Get(ctx)
}

func (s *SettingsService) DeleteSecret(ctx context.Context, channel, actor string) error {
	if !channelNames[channel] {
		return &SettingsError{Code: "unknown_channel"}
	}
	if actor == "" {
		actor = "admin"
	}
	now := s.now().UTC().Format(time.RFC3339Nano)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	key := channelKey(channel)
	if _, err := tx.ExecContext(ctx, "DELETE FROM settings WHERE key=?", key); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, "INSERT INTO settings_audit(at,actor,action,setting_key,configured) VALUES (?,?,?,?,0)", now, actor, "secret_clear", key); err != nil {
		return err
	}
	return tx.Commit()
}

func channelKey(channel string) string { return "channel." + channel + ".secret" }

func defaultChannels() map[string]ChannelState {
	return map[string]ChannelState{
		"telegram": {Enabled: false, Connected: false},
		"wecom":    {Enabled: false, Connected: false},
		"feishu":   {Enabled: false, Connected: false},
	}
}
