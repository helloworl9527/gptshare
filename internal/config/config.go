package config

import (
	"crypto/subtle"
	"encoding/base32"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"golang.org/x/crypto/bcrypt"
)

const minimumKeyBytes = 32

type Config struct {
	Environment             string
	ListenAddr              string
	DBPath                  string
	MigrationsDir           string
	CredentialMasterKeys    map[string][]byte
	CredentialActiveKeyID   string
	JWTSigningKey           []byte
	RateLimitKey            []byte
	AllocationServiceAPIKey string
	AdminUser               string
	AdminPasswordHash       string
	AdminTOTPSecret         []byte
	AppOrigin               string
	TrustLoopbackProxy      bool
	DevTLSCert              string
	DevTLSKey               string
}

func Load() (Config, error) {
	return load(os.LookupEnv)
}

func load(lookup func(string) (string, bool)) (Config, error) {
	get := func(name, fallback string) string {
		if value, ok := lookup(name); ok {
			return strings.TrimSpace(value)
		}
		return fallback
	}

	listenAddr, err := normalizeListenAddr(get("PORT", "8080"))
	if err != nil {
		return Config{}, err
	}
	masterKeys, err := parseKeyRing(get("CREDENTIAL_MASTER_KEYS", ""))
	if err != nil {
		return Config{}, err
	}
	jwtKey, err := decodeKey("JWT_SIGNING_KEY", get("JWT_SIGNING_KEY", ""))
	if err != nil {
		return Config{}, err
	}
	rateKey, err := decodeKey("RATE_LIMIT_KEY", get("RATE_LIMIT_KEY", ""))
	if err != nil {
		return Config{}, err
	}
	totp, err := decodeTOTP(get("ADMIN_TOTP_SECRET", ""))
	if err != nil {
		return Config{}, err
	}
	trustProxy, err := strconv.ParseBool(get("TRUST_LOOPBACK_PROXY", "false"))
	if err != nil {
		return Config{}, errors.New("TRUST_LOOPBACK_PROXY must be true or false")
	}
	appOrigin := get("APP_ORIGIN", "https://"+listenAddr)

	cfg := Config{
		Environment:             get("APP_ENV", "development"),
		ListenAddr:              listenAddr,
		DBPath:                  get("DB_PATH", filepath.Join("data", "chatgpt-monitor.db")),
		MigrationsDir:           get("MIGRATIONS_DIR", "migrations"),
		CredentialMasterKeys:    masterKeys,
		CredentialActiveKeyID:   get("CREDENTIAL_ACTIVE_KEY_ID", ""),
		JWTSigningKey:           jwtKey,
		RateLimitKey:            rateKey,
		AllocationServiceAPIKey: get("ALLOCATION_SERVICE_API_KEY", ""),
		AdminUser:               get("ADMIN_USER", ""),
		AdminPasswordHash:       get("ADMIN_PASSWORD_HASH", ""),
		AdminTOTPSecret:         totp,
		AppOrigin:               appOrigin,
		TrustLoopbackProxy:      trustProxy,
		DevTLSCert:              get("DEV_TLS_CERT", ""),
		DevTLSKey:               get("DEV_TLS_KEY", ""),
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func (c Config) Validate() error {
	if c.Environment != "development" && c.Environment != "test" && c.Environment != "production" {
		return errors.New("APP_ENV must be development, test, or production")
	}
	if !isLoopbackAddress(c.ListenAddr) {
		return errors.New("server listen address must be loopback during implementation")
	}
	origin, err := url.Parse(c.AppOrigin)
	if err != nil || origin.Scheme != "https" || origin.Host == "" || origin.Path != "" || origin.RawQuery != "" || origin.Fragment != "" {
		return errors.New("APP_ORIGIN must be an HTTPS origin without path, query, or fragment")
	}
	if origin.User != nil || !isLoopbackHost(origin.Hostname()) {
		return errors.New("APP_ORIGIN must use a loopback host during implementation")
	}
	if (c.DevTLSCert == "") != (c.DevTLSKey == "") {
		return errors.New("DEV_TLS_CERT and DEV_TLS_KEY must be configured together")
	}
	if c.Environment == "production" && c.DevTLSCert != "" {
		return errors.New("DEV TLS is forbidden in production")
	}
	if c.DBPath == "" || c.MigrationsDir == "" {
		return errors.New("DB_PATH and MIGRATIONS_DIR are required")
	}
	if c.AdminUser == "" {
		return errors.New("ADMIN_USER is required")
	}
	if !validPasswordHash(c.AdminPasswordHash) {
		return errors.New("ADMIN_PASSWORD_HASH must be a bcrypt hash with cost at least 12")
	}
	if len(c.AdminTOTPSecret) < 20 {
		return errors.New("ADMIN_TOTP_SECRET must decode to at least 20 bytes")
	}
	if len(c.CredentialMasterKeys) == 0 {
		return errors.New("CREDENTIAL_MASTER_KEYS is required")
	}
	if _, ok := c.CredentialMasterKeys[c.CredentialActiveKeyID]; !ok {
		return errors.New("CREDENTIAL_ACTIVE_KEY_ID is not present in CREDENTIAL_MASTER_KEYS")
	}
	if len(c.JWTSigningKey) < minimumKeyBytes || len(c.RateLimitKey) < minimumKeyBytes {
		return errors.New("JWT_SIGNING_KEY and RATE_LIMIT_KEY must be at least 32 bytes")
	}
	if !validAllocationServiceAPIKey(c.AllocationServiceAPIKey) {
		return errors.New("ALLOCATION_SERVICE_API_KEY must be an independently generated value of at least 32 bytes")
	}

	materials := [][]byte{c.JWTSigningKey, c.RateLimitKey, c.AdminTOTPSecret, []byte(c.AllocationServiceAPIKey)}
	for _, key := range c.CredentialMasterKeys {
		materials = append(materials, key)
	}
	for i := range materials {
		for j := i + 1; j < len(materials); j++ {
			if len(materials[i]) == len(materials[j]) && subtle.ConstantTimeCompare(materials[i], materials[j]) == 1 {
				return errors.New("security keys must be independently generated and distinct")
			}
		}
	}
	return nil
}

func validAllocationServiceAPIKey(value string) bool {
	if len(value) < minimumKeyBytes || strings.TrimSpace(value) != value || strings.ContainsAny(value, " \t\r\n") {
		return false
	}
	switch strings.ToLower(value) {
	case "change-me", "changeme", "default", "example", "sample", "password", "__replace_with_allocation_service_api_key__":
		return false
	}
	return true
}

func normalizeListenAddr(value string) (string, error) {
	if value == "" {
		value = "8080"
	}
	if port, err := strconv.Atoi(value); err == nil {
		if port < 0 || port > 65535 {
			return "", errors.New("PORT is outside 0..65535")
		}
		return net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), nil
	}
	host, portText, err := net.SplitHostPort(value)
	if err != nil {
		return "", errors.New("PORT must be a port number or host:port")
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port < 0 || port > 65535 {
		return "", errors.New("PORT contains an invalid port")
	}
	return net.JoinHostPort(host, strconv.Itoa(port)), nil
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

func parseKeyRing(value string) (map[string][]byte, error) {
	keys := make(map[string][]byte)
	if value == "" {
		return keys, errors.New("CREDENTIAL_MASTER_KEYS is required")
	}
	for _, entry := range strings.Split(value, ",") {
		id, encoded, ok := strings.Cut(strings.TrimSpace(entry), ":")
		if !ok || id == "" || encoded == "" || strings.ContainsAny(id, " \t\r\n") {
			return nil, errors.New("CREDENTIAL_MASTER_KEYS must use id:base64 entries")
		}
		if _, exists := keys[id]; exists {
			return nil, fmt.Errorf("duplicate credential key id %q", id)
		}
		decoded, err := decodeKey("CREDENTIAL_MASTER_KEYS", encoded)
		if err != nil {
			return nil, err
		}
		if len(decoded) != 32 {
			return nil, errors.New("CREDENTIAL_MASTER_KEYS entries must decode to exactly 32 bytes for AES-256-GCM")
		}
		keys[id] = decoded
	}
	return keys, nil
}

func decodeKey(name, value string) ([]byte, error) {
	decoded, err := base64.StdEncoding.DecodeString(value)
	if err != nil || len(decoded) < minimumKeyBytes {
		return nil, fmt.Errorf("%s must be standard base64 encoding at least 32 bytes", name)
	}
	return decoded, nil
}

func decodeTOTP(value string) ([]byte, error) {
	clean := strings.ToUpper(strings.ReplaceAll(value, " ", ""))
	decoded, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(clean)
	if err != nil || len(decoded) < 20 {
		return nil, errors.New("ADMIN_TOTP_SECRET must be unpadded base32 encoding at least 20 bytes")
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
