package chatgpt

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/mail"
	"strings"
	"unicode"
	"unicode/utf8"
)

const profileClaimNamespace = "https://api.openai.com/profile"

type emailClaim struct {
	email     string
	accountID string
}

// NormalizeEmail returns a display-safe mailbox string. It preserves the
// local-part and lowercases only the domain.
func NormalizeEmail(value string) (string, bool) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" || trimmed != value {
		return "", false
	}
	if len(trimmed) < 3 || len(trimmed) > 254 || !utf8.ValidString(trimmed) {
		return "", false
	}
	for _, r := range trimmed {
		if unicode.IsControl(r) || unicode.IsSpace(r) {
			return "", false
		}
	}
	if strings.ContainsAny(trimmed, "<>") || strings.Count(trimmed, "@") != 1 {
		return "", false
	}
	parsed, err := mail.ParseAddress(trimmed)
	if err != nil || parsed.Name != "" || parsed.Address != trimmed {
		return "", false
	}
	local, domain, ok := strings.Cut(parsed.Address, "@")
	if !ok || local == "" || domain == "" {
		return "", false
	}
	return local + "@" + strings.ToLower(domain), true
}

// ExtractEmail collects supported email claims from verified access and trusted
// ID tokens. Conflicting or malformed candidates fail closed to no email.
func ExtractEmail(accessToken, idToken, expectedProviderAccountID string) string {
	claims := make([]emailClaim, 0, 2)
	if claim, ok := extractEmailClaim(accessToken); ok {
		claims = append(claims, claim)
	}
	if claim, ok := extractEmailClaim(idToken); ok {
		if claim.accountID == "" || claim.accountID == expectedProviderAccountID {
			claims = append(claims, claim)
		}
	}
	var selected string
	for _, claim := range claims {
		if claim.accountID != "" && expectedProviderAccountID != "" && claim.accountID != expectedProviderAccountID {
			return ""
		}
		normalized, ok := NormalizeEmail(claim.email)
		if !ok {
			return ""
		}
		if selected == "" {
			selected = normalized
			continue
		}
		if selected != normalized {
			return ""
		}
	}
	return selected
}

func extractEmailClaim(token string) (emailClaim, bool) {
	payload, err := jwtPayload(token)
	if err != nil {
		return emailClaim{}, false
	}
	var raw map[string]json.RawMessage
	if json.Unmarshal(payload, &raw) != nil {
		return emailClaim{}, false
	}
	claim := emailClaim{accountID: extractAccountID(raw)}
	candidates := make([]string, 0, 3)
	appendEmail := func(raw json.RawMessage) {
		var value string
		if json.Unmarshal(raw, &value) == nil {
			candidates = append(candidates, value)
		} else {
			candidates = append(candidates, "\x00")
		}
	}
	if rawEmail, ok := raw["email"]; ok {
		appendEmail(rawEmail)
	}
	for _, key := range []string{profileClaimNamespace, "profile"} {
		if rawProfile, ok := raw[key]; ok {
			var profile map[string]json.RawMessage
			if json.Unmarshal(rawProfile, &profile) == nil {
				if rawEmail, ok := profile["email"]; ok {
					appendEmail(rawEmail)
				}
			}
		}
	}
	if len(candidates) == 0 {
		return claim, false
	}
	for _, candidate := range candidates {
		normalized, ok := NormalizeEmail(candidate)
		if !ok {
			return emailClaim{}, false
		}
		if claim.email == "" {
			claim.email = normalized
			continue
		}
		if claim.email != normalized {
			return emailClaim{}, false
		}
	}
	return claim, true
}

func extractAccountID(raw map[string]json.RawMessage) string {
	for _, key := range []string{"https://api.openai.com/auth", "auth"} {
		var auth struct {
			AccountID string `json:"chatgpt_account_id"`
		}
		if payload, ok := raw[key]; ok && json.Unmarshal(payload, &auth) == nil && strings.TrimSpace(auth.AccountID) != "" {
			return strings.TrimSpace(auth.AccountID)
		}
	}
	for _, key := range []string{"chatgpt_account_id", "account_id", "sub"} {
		var value string
		if payload, ok := raw[key]; ok && json.Unmarshal(payload, &value) == nil && strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func jwtPayload(token string) ([]byte, error) {
	parts := strings.Split(strings.TrimSpace(token), ".")
	if len(parts) != 3 {
		return nil, fmt.Errorf("expected JWT")
	}
	return base64.RawURLEncoding.DecodeString(parts[1])
}
