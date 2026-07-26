package auth

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"time"
)

const (
	limitWindow  = 10 * time.Minute
	retention    = 24 * time.Hour
	accountLimit = 5
	ipLimit      = 5
	globalLimit  = 30
	defaultRetry = 10 * time.Minute
)

type limiter struct {
	db  *sql.DB
	key []byte
}

func newLimiter(db *sql.DB, key []byte) *limiter {
	return &limiter{db: db, key: append([]byte(nil), key...)}
}

func (l *limiter) check(ctx context.Context, username, clientIP string, now time.Time) (bool, time.Duration, error) {
	return l.counts(ctx, l.db, username, clientIP, now)
}

func (l *limiter) record(ctx context.Context, username, clientIP, result string, now time.Time) (bool, time.Duration, error) {
	tx, err := l.db.BeginTx(ctx, nil)
	if err != nil {
		return false, 0, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, "DELETE FROM admin_login_attempts WHERE expires_at<=?", now.Format(time.RFC3339Nano)); err != nil {
		return false, 0, err
	}
	userDigest := l.digest("username", username)
	ipDigest := l.ipDigest(clientIP)
	if _, err := tx.ExecContext(ctx, `INSERT INTO admin_login_attempts
		(username_hmac,client_ip_hmac,result,attempted_at,expires_at) VALUES (?,?,?,?,?)`,
		userDigest[:], ipDigest[:], result,
		now.Format(time.RFC3339Nano), now.Add(retention).Format(time.RFC3339Nano)); err != nil {
		return false, 0, err
	}
	limited, retry, err := l.counts(ctx, tx, username, clientIP, now)
	if err != nil {
		return false, 0, err
	}
	if limited && result == "failure" {
		_, _ = tx.ExecContext(ctx, "UPDATE admin_login_attempts SET result='locked' WHERE id=last_insert_rowid()")
	}
	if err := tx.Commit(); err != nil {
		return false, 0, err
	}
	return limited, retry, nil
}

type queryer interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func (l *limiter) counts(ctx context.Context, q queryer, username, clientIP string, now time.Time) (bool, time.Duration, error) {
	since := now.Add(-limitWindow).Format(time.RFC3339Nano)
	userDigest := l.digest("username", username)
	ipDigest := l.ipDigest(clientIP)
	var account, ip, global int
	err := q.QueryRowContext(ctx, `SELECT
		COALESCE(sum(CASE WHEN username_hmac=? THEN 1 ELSE 0 END),0),
		COALESCE(sum(CASE WHEN client_ip_hmac=? THEN 1 ELSE 0 END),0),
		count(*)
		FROM admin_login_attempts WHERE attempted_at>=? AND result IN ('failure','locked')`,
		userDigest[:], ipDigest[:], since).Scan(&account, &ip, &global)
	if err != nil {
		return false, 0, err
	}
	if account >= accountLimit || ip >= ipLimit || global >= globalLimit {
		return true, defaultRetry, nil
	}
	return false, 0, nil
}

func (l *limiter) digest(domain, value string) [32]byte {
	mac := hmac.New(sha256.New, l.key)
	mac.Write([]byte(domain))
	mac.Write([]byte{0})
	mac.Write([]byte(value))
	var result [32]byte
	copy(result[:], mac.Sum(nil))
	return result
}

func (l *limiter) ipDigest(ip string) [32]byte { return l.digest("client-ip", ip) }
