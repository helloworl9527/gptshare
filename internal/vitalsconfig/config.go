package vitalsconfig

import (
	"crypto/subtle"
	"encoding/base32"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"
)

const minimumSecretBytes = 32

type Config struct {
	ListenAddr                   string
	MonitorDBPath                string
	MonitorMigrationsDir         string
	MonitorCredentialKeys        map[string][]byte
	MonitorCredentialActiveID    string
	AllocationDBPath             string
	AllocationCredentialKeys     map[string][]byte
	AllocationCredentialActiveID string
	AdminUser                    string
	AdminPasswordHash            string
	AdminTOTPSecret              []byte
	JWTSigningKey                []byte
	RateLimitKey                 []byte
	AppOrigin                    string
	TrustLoopbackProxy           bool
	CompatHTTP                   CompatHTTPConfig
}

type CompatHTTPConfig struct {
	Enabled      bool
	APIKey       string
	Consumer     string
	ExpiresAt    time.Time
	ShutdownTask string
}

func Load(lookup func(string) (string, bool)) (Config, error) {
	if lookup == nil {
		return Config{}, errors.New("environment lookup is required")
	}
	get := func(name, fallback string) string {
		if value, ok := lookup(name); ok {
			return strings.TrimSpace(value)
		}
		return fallback
	}
	listenAddr, err := normalizeListenAddr(get("VITALS_PORT", "8080"))
	if err != nil {
		return Config{}, err
	}
	monitorKeys, err := parseKeyring("CREDENTIAL_MASTER_KEYS", get("CREDENTIAL_MASTER_KEYS", ""), base64.StdEncoding)
	if err != nil {
		return Config{}, err
	}
	allocationValue := get("ALLOCATION_CREDENTIAL_MASTER_KEYS", "")
	if allocationValue == "" {
		return Config{}, errors.New("ALLOCATION_CREDENTIAL_MASTER_KEYS is required; allocation credentials must not use CREDENTIAL_MASTER_KEYS")
	}
	allocationKeys, err := parseKeyring("ALLOCATION_CREDENTIAL_MASTER_KEYS", allocationValue, base64.RawURLEncoding)
	if err != nil {
		return Config{}, err
	}
	monitorActive := get("CREDENTIAL_ACTIVE_KEY_ID", "")
	if _, ok := monitorKeys[monitorActive]; !ok {
		return Config{}, errors.New("CREDENTIAL_ACTIVE_KEY_ID is not present in CREDENTIAL_MASTER_KEYS")
	}
	allocationActive := get("ALLOCATION_CREDENTIAL_ACTIVE_KEY_ID", "")
	if _, ok := allocationKeys[allocationActive]; !ok {
		return Config{}, errors.New("ALLOCATION_CREDENTIAL_ACTIVE_KEY_ID is not present in ALLOCATION_CREDENTIAL_MASTER_KEYS")
	}
	if err := rejectCrossKeyringReuse(monitorKeys, allocationKeys); err != nil {
		return Config{}, err
	}
	jwtKey, err := decodeStandardKey("JWT_SIGNING_KEY", get("JWT_SIGNING_KEY", ""))
	if err != nil {
		return Config{}, err
	}
	rateKey, err := decodeStandardKey("RATE_LIMIT_KEY", get("RATE_LIMIT_KEY", ""))
	if err != nil {
		return Config{}, err
	}
	totpSecret, err := decodeTOTP(get("ADMIN_TOTP_SECRET", ""))
	if err != nil {
		return Config{}, err
	}
	adminUser := get("ADMIN_USER", "")
	if adminUser == "" || isExample(adminUser) {
		return Config{}, errors.New("ADMIN_USER is required and must not be an example value")
	}
	passwordHash := get("ADMIN_PASSWORD_HASH", "")
	if !validPasswordHash(passwordHash) {
		return Config{}, errors.New("ADMIN_PASSWORD_HASH must be a bcrypt hash with cost at least 12")
	}
	appOrigin := get("APP_ORIGIN", "https://"+listenAddr)
	if err := validateOrigin(appOrigin); err != nil {
		return Config{}, err
	}
	trustProxy, err := strconv.ParseBool(get("TRUST_LOOPBACK_PROXY", "false"))
	if err != nil {
		return Config{}, errors.New("TRUST_LOOPBACK_PROXY must be true or false")
	}
	if err := rejectAuthKeyReuse(monitorKeys, allocationKeys, jwtKey, rateKey, totpSecret); err != nil {
		return Config{}, err
	}
	compat, err := loadCompatHTTP(get)
	if err != nil {
		return Config{}, err
	}
	cfg := Config{
		ListenAddr:                   listenAddr,
		MonitorDBPath:                get("MONITOR_DB_PATH", filepath.Join("data", "monitor.db")),
		MonitorMigrationsDir:         get("MONITOR_MIGRATIONS_DIR", "migrations"),
		MonitorCredentialKeys:        monitorKeys,
		MonitorCredentialActiveID:    monitorActive,
		AllocationDBPath:             get("ALLOCATION_DB_PATH", filepath.Join("data", "allocation.db")),
		AllocationCredentialKeys:     allocationKeys,
		AllocationCredentialActiveID: allocationActive,
		AdminUser:                    adminUser,
		AdminPasswordHash:            passwordHash,
		AdminTOTPSecret:              totpSecret,
		JWTSigningKey:                jwtKey,
		RateLimitKey:                 rateKey,
		AppOrigin:                    appOrigin,
		TrustLoopbackProxy:           trustProxy,
		CompatHTTP:                   compat,
	}
	if cfg.MonitorDBPath == "" || cfg.AllocationDBPath == "" || cfg.MonitorMigrationsDir == "" {
		return Config{}, errors.New("both database paths and the monitor migrations directory are required")
	}
	if samePath(cfg.MonitorDBPath, cfg.AllocationDBPath) {
		return Config{}, errors.New("monitor and allocation databases must use different files")
	}
	return cfg, nil
}

