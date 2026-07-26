package notify

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	credentialcrypto "chatgpt-monitor/internal/crypto"
	"chatgpt-monitor/internal/store"
)

type notifierFunc func(context.Context, Event) error

func (fn notifierFunc) Send(ctx context.Context, event Event) error { return fn(ctx, event) }

func TestConsumerIdempotentConcurrentAndSanitizedLog(t *testing.T) {
	db, closeDB := testDB(t)
	defer closeDB()
	eventID := insertEvent(t, db, "epoch:1:alive_to_dead_banned")
	logs := new(bytes.Buffer)
	consumer, _ := NewConsumer(db, NewLogNotifier(slog.New(slog.NewJSONHandler(logs, nil))))
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() { defer wg.Done(); _, _ = consumer.ProcessOnce(context.Background()) }()
	}
	wg.Wait()
	var status string
	var attempts int
	if err := db.QueryRow("SELECT delivery_status,attempts FROM alert_events WHERE id=?", eventID).Scan(&status, &attempts); err != nil {
		t.Fatal(err)
	}
	if status != "recorded_no_channel" || attempts != 1 {
		t.Fatalf("status=%s attempts=%d", status, attempts)
	}
	if count := bytes.Count(logs.Bytes(), []byte("alert recorded")); count != 1 {
		t.Fatalf("log records=%d: %s", count, logs.String())
	}
	if bytes.Contains(logs.Bytes(), []byte("credential")) || bytes.Contains(logs.Bytes(), []byte("token")) {
		t.Fatal("sanitized alert log contains secret vocabulary")
	}
	if processed, err := consumer.ProcessOnce(context.Background()); err != nil || processed {
		t.Fatalf("replay processed=%v err=%v", processed, err)
	}
}

func TestConsumerFailureRetryLimitAndCrashRecovery(t *testing.T) {
	db, closeDB := testDB(t)
	defer closeDB()
	eventID := insertEvent(t, db, "epoch:2:alive_to_dead_banned")
	now := time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC)
	calls := 0
	consumer, _ := NewConsumer(db, notifierFunc(func(context.Context, Event) error {
		calls++
		return errors.New("synthetic adapter failure with no response body")
	}))
	consumer.now = func() time.Time { return now }
	for range maxAttempts {
		if _, err := consumer.ProcessOnce(context.Background()); err != nil {
			t.Fatal(err)
		}
		now = now.Add(10 * time.Second)
	}
	if processed, err := consumer.ProcessOnce(context.Background()); err != nil || processed {
		t.Fatalf("terminal retry processed=%v err=%v", processed, err)
	}
	var status, code string
	var attempts int
	if err := db.QueryRow("SELECT delivery_status,attempts,last_error_code FROM alert_events WHERE id=?", eventID).Scan(&status, &attempts, &code); err != nil {
		t.Fatal(err)
	}
	if status != "failed" || attempts != maxAttempts || code != "adapter_failed" || calls != maxAttempts {
		t.Fatalf("status=%s attempts=%d code=%s calls=%d", status, attempts, code, calls)
	}

	recoveredID := insertEvent(t, db, "epoch:3:alive_to_dead_banned")
	old := now.Add(-claimTimeout - time.Second).Format(time.RFC3339Nano)
	if _, err := db.Exec("UPDATE alert_events SET delivery_status='processing',attempts=1,claimed_at=?,updated_at=? WHERE id=?", old, old, recoveredID); err != nil {
		t.Fatal(err)
	}
	okConsumer, _ := NewConsumer(db, notifierFunc(func(context.Context, Event) error { return nil }))
	okConsumer.now = func() time.Time { return now }
	if processed, err := okConsumer.ProcessOnce(context.Background()); err != nil || !processed {
		t.Fatalf("recovery processed=%v err=%v", processed, err)
	}
	if err := db.QueryRow("SELECT delivery_status,attempts FROM alert_events WHERE id=?", recoveredID).Scan(&status, &attempts); err != nil {
		t.Fatal(err)
	}
	if status != "recorded_no_channel" || attempts != 2 {
		t.Fatalf("recovered status=%s attempts=%d", status, attempts)
	}
}

