package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base32"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"strconv"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/pquerna/otp"
	"github.com/pquerna/otp/hotp"
	"golang.org/x/crypto/bcrypt"
)

const (
	challengeTTL      = 2 * time.Minute
	challengeAttempts = 5
	defaultSessionTTL = 15 * time.Minute
	issuer            = "allocation-service"
	audience          = "allocation-service-admin"
)

var (
	ErrInvalidCredentials = errors.New("invalid credentials")
	ErrInvalidChallenge   = errors.New("invalid challenge")
	ErrTOTPReplay         = errors.New("TOTP replay")
	ErrUnauthorized       = errors.New("unauthorized")
	ErrForbidden          = errors.New("forbidden")
)

type RateLimitError struct{ RetryAfter time.Duration }

func (e *RateLimitError) Error() string { return "rate limited" }

type Config struct {
	DB             *sql.DB
	Username       string
	PasswordHash   string
	TOTPSecret     []byte
	SessionKey     []byte
	CSRFSigningKey []byte
	SessionTTL     time.Duration
}

type Manager struct {
	db           *sql.DB
	username     string
	passwordHash []byte
	totpSecret   string
	sessionKey   []byte
	limiter      *limiter
	sessionTTL   time.Duration
	now          func() time.Time
	random       io.Reader

	mu         sync.Mutex
	challenges map[string]*challenge
}

type challenge struct {
	expiresAt time.Time
	attempts  int
	ipHMAC    [32]byte
	csrfHash  [32]byte
}

type Session struct {
	Token     string
	JTI       string
	ExpiresAt time.Time
}

type Principal struct {
	Username string
	JTI      string
	Expires  time.Time
}

type claims struct {
	jwt.RegisteredClaims
}

func New(cfg Config) (*Manager, error) {
	if cfg.DB == nil || cfg.Username == "" || len(cfg.SessionKey) < 32 || len(cfg.CSRFSigningKey) < 32 || len(cfg.TOTPSecret) < 20 {
		return nil, errors.New("invalid auth configuration")
	}
	cost, err := bcrypt.Cost([]byte(cfg.PasswordHash))
	if err != nil || cost < 12 {
		return nil, errors.New("password hash must be bcrypt cost 12 or greater")
	}
	if cfg.SessionTTL <= 0 {
		cfg.SessionTTL = defaultSessionTTL
	}
	return &Manager{
		db: cfg.DB, username: cfg.Username, passwordHash: []byte(cfg.PasswordHash),
		totpSecret: base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(cfg.TOTPSecret),
		sessionKey: append([]byte(nil), cfg.SessionKey...), limiter: newLimiter(cfg.DB, cfg.CSRFSigningKey),
		sessionTTL: cfg.SessionTTL, now: time.Now, random: rand.Reader, challenges: make(map[string]*challenge),
	}, nil
}

func (m *Manager) Password(ctx context.Context, username, password, clientIP, csrfToken string) (string, error) {
	now := m.now().UTC()
	limited, retry, err := m.limiter.check(ctx, username, clientIP, now)
	if err != nil {
		return "", err
	}
	if limited {
		return "", &RateLimitError{RetryAfter: retry}
	}
	wantUser := sha256.Sum256([]byte(m.username))
	gotUser := sha256.Sum256([]byte(username))
	userOK := subtle.ConstantTimeCompare(wantUser[:], gotUser[:]) == 1
	passwordOK := bcrypt.CompareHashAndPassword(m.passwordHash, []byte(password)) == nil
	if !userOK || !passwordOK {
		limited, retry, recordErr := m.limiter.record(ctx, username, clientIP, "failure", now)
		if recordErr != nil {
			return "", recordErr
		}
		if limited {
			return "", &RateLimitError{RetryAfter: retry}
		}
		return "", ErrInvalidCredentials
	}
	raw, hash, err := m.newOpaque()
	if err != nil {
		return "", err
	}
	m.mu.Lock()
	m.pruneChallenges(now)
	m.challenges[hash] = &challenge{expiresAt: now.Add(challengeTTL), ipHMAC: m.limiter.ipDigest(clientIP), csrfHash: sha256.Sum256([]byte(csrfToken))}
	m.mu.Unlock()
	if _, _, err := m.limiter.record(ctx, username, clientIP, "success", now); err != nil {
		m.mu.Lock()
		delete(m.challenges, hash)
		m.mu.Unlock()
		return "", err
	}
	return raw, nil
}