func loadCompatHTTP(get func(string, string) string) (CompatHTTPConfig, error) {
	enabled, err := strconv.ParseBool(get("VITALS_MONITOR_COMPAT_HTTP_ENABLED", "false"))
	if err != nil {
		return CompatHTTPConfig{}, errors.New("VITALS_MONITOR_COMPAT_HTTP_ENABLED must be true or false")
	}
	result := CompatHTTPConfig{Enabled: enabled}
	if !enabled {
		return result, nil
	}
	result.APIKey = get("ALLOCATION_SERVICE_API_KEY", "")
	result.Consumer = get("VITALS_MONITOR_COMPAT_CONSUMER", "")
	result.ShutdownTask = get("VITALS_MONITOR_COMPAT_SHUTDOWN_TASK", "")
	expires := get("VITALS_MONITOR_COMPAT_EXPIRES_AT", "")
	if !validPlainSecret(result.APIKey) {
		return CompatHTTPConfig{}, errors.New("ALLOCATION_SERVICE_API_KEY must be an independently generated value of at least 32 bytes when compatibility HTTP is enabled")
	}
	if result.Consumer == "" || result.ShutdownTask == "" || isExample(result.Consumer) || isExample(result.ShutdownTask) {
		return CompatHTTPConfig{}, errors.New("compatibility HTTP requires a non-example consumer and shutdown task")
	}
	result.ExpiresAt, err = time.Parse(time.RFC3339, expires)
	if err != nil || !result.ExpiresAt.After(time.Now().UTC()) {
		return CompatHTTPConfig{}, errors.New("compatibility HTTP expiry must be a future RFC3339 timestamp")
	}
	return result, nil
}

func parseKeyring(name, value string, encoding *base64.Encoding) (map[string][]byte, error) {
	if value == "" {
		return nil, fmt.Errorf("%s is required", name)
	}
	keys := make(map[string][]byte)
	for _, entry := range strings.Split(value, ",") {
		id, encoded, ok := strings.Cut(strings.TrimSpace(entry), ":")
		if !ok || id == "" || encoded == "" || len(id) > 64 || strings.ContainsAny(id, " \t\r\n:/,") || isExample(id) {
			return nil, fmt.Errorf("%s must use non-example key_id:encoded_32bytes entries", name)
		}
		if _, exists := keys[id]; exists {
			return nil, fmt.Errorf("%s contains duplicate key id %q", name, id)
		}
		decoded, err := encoding.DecodeString(encoded)
		if err != nil || len(decoded) != minimumSecretBytes || isExample(string(decoded)) {
			return nil, fmt.Errorf("%s entries must decode to exactly 32 non-example bytes", name)
		}
		keys[id] = decoded
	}
	return keys, nil
}

func rejectCrossKeyringReuse(monitor, allocation map[string][]byte) error {
	for _, monitorKey := range monitor {
		for _, allocationKey := range allocation {
			if len(monitorKey) == len(allocationKey) && subtle.ConstantTimeCompare(monitorKey, allocationKey) == 1 {
				return errors.New("monitor and allocation credential key material must be independently generated and distinct")
			}
		}
	}
	return nil
}

