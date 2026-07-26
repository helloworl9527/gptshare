package config

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

	"golang.org/x/crypto/bcrypt"
)

const minimumSecretBytes = 32

var phaseOneSecretEnvNames = []string{
	"CREDENTIAL_MASTER_KEYS",
	"CREDENTIAL_ACTIVE_KEY_ID",
	"JWT_SIGNING_KEY",
	"RATE_LIMIT_KEY",
	"ADMIN_TOTP_SECRET",
	"ALLOCATION_SERVICE_API_KEY",
}

type Config struct {
	Environment                  string
	ListenAddr                   string
	DBPath                       string
	AppOrigin                    string
	MonitorBaseURL               string
	MonitorAPIKey                string
	AdminUser                    string
	AdminPasswordHash            string
	AdminTOTPSecret              []byte
	SessionSigningKey            []byte
	CSRFSigningKey               []byte
	CredentialMasterKeys         map[string][]byte
	CredentialActiveKeyID        string
	CredentialMasterKeyMaterials [][]byte
}

func Load() (Config, error) {
	return LoadWithLookup(func(name string) (string, bool) { return lookupEnv(name) })
}

func LoadWithLookup(lookup func(string) (string, bool)) (Config, error) {
	if lookup == nil {
		return Config{}, errors.New("environment lookup is required")
	}
	get := func(name, fallback string) string {
		if value, ok := lookup(name); ok {
			return strings.TrimSpace(value)
		}
		return fallback
	}
	listenAddr, err := normalizeListenAddr(get("ALLOCATION_PORT", "9090"))
	if err != nil {
		return Config{}, err
	}
	monitorBaseURL, err := normalizeMonitorBaseURL(get("ALLOCATION_MONITOR_BASE_URL", "http://127.0.0.1:8080"))
	if err != nil {
		return Config{}, err
	}
	appOrigin, err := normalizeAppOrigin(get("ALLOCATION_APP_ORIGIN", "https://127.0.0.1:9090"))
	if err != nil {
		return Config{}, err
	}
	sessionKey, err := decodeStandardKey("ALLOCATION_SESSION_SIGNING_KEY", get("ALLOCATION_SESSION_SIGNING_KEY", ""))
	if err != nil {
		return Config{}, err
	}
	csrfKey, err := decodeStandardKey("ALLOCATION_CSRF_SIGNING_KEY", get("ALLOCATION_CSRF_SIGNING_KEY", ""))
	if err != nil {
		return Config{}, err
	}
	totp, err := decodeTOTP(get("ALLOCATION_ADMIN_TOTP_SECRET", ""))
	if err != nil {
		return Config{}, err
	}
	masterKeys, materials, err := parseAllocationKeyRing(get("ALLOCATION_CREDENTIAL_MASTER_KEYS", ""))
	if err != nil {
		return Config{}, err
	}
	cfg := Config{
		Environment:                  get("ALLOCATION_ENV", "development"),
		ListenAddr:                   listenAddr,
		DBPath:                       get("ALLOCATION_DB_PATH", filepath.Join("data", "allocation-service.db")),
		AppOrigin:                    appOrigin,
		MonitorBaseURL:               monitorBaseURL,
		MonitorAPIKey:                get("ALLOCATION_MONITOR_API_KEY", ""),
		AdminUser:                    get("ALLOCATION_ADMIN_USER", ""),
		AdminPasswordHash:            get("ALLOCATION_ADMIN_PASSWORD_HASH", ""),
		AdminTOTPSecret:              totp,
		SessionSigningKey:            sessionKey,
		CSRFSigningKey:               csrfKey,
		CredentialMasterKeys:         masterKeys,
		CredentialMasterKeyMaterials: materials,
		CredentialActiveKeyID:        get("ALLOCATION_CREDENTIAL_ACTIVE_KEY_ID", ""),
	}
	if err := cfg.Validate(lookup); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func (c Config) Validate(lookup func(string) (string, bool)) error {
	if c.Environment != "development" && c.Environment != "test" && c.Environment != "production" {
		return errors.New("ALLOCATION_ENV must be development, test, or production")
	}
	if !isLoopbackAddress(c.ListenAddr) {
		return errors.New("allocation service listen address must be loopback during implementation")
	}
	if c.DBPath == "" {
		return errors.New("ALLOCATION_DB_PATH is required")
	}
	if !validPlainSecret(c.MonitorAPIKey) {
		return errors.New("ALLOCATION_MONITOR_API_KEY must be an independently generated value of at least 32 bytes")
	}
	if c.AdminUser == "" {
		return errors.New("ALLOCATION_ADMIN_USER is required")
	}
	if !validPasswordHash(c.AdminPasswordHash) {
		return errors.New("ALLOCATION_ADMIN_PASSWORD_HASH must be a bcrypt hash with cost at least 12")
	}
	if len(c.AdminTOTPSecret) < 20 {
		return errors.New("ALLOCATION_ADMIN_TOTP_SECRET must decode to at least 20 bytes")
	}
	if len(c.SessionSigningKey) < minimumSecretBytes || len(c.CSRFSigningKey) < minimumSecretBytes {
		return errors.New("ALLOCATION_SESSION_SIGNING_KEY and ALLOCATION_CSRF_SIGNING_KEY must be at least 32 bytes")
	}
	if len(c.CredentialMasterKeys) == 0 {
		return errors.New("ALLOCATION_CREDENTIAL_MASTER_KEYS is required")
	}
	if _, ok := c.CredentialMasterKeys[c.CredentialActiveKeyID]; !ok {
		return errors.New("ALLOCATION_CREDENTIAL_ACTIVE_KEY_ID is not present in ALLOCATION_CREDENTIAL_MASTER_KEYS")
	}
	materials := [][]byte{[]byte(c.MonitorAPIKey), c.AdminTOTPSecret, c.SessionSigningKey, c.CSRFSigningKey}
	materials = append(materials, c.CredentialMasterKeyMaterials...)
	if err := rejectDuplicateMaterials("allocation secret", materials); err != nil {
		return err
	}
	if err := rejectPhaseOneMaterialReuse(lookup, materials); err != nil {
		return err
	}
	if err := rejectPhaseOneNames(lookup); err != nil {
		return err
	}
	return nil
}

func rejectPhaseOneNames(lookup func(string) (string, bool)) error {
	for _, name := range phaseOneSecretEnvNames {
		if value, ok := lookup(name); ok && strings.TrimSpace(value) != "" {
			return fmt.Errorf("phase one secret environment variable %s must not be present in allocation service configuration", name)
		}
	}
	return nil
}

func rejectDuplicateMaterials(label string, materials [][]byte) error {
	for i := range materials {
		for j := i + 1; j < len(materials); j++ {
			if len(materials[i]) == len(materials[j]) && subtle.ConstantTimeCompare(materials[i], materials[j]) == 1 {
				return fmt.Errorf("%s materials must be independently generated and distinct", label)
			}
		}
	}
	return nil
}

func rejectPhaseOneMaterialReuse(lookup func(string) (string, bool), allocationMaterials [][]byte) error {
	var phaseOneMaterials [][]byte
	if value, ok := lookup("CREDENTIAL_MASTER_KEYS"); ok {
		for _, material := range decodePhaseOneKeyRing(value) {
			phaseOneMaterials = append(phaseOneMaterials, material)
		}
	}
	for _, name := range []string{"JWT_SIGNING_KEY", "RATE_LIMIT_KEY"} {
		if value, ok := lookup(name); ok {
			if decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(value)); err == nil {
				phaseOneMaterials = append(phaseOneMaterials, decoded)
			}
		}
	}
	if value, ok := lookup("ADMIN_TOTP_SECRET"); ok {
		if decoded, err := decodeTOTP(value); err == nil {
			phaseOneMaterials = append(phaseOneMaterials, decoded)
		}
	}
	if value, ok := lookup("ALLOCATION_SERVICE_API_KEY"); ok {
		phaseOneMaterials = append(phaseOneMaterials, []byte(strings.TrimSpace(value)))
	}
	for _, allocation := range allocationMaterials {
		for _, phaseOne := range phaseOneMaterials {
			if len(allocation) == len(phaseOne) && subtle.ConstantTimeCompare(allocation, phaseOne) == 1 {
				return errors.New("allocation service secrets must not reuse phase one key material")
			}
		}
	}
	return nil
}

