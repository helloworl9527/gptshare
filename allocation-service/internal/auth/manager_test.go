package auth

import (
	"context"
	"crypto/sha256"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"allocation-service/internal/store"
	"github.com/golang-jwt/jwt/v5"
	"github.com/pquerna/otp"
	"github.com/pquerna/otp/hotp"
	"golang.org/x/crypto/bcrypt"
)

var (
	testHashOnce sync.Once
	testHash     string
)

func TestPasswordTOTPReplaySessionAndLogout(t *testing.T) {
	m, closeDB := testManager(t)
	defer closeDB()
	ctx := context.Background()
	now := time.Date(2026, 7, 24, 8, 30, 0, 0, time.UTC)
	m.now = func() time.Time { return now }
	challenge, err := m.Password(ctx, "admin", "correct horse battery staple", "127.0.0.1", "preauth-csrf-value-with-enough-entropy")
	if err != nil {
		t.Fatal(err)
	}
	if _, exists := m.challenges[challenge]; exists {
		t.Fatal("raw challenge stored server-side")
	}
	code := codeAt(t, m.totpSecret, now)
	session, err := m.TOTP(ctx, challenge, code, "127.0.0.1", "preauth-csrf-value-with-enough-entropy")
	if err != nil {
		t.Fatal(err)
	}
	principal, err := m.Authenticate(ctx, session.Token)
	if err != nil || principal.Username != "admin" {
		t.Fatalf("principal=%v err=%v", principal, err)
	}
	if _, err := m.TOTP(ctx, challenge, code, "127.0.0.1", "preauth-csrf-value-with-enough-entropy"); !errors.Is(err, ErrInvalidChallenge) {
		t.Fatalf("challenge replay err=%v", err)
	}
	nextChallenge, err := m.Password(ctx, "admin", "correct horse battery staple", "127.0.0.1", "second-csrf-value-with-enough-entropy")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.TOTP(ctx, nextChallenge, code, "127.0.0.1", "second-csrf-value-with-enough-entropy"); !errors.Is(err, ErrTOTPReplay) {
		t.Fatalf("TOTP replay err=%v", err)
	}
	if err := m.Logout(ctx, principal.JTI); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Authenticate(ctx, session.Token); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("revoked session err=%v", err)
	}
}

func TestPasswordFailureAndLockout(t *testing.T) {
	m, closeDB := testManager(t)
	defer closeDB()
	ctx := context.Background()
	for _, request := range []struct{ username, password string }{{"missing", "wrong"}, {"admin", "wrong"}} {
		_, err := m.Password(ctx, request.username, request.password, "192.0.2.10", "csrf-value-with-enough-entropy")
		if !errors.Is(err, ErrInvalidCredentials) {
			t.Fatalf("%s err=%v", request.username, err)
		}
	}
	for attempt := 3; attempt <= 5; attempt++ {
		_, err := m.Password(ctx, "admin", "wrong", "192.0.2.10", "csrf-value-with-enough-entropy")
		if attempt < 5 && !errors.Is(err, ErrInvalidCredentials) {
			t.Fatalf("attempt %d err=%v", attempt, err)
		}
		if attempt == 5 {
			var limited *RateLimitError
			if !errors.As(err, &limited) {
				t.Fatalf("fifth attempt err=%v", err)
			}
		}
	}
}

func TestRejectsJWTAlgorithmAndExpiry(t *testing.T) {
	m, closeDB := testManager(t)
	defer closeDB()
	now := time.Date(2026, 7, 24, 8, 30, 0, 0, time.UTC)
	m.now = func() time.Time { return now }
	makeToken := func(method jwt.SigningMethod, expires time.Time) string {
		token, err := jwt.NewWithClaims(method, claims{RegisteredClaims: jwt.RegisteredClaims{
			Issuer: issuer, Subject: "admin", Audience: jwt.ClaimStrings{audience}, ID: "synthetic-jti",
			IssuedAt: jwt.NewNumericDate(now.Add(-time.Minute)), ExpiresAt: jwt.NewNumericDate(expires),
		}}).SignedString(m.sessionKey)
		if err != nil {
			t.Fatal(err)
		}
		return token
	}
	if _, err := m.Authenticate(context.Background(), makeToken(jwt.SigningMethodHS512, now.Add(time.Minute))); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("algorithm confusion err=%v", err)
	}
	if _, err := m.Authenticate(context.Background(), makeToken(jwt.SigningMethodHS256, now.Add(-time.Second))); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("expired JWT err=%v", err)
	}
}

func TestCSRFBindingUsesOnlyHashes(t *testing.T) {
	m, closeDB := testManager(t)
	defer closeDB()
	ctx := context.Background()
	if err := m.BindCSRF(ctx, "jti-sensitive", "csrf-sensitive"); err != nil {
		t.Fatal(err)
	}
	if !m.VerifyCSRF(ctx, "jti-sensitive", "csrf-sensitive") || m.VerifyCSRF(ctx, "jti-sensitive", "wrong") {
		t.Fatal("CSRF validation mismatch")
	}
	var key, value string
	if err := m.db.QueryRow("SELECT key, CAST(value AS TEXT) FROM settings WHERE key LIKE 'internal.auth.csrf.%'").Scan(&key, &value); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(key+value, "jti-sensitive") || strings.Contains(key+value, "csrf-sensitive") {
		t.Fatal("raw CSRF or JTI persisted")
	}
}

func TestLimiterHMACPersistence(t *testing.T) {
	m, closeDB := testManager(t)
	defer closeDB()
	now := time.Date(2026, 7, 24, 8, 30, 0, 0, time.UTC)
	if _, _, err := m.limiter.record(context.Background(), "admin", "198.51.100.1", "failure", now); err != nil {
		t.Fatal(err)
	}
	var usernameHMAC, ipHMAC []byte
	if err := m.db.QueryRow("SELECT username_hmac,client_ip_hmac FROM admin_login_attempts LIMIT 1").Scan(&usernameHMAC, &ipHMAC); err != nil {
		t.Fatal(err)
	}
	if len(usernameHMAC) != sha256.Size || len(ipHMAC) != sha256.Size {
		t.Fatalf("HMAC lengths username=%d ip=%d", len(usernameHMAC), len(ipHMAC))
	}
}

func testManager(t *testing.T) (*Manager, func()) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "private")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	database, err := store.Open(context.Background(), filepath.Join(dir, "auth.db"))
	if err != nil {
		t.Fatal(err)
	}
	testHashOnce.Do(func() {
		hash, hashErr := bcrypt.GenerateFromPassword([]byte("correct horse battery staple"), 12)
		if hashErr != nil {
			t.Fatal(hashErr)
		}
		testHash = string(hash)
	})
	m, err := New(Config{
		DB: database.DB(), Username: "admin", PasswordHash: testHash, TOTPSecret: []byte(strings.Repeat("s", 20)),
		SessionKey: []byte(strings.Repeat("j", 32)), CSRFSigningKey: []byte(strings.Repeat("r", 32)),
	})
	if err != nil {
		database.Close()
		t.Fatal(err)
	}
	return m, func() { database.Close() }
}

func codeAt(t *testing.T, secret string, at time.Time) string {
	t.Helper()
	code, err := hotp.GenerateCodeCustom(secret, uint64(at.Unix()/30), hotp.ValidateOpts{Digits: otp.DigitsSix, Algorithm: otp.AlgorithmSHA1})
	if err != nil {
		t.Fatal(err)
	}
	return code
}