func (m *Manager) TOTP(ctx context.Context, rawChallenge, passcode, clientIP, csrfToken string) (*Session, error) {
	now := m.now().UTC()
	limited, retry, err := m.limiter.check(ctx, m.username, clientIP, now)
	if err != nil {
		return nil, err
	}
	if limited {
		return nil, &RateLimitError{RetryAfter: retry}
	}
	hashBytes := sha256.Sum256([]byte(rawChallenge))
	hash := base64.RawURLEncoding.EncodeToString(hashBytes[:])
	ipDigest := m.limiter.ipDigest(clientIP)
	csrfDigest := sha256.Sum256([]byte(csrfToken))
	m.mu.Lock()
	entry, ok := m.challenges[hash]
	if !ok || !now.Before(entry.expiresAt) || subtle.ConstantTimeCompare(entry.ipHMAC[:], ipDigest[:]) != 1 || subtle.ConstantTimeCompare(entry.csrfHash[:], csrfDigest[:]) != 1 {
		if ok {
			delete(m.challenges, hash)
		}
		m.mu.Unlock()
		return nil, m.failedTOTP(ctx, clientIP, now, ErrInvalidChallenge)
	}
	entry.attempts++
	if entry.attempts >= challengeAttempts {
		delete(m.challenges, hash)
	}
	step, valid := m.validateTOTP(passcode, now)
	if !valid {
		m.mu.Unlock()
		return nil, m.failedTOTP(ctx, clientIP, now, ErrInvalidCredentials)
	}
	session, err := m.createSession(ctx, step, now)
	if err != nil {
		m.mu.Unlock()
		if errors.Is(err, ErrTOTPReplay) {
			return nil, m.failedTOTP(ctx, clientIP, now, err)
		}
		return nil, err
	}
	delete(m.challenges, hash)
	m.mu.Unlock()
	if _, _, err := m.limiter.record(ctx, m.username, clientIP, "success", now); err != nil {
		return nil, err
	}
	return session, nil
}

func (m *Manager) failedTOTP(ctx context.Context, clientIP string, now time.Time, cause error) error {
	limited, retry, err := m.limiter.record(ctx, m.username, clientIP, "failure", now)
	if err != nil {
		return err
	}
	if limited {
		return &RateLimitError{RetryAfter: retry}
	}
	return cause
}

func (m *Manager) validateTOTP(passcode string, now time.Time) (int64, bool) {
	if len(passcode) != 6 {
		return 0, false
	}
	current := now.Unix() / 30
	opts := hotp.ValidateOpts{Digits: otp.DigitsSix, Algorithm: otp.AlgorithmSHA1}
	for _, offset := range []int64{0, -1, 1} {
		step := current + offset
		if step < 0 {
			continue
		}
		valid, err := hotp.ValidateCustom(passcode, uint64(step), m.totpSecret, opts)
		if err == nil && valid {
			return step, true
		}
	}
	return 0, false
}

func (m *Manager) createSession(ctx context.Context, totpStep int64, now time.Time) (*Session, error) {
	jti, jtiHash, err := m.newOpaque()
	if err != nil {
		return nil, err
	}
	expires := now.Add(m.sessionTTL)
	token, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims{RegisteredClaims: jwt.RegisteredClaims{
		Issuer: issuer, Subject: m.username, Audience: jwt.ClaimStrings{audience},
		ExpiresAt: jwt.NewNumericDate(expires), IssuedAt: jwt.NewNumericDate(now), NotBefore: jwt.NewNumericDate(now), ID: jti,
	}}).SignedString(m.sessionKey)
	if err != nil {
		return nil, fmt.Errorf("sign session: %w", err)
	}
	tx, err := m.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var lastText string
	err = tx.QueryRowContext(ctx, "SELECT CAST(value AS TEXT) FROM settings WHERE key='internal.auth.totp_last_step'").Scan(&lastText)
	if err != nil && err != sql.ErrNoRows {
		return nil, err
	}
	if err == nil {
		last, parseErr := strconv.ParseInt(lastText, 10, 64)
		if parseErr != nil {
			return nil, errors.New("invalid persisted TOTP replay state")
		}
		if totpStep <= last {
			return nil, ErrTOTPReplay
		}
	}
	stamp := now.Format(time.RFC3339Nano)
	if _, err := tx.ExecContext(ctx, `INSERT INTO settings(key,value,is_secret,key_id,updated_at)
		VALUES ('internal.auth.totp_last_step', ?, 0, NULL, ?)
		ON CONFLICT(key) DO UPDATE SET value=excluded.value, updated_at=excluded.updated_at`, strconv.FormatInt(totpStep, 10), stamp); err != nil {
		return nil, err
	}
	decodedHash, _ := base64.RawURLEncoding.DecodeString(jtiHash)
	if _, err := tx.ExecContext(ctx, `INSERT INTO admin_sessions(jti_hash,issued_at,expires_at) VALUES (?,?,?)`, decodedHash, stamp, expires.Format(time.RFC3339Nano)); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &Session{Token: token, JTI: jti, ExpiresAt: expires}, nil
}