func parseAllocationKeyRing(value string) (map[string][]byte, [][]byte, error) {
	keys := make(map[string][]byte)
	var materials [][]byte
	if value == "" {
		return nil, nil, errors.New("ALLOCATION_CREDENTIAL_MASTER_KEYS is required")
	}
	for _, entry := range strings.Split(value, ",") {
		id, encoded, ok := strings.Cut(strings.TrimSpace(entry), ":")
		if !ok || !validKeyID(id) || encoded == "" {
			return nil, nil, errors.New("ALLOCATION_CREDENTIAL_MASTER_KEYS must use key_id:base64url_32bytes entries")
		}
		if isExampleValue(id) || isPhaseOneKeyID(id) {
			return nil, nil, errors.New("ALLOCATION_CREDENTIAL_MASTER_KEYS key ids must be allocation-specific and non-example")
		}
		if _, exists := keys[id]; exists {
			return nil, nil, fmt.Errorf("duplicate allocation credential key id %q", id)
		}
		decoded, err := base64.RawURLEncoding.DecodeString(encoded)
		if err != nil || len(decoded) != 32 {
			return nil, nil, errors.New("ALLOCATION_CREDENTIAL_MASTER_KEYS entries must decode from base64url to exactly 32 bytes")
		}
		keys[id] = decoded
		materials = append(materials, decoded)
	}
	return keys, materials, nil
}

