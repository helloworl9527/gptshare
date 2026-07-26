package auth

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"chatgpt-monitor/internal/store"
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
	now := time.Date(2026, 7, 22, 8, 30, 0, 0, time.UTC)
	m.now = func() time.Time { return now }

	challenge, err := m.Password(ctx, "admin", "correct horse battery staple", "127.0.0.1", "preauth-csrf-value-with-enough-entropy")
	if err != nil {
		t.Fatal(err)
	}
	if _, exists := m.challenges[challenge]; exists {
		t.Fatal("raw challenge was stored server-side")
	}
	code := codeAt(t, m.totpSecret, now)
	session, err := m.TOTP(ctx, challenge, code, "127.0.0.1", "preauth-csrf-value-with-enough-entropy")
	if err != nil {
		t.Fatal(err)
	}
	principal, err := m.Authenticate(ctx, session.Token)
	if err != nil || principal.Username != "admin" {
		t.Fatalf("authenticate principal=%v err=%v", principal, err)
	}
	if _, err := m.TOTP(ctx, challenge, code, "127.0.0.1", "preauth-csrf-value-with-enough-entropy"); !errors.Is(err, ErrInvalidChallenge) {
		t.Fatalf("challenge replay error=%v", err)
	}

	nextChallenge, err := m.Password(ctx, "admin", "correct horse battery staple", "127.0.0.1", "second-csrf-value-with-enough-entropy")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.TOTP(ctx, nextChallenge, code, "127.0.0.1", "second-csrf-value-with-enough-entropy"); !errors.Is(err, ErrTOTPReplay) {
		t.Fatalf("TOTP replay error=%v", err)
	}
	if err := m.Logout(ctx, principal.JTI); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Authenticate(ctx, session.Token); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("revoked session error=%v", err)
	}
}

func TestPasswordFailureIsUniformAndFifthAttemptIsLimited(t *testing.T) {
	m, closeDB := testManager(t)
	defer closeDB()
	ctx := context.Background()
	for _, request := range []struct{ username, password string }{{"missing", "wrong"}, {"admin", "wrong"}} {
		_, err := m.Password(ctx, request.username, request.password, "192.0.2.10", "csrf-value-with-enough-entropy")
		if !errors.Is(err, ErrInvalidCredentials) {
			t.Fatalf("%s error=%v", request.username, err)
		}
	}
	for attempt := 3; attempt <= 5; attempt++ {
		_, err := m.Password(ctx, "admin", "wrong", "192.0.2.10", "csrf-value-with-enough-entropy")
		if attempt < 5 && !errors.Is(err, ErrInvalidCredentials) {
			t.Fatalf("attempt %d error=%v", attempt, err)
		}
		if attempt == 5 {
			var limited *RateLimitError
			if !errors.As(err, &limited) {
				t.Fatalf("fifth attempt error=%v", err)
			}
		}
	}

	restarted, err := New(Config{DB: m.db, Username: m.username, PasswordHash: string(m.passwordHash), TOTPSecret: []byte(strings.Repeat("s", 20)), JWTKey: []byte(strings.Repeat("j", 32)), RateLimitKey: []byte(strings.Repeat("r", 32))})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := restarted.Password(ctx, "admin", "correct horse battery staple", "192.0.2.10", "csrf-value-with-enough-entropy"); err == nil {
		t.Fatal("persistent limit was lost after manager restart")
	}
}

func TestChallengeExpiryAndContextBinding(t *testing.T) {
	m, closeDB := testManager(t)
	defer closeDB()
	ctx := context.Background()
	now := time.Date(2026, 7, 22, 8, 30, 0, 0, time.UTC)
	m.now = func() time.Time { return now }
	challenge, err := m.Password(ctx, "admin", "correct horse battery staple", "127.0.0.1", "original-csrf-value-with-enough-entropy")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.TOTP(ctx, challenge, codeAt(t, m.totpSecret, now), "127.0.0.2", "original-csrf-value-with-enough-entropy"); !errors.Is(err, ErrInvalidChallenge) {
		t.Fatalf("IP binding error=%v", err)
	}
	challenge, err = m.Password(ctx, "admin", "correct horse battery staple", "127.0.0.1", "original-csrf-value-with-enough-entropy")
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(challengeTTL)
	if _, err := m.TOTP(ctx, challenge, codeAt(t, m.totpSecret, now), "127.0.0.1", "original-csrf-value-with-enough-entropy"); !errors.Is(err, ErrInvalidChallenge) {
		t.Fatalf("expiry error=%v", err)
	}
}