func (m *Manager) Authenticate(ctx context.Context, tokenText string) (*Principal, error) {
	parsed, err := jwt.ParseWithClaims(tokenText, &claims{}, func(token *jwt.Token) (any, error) {
		if token.Method != jwt.SigningMethodHS256 {
			return nil, ErrUnauthorized
		}
		return m.sessionKey, nil
	}, jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}), jwt.WithIssuer(issuer), jwt.WithAudience(audience), jwt.WithExpirationRequired(), jwt.WithIssuedAt(), jwt.WithTimeFunc(m.now))
	if err != nil || !parsed.Valid {
		return nil, ErrUnauthorized
	}
	parsedClaims, ok := parsed.Claims.(*claims)
	if !ok || parsedClaims.Subject != m.username || parsedClaims.ID == "" || parsedClaims.ExpiresAt == nil {
		return nil, ErrUnauthorized
	}
	hash := sha256.Sum256([]byte(parsedClaims.ID))
	var count int
	if err := m.db.QueryRowContext(ctx, `SELECT count(*) FROM admin_sessions
		WHERE jti_hash=? AND revoked_at IS NULL AND expires_at>?`, hash[:], m.now().UTC().Format(time.RFC3339Nano)).Scan(&count); err != nil || count != 1 {
		return nil, ErrUnauthorized
	}
	return &Principal{Username: m.username, JTI: parsedClaims.ID, Expires: parsedClaims.ExpiresAt.Time}, nil
}

func (m *Manager) Logout(ctx context.Context, jti string) error {
	hash := sha256.Sum256([]byte(jti))
	stamp := m.now().UTC().Format(time.RFC3339Nano)
	result, err := m.db.ExecContext(ctx, "UPDATE admin_sessions SET revoked_at=? WHERE jti_hash=? AND revoked_at IS NULL", stamp, hash[:])
	if err != nil {
		return err
	}
	rows, err := result.RowsAffected()
	if err != nil || rows != 1 {
		return ErrUnauthorized
	}
	_, _ = m.db.ExecContext(ctx, "DELETE FROM settings WHERE key=?", csrfSettingKey(hash[:]))
	return nil
}

func (m *Manager) BindCSRF(ctx context.Context, jti, token string) error {
	jtiHash := sha256.Sum256([]byte(jti))
	csrfHash := sha256.Sum256([]byte(token))
	_, err := m.db.ExecContext(ctx, `INSERT INTO settings(key,value,is_secret,key_id,updated_at)
		VALUES (?, ?, 0, NULL, ?) ON CONFLICT(key) DO UPDATE SET value=excluded.value, updated_at=excluded.updated_at`,
		csrfSettingKey(jtiHash[:]), fmt.Sprintf("%x", csrfHash[:]), m.now().UTC().Format(time.RFC3339Nano))
	return err
}

func (m *Manager) VerifyCSRF(ctx context.Context, jti, token string) bool {
	jtiHash := sha256.Sum256([]byte(jti))
	var stored string
	if err := m.db.QueryRowContext(ctx, "SELECT CAST(value AS TEXT) FROM settings WHERE key=?", csrfSettingKey(jtiHash[:])).Scan(&stored); err != nil {
		return false
	}
	got := sha256.Sum256([]byte(token))
	return subtle.ConstantTimeCompare([]byte(stored), []byte(fmt.Sprintf("%x", got[:]))) == 1
}

func csrfSettingKey(jtiHash []byte) string { return "internal.auth.csrf." + fmt.Sprintf("%x", jtiHash) }

func (m *Manager) newOpaque() (raw, encodedHash string, err error) {
	value := make([]byte, 32)
	if _, err := io.ReadFull(m.random, value); err != nil {
		return "", "", err
	}
	raw = base64.RawURLEncoding.EncodeToString(value)
	hash := sha256.Sum256([]byte(raw))
	return raw, base64.RawURLEncoding.EncodeToString(hash[:]), nil
}

func (m *Manager) pruneChallenges(now time.Time) {
	for key, entry := range m.challenges {
		if !now.Before(entry.expiresAt) {
			delete(m.challenges, key)
		}
	}
}
