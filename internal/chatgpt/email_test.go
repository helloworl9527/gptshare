package chatgpt

import (
	"encoding/base64"
	"encoding/json"
	"testing"
)

func emailJWT(t *testing.T, claims map[string]any) string {
	t.Helper()
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	return "e30." + base64.RawURLEncoding.EncodeToString(payload) + ".signature"
}

func TestNormalizeEmail(t *testing.T) {
	tests := []struct {
		name  string
		raw   string
		want  string
		valid bool
	}{
		{name: "domain lowercased", raw: "User.Name@EXAMPLE.TEST", want: "User.Name@example.test", valid: true},
		{name: "display name rejected", raw: "User <user@example.test>", valid: false},
		{name: "crlf rejected", raw: "user@example.test\r\n", valid: false},
		{name: "space rejected", raw: "user name@example.test", valid: false},
		{name: "too long rejected", raw: "local@" + string(make([]byte, 250)) + ".test", valid: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, ok := NormalizeEmail(test.raw)
			if ok != test.valid || got != test.want {
				t.Fatalf("NormalizeEmail(%q)=(%q,%v), want (%q,%v)", test.raw, got, ok, test.want, test.valid)
			}
		})
	}
}

func TestExtractEmailClaimShapesAndConflicts(t *testing.T) {
	auth := map[string]any{"chatgpt_account_id": "acct-1"}
	access := emailJWT(t, map[string]any{
		"https://api.openai.com/auth":    auth,
		"https://api.openai.com/profile": map[string]any{"email": "Owner@EXAMPLE.TEST"},
	})
	if got := ExtractEmail(access, "", "acct-1"); got != "Owner@example.test" {
		t.Fatalf("namespace email=%q", got)
	}
	profile := emailJWT(t, map[string]any{"profile": map[string]any{"email": "nested@example.test"}})
	if got := ExtractEmail(profile, "", ""); got != "nested@example.test" {
		t.Fatalf("profile email=%q", got)
	}
	top := emailJWT(t, map[string]any{"email": "top@example.test"})
	if got := ExtractEmail(top, "", ""); got != "top@example.test" {
		t.Fatalf("top email=%q", got)
	}
	conflict := emailJWT(t, map[string]any{"email": "one@example.test", "profile": map[string]any{"email": "two@example.test"}})
	if got := ExtractEmail(conflict, "", ""); got != "" {
		t.Fatalf("conflict returned %q", got)
	}
	idMismatch := emailJWT(t, map[string]any{"sub": "acct-other", "email": "id@example.test"})
	if got := ExtractEmail("", idMismatch, "acct-1"); got != "" {
		t.Fatalf("id mismatch returned %q", got)
	}
}