func TestChallengeHasFiveAttemptMaximumAndAcceptsAdjacentTimestep(t *testing.T) {
	m, closeDB := testManager(t)
	defer closeDB()
	ctx := context.Background()
	now := time.Date(2026, 7, 22, 8, 30, 30, 0, time.UTC)
	m.now = func() time.Time { return now }
	challenge, err := m.Password(ctx, "admin", "correct horse battery staple", "127.0.0.1", "attempt-csrf-value-with-enough-entropy")
	if err != nil {
		t.Fatal(err)
	}
	for attempt := 1; attempt <= challengeAttempts; attempt++ {
		_, _ = m.TOTP(ctx, challenge, "000000", "127.0.0.1", "attempt-csrf-value-with-enough-entropy")
	}
	hash := sha256.Sum256([]byte(challenge))
	if _, exists := m.challenges[base64.RawURLEncoding.EncodeToString(hash[:])]; exists {
		t.Fatal("challenge survived five attempts")
	}

	m, closeSecond := testManager(t)
	defer closeSecond()
	m.now = func() time.Time { return now }
	challenge, err = m.Password(ctx, "admin", "correct horse battery staple", "127.0.0.1", "skew-csrf-value-with-enough-entropy")
	if err != nil {
		t.Fatal(err)
	}
	previousCode := codeAt(t, m.totpSecret, now.Add(-30*time.Second))
	if _, err := m.TOTP(ctx, challenge, previousCode, "127.0.0.1", "skew-csrf-value-with-enough-entropy"); err != nil {
		t.Fatalf("adjacent TOTP timestep rejected: %v", err)
	}
}

func TestRejectsJWTAlgorithmAndExpiry(t *testing.T) {
	m, closeDB := testManager(t)
	defer closeDB()
	now := time.Date(2026, 7, 22, 8, 30, 0, 0, time.UTC)
	m.now = func() time.Time { return now }
	makeToken := func(method jwt.SigningMethod, expires time.Time) string {
		token, err := jwt.NewWithClaims(method, claims{RegisteredClaims: jwt.RegisteredClaims{
			Issuer: issuer, Subject: "admin", Audience: jwt.ClaimStrings{audience}, ID: "synthetic-jti",
			IssuedAt: jwt.NewNumericDate(now.Add(-time.Minute)), ExpiresAt: jwt.NewNumericDate(expires),
		}}).SignedString(m.jwtKey)
		if err != nil {
			t.Fatal(err)
		}
		return token
	}
	if _, err := m.Authenticate(context.Background(), makeToken(jwt.SigningMethodHS512, now.Add(time.Minute))); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("algorithm confusion error=%v", err)
	}
	if _, err := m.Authenticate(context.Background(), makeToken(jwt.SigningMethodHS256, now.Add(-time.Second))); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("expired JWT error=%v", err)
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
		t.Fatal("CSRF binding validation mismatch")
	}
	var key, value string
	if err := m.db.QueryRow("SELECT key, CAST(value AS TEXT) FROM settings WHERE key LIKE 'internal.auth.csrf.%'").Scan(&key, &value); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(key+value, "jti-sensitive") || strings.Contains(key+value, "csrf-sensitive") {
		t.Fatal("raw CSRF or jti persisted")
	}
}

func TestLimiterGlobalLayerAndHMACPersistence(t *testing.T) {
	m, closeDB := testManager(t)
	defer closeDB()
	now := time.Date(2026, 7, 22, 8, 30, 0, 0, time.UTC)
	for i := 0; i < globalLimit; i++ {
		username := "user-" + string(rune('a'+i/4))
		ip := "198.51.100." + string(rune('1'+i%9))
		limited, _, err := m.limiter.record(context.Background(), username, ip, "failure", now.Add(time.Duration(i)*time.Millisecond))
		if err != nil {
			t.Fatal(err)
		}
		if i == globalLimit-1 && !limited {
			t.Fatal("global limit did not engage")
		}
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
	migrationRoot, _ := filepath.Abs(filepath.Join("..", "..", "migrations"))
	database, err := store.Open(context.Background(), filepath.Join(dir, "auth.db"), os.DirFS(migrationRoot))
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
	m, err := New(Config{DB: database.DB(), Username: "admin", PasswordHash: testHash, TOTPSecret: []byte(strings.Repeat("s", 20)), JWTKey: []byte(strings.Repeat("j", 32)), RateLimitKey: []byte(strings.Repeat("r", 32))})
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