func TestSettingsBoundsWriteOnlyClearAndInternalIsolation(t *testing.T) {
	db, closeDB := testDB(t)
	defer closeDB()
	keyring := testKeyring(t, "key-a", 1)
	service, _ := NewSettingsService(db, keyring)
	secret := "synthetic-channel-secret-never-return"
	poll, days := 1800, 5
	truth := false
	settings, err := service.Update(context.Background(), Update{
		PollInterval: &poll, NearExpiryDays: &days,
		Channels: map[string]ChannelUpdate{"telegram": {Enabled: &truth, Secret: secret}},
	}, "admin")
	if err != nil {
		t.Fatal(err)
	}
	if settings.PollInterval != poll || settings.NearExpiryDays != days || !settings.Channels["telegram"].Configured || settings.Channels["telegram"].Enabled || settings.Channels["telegram"].Connected {
		t.Fatalf("settings=%+v", settings)
	}
	var envelope []byte
	var keyID string
	key := channelKey("telegram")
	if err := db.QueryRow("SELECT value,key_id FROM settings WHERE key=? AND is_secret=1", key).Scan(&envelope, &keyID); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(envelope, []byte(secret)) || keyID != "key-a" {
		t.Fatal("secret was not encrypted")
	}
	opened, err := keyring.Open(envelope, credentialcrypto.SettingAAD(key))
	if err != nil || string(opened) != secret {
		t.Fatalf("open=%q err=%v", opened, err)
	}
	if _, err := service.Update(context.Background(), Update{Channels: map[string]ChannelUpdate{"telegram": {Secret: ""}}}, "admin"); err != nil {
		t.Fatal(err)
	}
	var unchanged []byte
	if err := db.QueryRow("SELECT value FROM settings WHERE key=?", key).Scan(&unchanged); err != nil || !bytes.Equal(unchanged, envelope) {
		t.Fatal("empty secret overwrote existing secret")
	}
	if _, err := db.Exec("INSERT INTO settings(key,value,is_secret,key_id,updated_at) VALUES ('internal.evidence.private',x'01',0,NULL,?)", time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	settings, err = service.Get(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(settings.Channels) != 3 {
		t.Fatalf("channels=%v", settings.Channels)
	}
	if err := service.DeleteSecret(context.Background(), "telegram", "admin"); err != nil {
		t.Fatal(err)
	}
	if err := service.DeleteSecret(context.Background(), "telegram", "admin"); err != nil {
		t.Fatal(err)
	}
	settings, _ = service.Get(context.Background())
	if settings.Channels["telegram"].Configured {
		t.Fatal("secret remained configured")
	}
	var audits int
	if err := db.QueryRow("SELECT count(*) FROM settings_audit WHERE setting_key=?", key).Scan(&audits); err != nil || audits != 3 {
		t.Fatalf("audits=%d err=%v", audits, err)
	}

	tooSmall, tooLarge, zeroDays, tooManyDays := 899, 86401, 0, 31
	for _, update := range []Update{{PollInterval: &tooSmall}, {PollInterval: &tooLarge}, {NearExpiryDays: &zeroDays}, {NearExpiryDays: &tooManyDays}} {
		if _, err := service.Update(context.Background(), update, "admin"); err == nil {
			t.Fatal("invalid settings accepted")
		}
	}
	enabled := true
	if _, err := service.Update(context.Background(), Update{Channels: map[string]ChannelUpdate{"wecom": {Enabled: &enabled}}}, "admin"); err == nil {
		t.Fatal("connected channel accepted")
	}
}

func TestSettingsSecretRotation(t *testing.T) {
	db, closeDB := testDB(t)
	defer closeDB()
	oldRing := testKeyring(t, "old", 1)
	service, _ := NewSettingsService(db, oldRing)
	if _, err := service.Update(context.Background(), Update{Channels: map[string]ChannelUpdate{"feishu": {Secret: "rotation-value"}}}, "admin"); err != nil {
		t.Fatal(err)
	}
	rotating, _ := credentialcrypto.NewKeyring(map[string][]byte{"old": bytes.Repeat([]byte{1}, 32), "new": bytes.Repeat([]byte{2}, 32)}, "new")
	count, err := rotating.ReencryptAccounts(context.Background(), db)
	if err != nil || count != 1 {
		t.Fatalf("count=%d err=%v", count, err)
	}
	var envelope []byte
	var keyID string
	key := channelKey("feishu")
	if err := db.QueryRow("SELECT value,key_id FROM settings WHERE key=?", key).Scan(&envelope, &keyID); err != nil {
		t.Fatal(err)
	}
	opened, err := rotating.Open(envelope, credentialcrypto.SettingAAD(key))
	if err != nil || string(opened) != "rotation-value" || keyID != "new" {
		t.Fatalf("key=%s open=%q err=%v", keyID, opened, err)
	}
}

func TestSettingsConcurrentIndependentUpdates(t *testing.T) {
	db, closeDB := testDB(t)
	defer closeDB()
	service, _ := NewSettingsService(db, testKeyring(t, "active", 3))
	poll, days := 7200, 9
	start := make(chan struct{})
	errs := make(chan error, 2)
	go func() {
		<-start
		_, err := service.Update(context.Background(), Update{PollInterval: &poll}, "admin")
		errs <- err
	}()
	go func() {
		<-start
		_, err := service.Update(context.Background(), Update{NearExpiryDays: &days}, "admin")
		errs <- err
	}()
	close(start)
	for range 2 {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	settings, err := service.Get(context.Background())
	if err != nil || settings.PollInterval != poll || settings.NearExpiryDays != days {
		t.Fatalf("settings=%+v err=%v", settings, err)
	}
}

func testDB(t *testing.T) (*sql.DB, func()) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "private")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	migrations, _ := filepath.Abs(filepath.Join("..", "..", "migrations"))
	opened, err := store.Open(context.Background(), filepath.Join(dir, "test.db"), os.DirFS(migrations))
	if err != nil {
		t.Fatal(err)
	}
	return opened.DB(), func() { _ = opened.Close() }
}

func insertEvent(t *testing.T, db *sql.DB, eventKey string) int64 {
	t.Helper()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	result, err := db.Exec(`INSERT INTO accounts(provider_account_id,token_type,enc_credentials,credential_key_id,auth_expiry,import_time,updated_at)
		VALUES (?, 'access', x'01', 'test', ?, ?, ?)`, eventKey, now, now, now)
	if err != nil {
		t.Fatal(err)
	}
	accountID, _ := result.LastInsertId()
	epochResult, err := db.Exec("INSERT INTO authorization_epochs(account_id,started_at,auth_expiry) VALUES (?,?,?)", accountID, now, now)
	if err != nil {
		t.Fatal(err)
	}
	epochID, _ := epochResult.LastInsertId()
	result, err = db.Exec(`INSERT INTO alert_events(account_id,epoch_id,event_key,event_type,created_at,updated_at)
		VALUES (?,?,?,?,?,?)`, accountID, epochID, eventKey, "account_banned", now, now)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := result.LastInsertId()
	return id
}

func testKeyring(t *testing.T, id string, fill byte) *credentialcrypto.Keyring {
	t.Helper()
	ring, err := credentialcrypto.NewKeyring(map[string][]byte{id: bytes.Repeat([]byte{fill}, 32)}, id)
	if err != nil {
		t.Fatal(err)
	}
	return ring
}