func decodePhaseOneKeyRing(value string) [][]byte {
	var out [][]byte
	for _, entry := range strings.Split(value, ",") {
		_, encoded, ok := strings.Cut(strings.TrimSpace(entry), ":")
		if !ok {
			continue
		}
		if decoded, err := base64.StdEncoding.DecodeString(encoded); err == nil {
			out = append(out, decoded)
		}
	}
	return out
}

func decodeStandardKey(name, value string) ([]byte, error) {
	if isExampleValue(value) {
		return nil, fmt.Errorf("%s must not use an example value", name)
	}
	decoded, err := base64.StdEncoding.DecodeString(value)
	if err != nil || len(decoded) < minimumSecretBytes {
		return nil, fmt.Errorf("%s must be standard base64 encoding at least 32 bytes", name)
	}
	return decoded, nil
}

func decodeTOTP(value string) ([]byte, error) {
	if isExampleValue(value) {
		return nil, errors.New("ALLOCATION_ADMIN_TOTP_SECRET must not use an example value")
	}
	clean := strings.ToUpper(strings.ReplaceAll(strings.TrimSpace(value), " ", ""))
	decoded, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(clean)
	if err != nil || len(decoded) < 20 {
		return nil, errors.New("ALLOCATION_ADMIN_TOTP_SECRET must be unpadded base32 encoding at least 20 bytes")
	}
	return decoded, nil
}

func validPasswordHash(value string) bool {
	if isExampleValue(value) || len(value) != 60 || !(strings.HasPrefix(value, "$2a$") || strings.HasPrefix(value, "$2b$") || strings.HasPrefix(value, "$2y$")) {
		return false
	}
	cost, err := bcrypt.Cost([]byte(value))
	return err == nil && cost >= 12
}

func validPlainSecret(value string) bool {
	return len(value) >= minimumSecretBytes && strings.TrimSpace(value) == value && !strings.ContainsAny(value, " \t\r\n") && !isExampleValue(value)
}

func validKeyID(value string) bool {
	return value != "" && len(value) <= 64 && strings.TrimSpace(value) == value && !strings.ContainsAny(value, " \t\r\n:/,")
}

func isPhaseOneKeyID(value string) bool {
	switch strings.ToLower(value) {
	case "active", "old", "dev", "phase1", "credential", "credential_master_key":
		return true
	default:
		return false
	}
}

func isExampleValue(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", "change-me", "changeme", "default", "example", "sample", "password",
		"__replace_with_key_id_and_base64__", "__replace_with_independent_base64_key__",
		"__replace_with_allocation_monitor_api_key__", "__replace_with_allocation_admin_user__",
		"__replace_with_bcrypt_cost_12_or_higher__", "__replace_with_base32_secret__",
		"__replace_with_allocation_credential_keys__", "__replace_with_active_key_id__":
		return true
	default:
		return strings.Contains(strings.ToLower(value), "replace_with")
	}
}

func normalizeListenAddr(value string) (string, error) {
	if value == "" {
		value = "9090"
	}
	if port, err := strconv.Atoi(value); err == nil {
		if port < 0 || port > 65535 {
			return "", errors.New("ALLOCATION_PORT is outside 0..65535")
		}
		return net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), nil
	}
	host, portText, err := net.SplitHostPort(value)
	if err != nil {
		return "", errors.New("ALLOCATION_PORT must be a port number or host:port")
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port < 0 || port > 65535 {
		return "", errors.New("ALLOCATION_PORT contains an invalid port")
	}
	return net.JoinHostPort(host, strconv.Itoa(port)), nil
}

func normalizeMonitorBaseURL(value string) (string, error) {
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" || parsed.Path != "" || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.User != nil {
		return "", errors.New("ALLOCATION_MONITOR_BASE_URL must be an origin without path, query, fragment, or userinfo")
	}
	if parsed.Scheme != "https" && !(parsed.Scheme == "http" && isLoopbackHost(parsed.Hostname())) {
		return "", errors.New("ALLOCATION_MONITOR_BASE_URL must be https, except loopback http for local development")
	}
	return strings.TrimRight(parsed.String(), "/"), nil
}

func normalizeAppOrigin(value string) (string, error) {
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.Path != "" || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.User != nil {
		return "", errors.New("ALLOCATION_APP_ORIGIN must be an HTTPS origin without path, query, fragment, or userinfo")
	}
	if !isLoopbackHost(parsed.Hostname()) {
		return "", errors.New("ALLOCATION_APP_ORIGIN must use a loopback host during implementation")
	}
	return strings.TrimRight(parsed.String(), "/"), nil
}

func isLoopbackAddress(address string) bool {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return false
	}
	return isLoopbackHost(host)
}

func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