func rejectAuthKeyReuse(monitor, allocation map[string][]byte, authKeys ...[]byte) error {
	materials := make([][]byte, 0, len(monitor)+len(allocation)+len(authKeys))
	for _, key := range monitor {
		materials = append(materials, key)
	}
	for _, key := range allocation {
		materials = append(materials, key)
	}
	materials = append(materials, authKeys...)
	for i := range materials {
		for j := i + 1; j < len(materials); j++ {
			if len(materials[i]) == len(materials[j]) && subtle.ConstantTimeCompare(materials[i], materials[j]) == 1 {
				return errors.New("unified auth and module data encryption keys must be independently generated and distinct")
			}
		}
	}
	return nil
}

func decodeStandardKey(name, value string) ([]byte, error) {
	decoded, err := base64.StdEncoding.DecodeString(value)
	if err != nil || len(decoded) < minimumSecretBytes || isExample(string(decoded)) {
		return nil, fmt.Errorf("%s must be standard base64 encoding of at least 32 non-example bytes", name)
	}
	return decoded, nil
}

func decodeTOTP(value string) ([]byte, error) {
	clean := strings.ToUpper(strings.ReplaceAll(value, " ", ""))
	decoded, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(clean)
	if err != nil || len(decoded) < 20 || isExample(string(decoded)) {
		return nil, errors.New("ADMIN_TOTP_SECRET must be unpadded base32 encoding of at least 20 non-example bytes")
	}
	return decoded, nil
}

func validPasswordHash(value string) bool {
	if len(value) != 60 || !(strings.HasPrefix(value, "$2a$") || strings.HasPrefix(value, "$2b$") || strings.HasPrefix(value, "$2y$")) {
		return false
	}
	cost, err := bcrypt.Cost([]byte(value))
	return err == nil && cost >= 12
}

func validateOrigin(value string) error {
	origin, err := url.Parse(value)
	if err != nil || origin.Scheme != "https" || origin.Host == "" || origin.Path != "" || origin.RawQuery != "" || origin.Fragment != "" || origin.User != nil {
		return errors.New("APP_ORIGIN must be an HTTPS origin without credentials, path, query, or fragment")
	}
	host := origin.Hostname()
	ip := net.ParseIP(host)
	if !strings.EqualFold(host, "localhost") && (ip == nil || !ip.IsLoopback()) {
		return errors.New("APP_ORIGIN must use a loopback host during implementation")
	}
	return nil
}

func normalizeListenAddr(value string) (string, error) {
	if port, err := strconv.Atoi(value); err == nil {
		if port < 0 || port > 65535 {
			return "", errors.New("VITALS_PORT is outside 0..65535")
		}
		return net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), nil
	}
	host, portText, err := net.SplitHostPort(value)
	if err != nil {
		return "", errors.New("VITALS_PORT must be a port number or host:port")
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port < 0 || port > 65535 {
		return "", errors.New("VITALS_PORT contains an invalid port")
	}
	ip := net.ParseIP(host)
	if !strings.EqualFold(host, "localhost") && (ip == nil || !ip.IsLoopback()) {
		return "", errors.New("vitals listen address must be loopback during implementation")
	}
	return net.JoinHostPort(host, strconv.Itoa(port)), nil
}

func samePath(left, right string) bool {
	a, errA := filepath.Abs(left)
	b, errB := filepath.Abs(right)
	if errA != nil || errB != nil {
		return false
	}
	if resolved, err := filepath.EvalSymlinks(a); err == nil {
		a = resolved
	} else if resolvedParent, parentErr := filepath.EvalSymlinks(filepath.Dir(a)); parentErr == nil {
		a = filepath.Join(resolvedParent, filepath.Base(a))
	}
	if resolved, err := filepath.EvalSymlinks(b); err == nil {
		b = resolved
	} else if resolvedParent, parentErr := filepath.EvalSymlinks(filepath.Dir(b)); parentErr == nil {
		b = filepath.Join(resolvedParent, filepath.Base(b))
	}
	return filepath.Clean(a) == filepath.Clean(b)
}

func validPlainSecret(value string) bool {
	return len(value) >= minimumSecretBytes && value == strings.TrimSpace(value) && !strings.ContainsAny(value, " \t\r\n") && !isExample(value)
}

func isExample(value string) bool {
	lower := strings.ToLower(strings.TrimSpace(value))
	switch lower {
	case "", "change-me", "changeme", "default", "example", "sample", "password", "dev", "active", "old":
		return true
	default:
		return strings.Contains(lower, "replace_with") || strings.Contains(lower, "example")
	}
}
