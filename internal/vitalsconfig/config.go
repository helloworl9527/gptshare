package vitalsconfig

import (
	"crypto/subtle"
	"encoding/base32"
	"encoding/base64"
	"encoding/hex"
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

const (
	defaultAdminSessionTTL = 30 * 24 * time.Hour
	minAdminSessionTTL     = time.Hour
	maxAdminSessionTTL     = 90 * 24 * time.Hour
)

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
	AdminSessionTTL              time.Duration
	JWTSigningKey                []byte
	RateLimitKey                 []byte
	AppOrigin                    string
	TrustLoopbackProxy           bool
	CompatHTTP                   CompatHTTPConfig
	RefreshScanEvery             time.Duration
	RefreshLead                  time.Duration
	RefreshMaxLatency            time.Duration
	UserCenter                   UserCenterConfig
}

// UserCenterConfig 是统一用户中心绑定的配置。两个密钥都留空 = 功能关闭（匿名兑换不受影响）；
// 只配一个、或任一不是独立生成的强随机值，都拒绝启动。
type UserCenterConfig struct {
	Enabled bool
	// BindTicketKey 是 X-Bind-Ticket 的 HMAC-SHA256 密钥（32 字节）。
	BindTicketKey []byte
	// InternalToken 是 /internal/uc/ 接口 X-UC-Token 的共享密钥（64 位十六进制原文）。
	InternalToken string
	// DetailOrigin 是 detail-link 返回的公开兑换页 origin，例如 https://gpt.baochai.cc。
	DetailOrigin string
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
	adminSessionTTL, err := time.ParseDuration(get("ADMIN_SESSION_TTL", defaultAdminSessionTTL.String()))
	if err != nil || adminSessionTTL < minAdminSessionTTL || adminSessionTTL > maxAdminSessionTTL {
		return Config{}, errors.New("ADMIN_SESSION_TTL must be a duration between 1h and 2160h")
	}
	appOrigin := get("APP_ORIGIN", "https://"+listenAddr)
	allowPublicOrigin, err := strconv.ParseBool(get("VITALS_ALLOW_PUBLIC_APP_ORIGIN", "false"))
	if err != nil {
		return Config{}, errors.New("VITALS_ALLOW_PUBLIC_APP_ORIGIN must be true or false")
	}
	if err := validateOrigin(appOrigin, allowPublicOrigin); err != nil {
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
	refreshScanEvery, err := parseBoundedDuration(get, "VITALS_MONITOR_REFRESH_SCAN_EVERY", time.Hour, time.Minute, 24*time.Hour)
	if err != nil {
		return Config{}, err
	}
	refreshLead, err := parseBoundedDuration(get, "VITALS_MONITOR_REFRESH_LEAD", 24*time.Hour, time.Minute, 30*24*time.Hour)
	if err != nil {
		return Config{}, err
	}
	refreshMaxLatency, err := parseBoundedDuration(get, "VITALS_MONITOR_REFRESH_MAX_LATENCY", 5*time.Minute, time.Second, 30*time.Minute)
	if err != nil {
		return Config{}, err
	}
	userCenter, err := loadUserCenter(get, compat.APIKey, monitorKeys, allocationKeys, jwtKey, rateKey, totpSecret)
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
		AdminSessionTTL:              adminSessionTTL,
		JWTSigningKey:                jwtKey,
		RateLimitKey:                 rateKey,
		AppOrigin:                    appOrigin,
		TrustLoopbackProxy:           trustProxy,
		CompatHTTP:                   compat,
		RefreshScanEvery:             refreshScanEvery,
		RefreshLead:                  refreshLead,
		RefreshMaxLatency:            refreshMaxLatency,
		UserCenter:                   userCenter,
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

func loadUserCenter(get func(string, string) string, compatAPIKey string, monitor, allocation map[string][]byte, authKeys ...[]byte) (UserCenterConfig, error) {
	ticketHex := get("UC_BIND_TICKET_KEY", "")
	tokenHex := get("UC_INTERNAL_TOKEN", "")
	originValue := get("UC_DETAIL_ORIGIN", "")
	if ticketHex == "" && tokenHex == "" {
		if originValue != "" {
			return UserCenterConfig{}, errors.New("UC_DETAIL_ORIGIN requires UC_BIND_TICKET_KEY and UC_INTERNAL_TOKEN")
		}
		return UserCenterConfig{}, nil
	}
	if ticketHex == "" || tokenHex == "" {
		return UserCenterConfig{}, errors.New("UC_BIND_TICKET_KEY and UC_INTERNAL_TOKEN must be configured together")
	}
	ticketKey, err := decodeUserCenterSecret("UC_BIND_TICKET_KEY", ticketHex)
	if err != nil {
		return UserCenterConfig{}, err
	}
	tokenKey, err := decodeUserCenterSecret("UC_INTERNAL_TOKEN", tokenHex)
	if err != nil {
		return UserCenterConfig{}, err
	}
	materials := make([][]byte, 0, len(monitor)+len(allocation)+len(authKeys))
	for _, key := range monitor {
		materials = append(materials, key)
	}
	for _, key := range allocation {
		materials = append(materials, key)
	}
	materials = append(materials, authKeys...)
	if compatAPIKey != "" {
		materials = append(materials, []byte(compatAPIKey))
	}
	if subtle.ConstantTimeCompare(ticketKey, tokenKey) == 1 {
		return UserCenterConfig{}, errors.New("UC_BIND_TICKET_KEY and UC_INTERNAL_TOKEN must be independently generated and distinct")
	}
	for _, candidate := range [][]byte{ticketKey, tokenKey, []byte(ticketHex), []byte(tokenHex)} {
		for _, material := range materials {
			if secretOverlaps(candidate, material) {
				return UserCenterConfig{}, errors.New("user center secrets must not reuse monitor, allocation, or unified auth key material")
			}
		}
	}
	if err := validateDetailOrigin(originValue); err != nil {
		return UserCenterConfig{}, err
	}
	return UserCenterConfig{Enabled: true, BindTicketKey: ticketKey, InternalToken: tokenHex, DetailOrigin: originValue}, nil
}

// decodeUserCenterSecret 要求 64 位十六进制（32 字节随机，openssl rand -hex 32），
// 并拒绝示例值和明显的弱值（全同字节、短周期重复等）。
func decodeUserCenterSecret(name, value string) ([]byte, error) {
	decoded, err := hex.DecodeString(value)
	if err != nil || len(decoded) != minimumSecretBytes || isExample(value) || isExample(string(decoded)) || weakSecret(decoded) {
		return nil, fmt.Errorf("%s must be 64 hex characters of independently generated random bytes (openssl rand -hex 32)", name)
	}
	return decoded, nil
}

// weakSecret 拒绝不同字节值过少的材料。32 个真随机字节的不同值个数几乎总在 25 以上，
// 低于 16 说明是重复模式（如 0123…ef 循环、全 0、同一字符重复）。
func weakSecret(value []byte) bool {
	seen := make(map[byte]struct{}, len(value))
	for _, b := range value {
		seen[b] = struct{}{}
	}
	return len(seen) < 16
}

func secretOverlaps(left, right []byte) bool {
	if len(left) == 0 || len(right) == 0 {
		return false
	}
	if len(left) == len(right) && subtle.ConstantTimeCompare(left, right) == 1 {
		return true
	}
	// 同一份随机串换个编码填进别的变量也算复用：比较两边的十六进制 / base64 表示。
	for _, encoded := range []string{hex.EncodeToString(right), base64.StdEncoding.EncodeToString(right), base64.RawURLEncoding.EncodeToString(right)} {
		if strings.EqualFold(string(left), encoded) {
			return true
		}
	}
	for _, encoded := range []string{hex.EncodeToString(left), base64.StdEncoding.EncodeToString(left), base64.RawURLEncoding.EncodeToString(left)} {
		if strings.EqualFold(string(right), encoded) {
			return true
		}
	}
	return false
}

func validateDetailOrigin(value string) error {
	origin, err := url.Parse(value)
	if value == "" || err != nil || origin.Scheme != "https" || origin.Host == "" || origin.Path != "" || origin.RawQuery != "" || origin.Fragment != "" || origin.User != nil {
		return errors.New("UC_DETAIL_ORIGIN must be the public HTTPS origin of the redeem page (for example https://gpt.example.com) when the user center binding is enabled")
	}
	return nil
}

func parseBoundedDuration(get func(string, string) string, name string, fallback, min, max time.Duration) (time.Duration, error) {
	parsed, err := time.ParseDuration(get(name, fallback.String()))
	if err != nil || parsed < min || parsed > max {
		return 0, fmt.Errorf("%s must be a duration between %s and %s", name, min, max)
	}
	return parsed, nil
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

func validateOrigin(value string, allowPublic bool) error {
	origin, err := url.Parse(value)
	if err != nil || origin.Scheme != "https" || origin.Host == "" || origin.Path != "" || origin.RawQuery != "" || origin.Fragment != "" || origin.User != nil {
		return errors.New("APP_ORIGIN must be an HTTPS origin without credentials, path, query, or fragment")
	}
	host := origin.Hostname()
	ip := net.ParseIP(host)
	if !allowPublic && !strings.EqualFold(host, "localhost") && (ip == nil || !ip.IsLoopback()) {
		return errors.New("APP_ORIGIN must use a loopback host unless VITALS_ALLOW_PUBLIC_APP_ORIGIN is true")
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
