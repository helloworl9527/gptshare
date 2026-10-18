package usercenter

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

var (
	testTicketKey = bytes.Repeat([]byte{0x5c}, 32)
	testNow       = time.Date(2026, 10, 18, 8, 0, 0, 0, time.UTC)
)

func signRaw(t *testing.T, key []byte, payload []byte) string {
	t.Helper()
	encoded := base64.RawURLEncoding.EncodeToString(payload)
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(encoded))
	return encoded + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func signPayload(t *testing.T, key []byte, payload map[string]any) string {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	return signRaw(t, key, raw)
}

func validPayload() map[string]any {
	return map[string]any{"v": 1, "svc": "sharegpt", "uid": "u_9f3k2m7q", "nonce": "AbCdEfGhIjKlMnOpQrStUv", "exp": testNow.Add(5 * time.Minute).Unix()}
}

func testVerifier(t *testing.T) *Verifier {
	t.Helper()
	verifier, err := NewVerifier(testTicketKey)
	if err != nil {
		t.Fatal(err)
	}
	verifier.SetNow(func() time.Time { return testNow })
	return verifier
}

func TestVerifyAcceptsValidTicket(t *testing.T) {
	ticket, err := testVerifier(t).Verify(signPayload(t, testTicketKey, validPayload()))
	if err != nil {
		t.Fatal(err)
	}
	if ticket.UserUID != "u_9f3k2m7q" || ticket.Nonce != "AbCdEfGhIjKlMnOpQrStUv" || ticket.Exp != testNow.Add(5*time.Minute).Unix() {
		t.Fatalf("unexpected ticket: %+v", ticket)
	}
}

func TestVerifyClockSkewBoundary(t *testing.T) {
	verifier := testVerifier(t)
	withinSkew := validPayload()
	withinSkew["exp"] = testNow.Add(-29 * time.Second).Unix()
	if _, err := verifier.Verify(signPayload(t, testTicketKey, withinSkew)); err != nil {
		t.Fatalf("ticket expired 29s ago is inside the 30s skew window: %v", err)
	}
	for _, age := range []time.Duration{30 * time.Second, 31 * time.Second, time.Hour} {
		expired := validPayload()
		expired["exp"] = testNow.Add(-age).Unix()
		if _, err := verifier.Verify(signPayload(t, testTicketKey, expired)); !errors.Is(err, ErrTicketInvalid) {
			t.Fatalf("ticket expired %s ago must be rejected, got %v", age, err)
		}
	}
}

func TestVerifyRejectsInvalidTickets(t *testing.T) {
	verifier := testVerifier(t)
	valid := signPayload(t, testTicketKey, validPayload())
	payloadPart, sigPart, _ := strings.Cut(valid, ".")
	mutated := func(change func(map[string]any)) string {
		payload := validPayload()
		change(payload)
		return signPayload(t, testTicketKey, payload)
	}
	flipped := []byte(sigPart)
	if flipped[0] == 'A' {
		flipped[0] = 'B'
	} else {
		flipped[0] = 'A'
	}
	otherPayload := validPayload()
	otherPayload["uid"] = "u_attacker"
	forgedPayload := base64.RawURLEncoding.EncodeToString(mustJSON(t, otherPayload))

	cases := map[string]string{
		"empty":                 "",
		"no separator":          payloadPart,
		"three parts":           valid + ".x",
		"empty signature":       payloadPart + ".",
		"empty payload":         "." + sigPart,
		"tampered signature":    payloadPart + "." + string(flipped),
		"tampered payload":      forgedPayload + "." + sigPart,
		"wrong key":             signPayload(t, bytes.Repeat([]byte{0x36}, 32), validPayload()),
		"padded signature":      payloadPart + "." + sigPart + "=",
		"std base64 signature":  payloadPart + "." + base64.StdEncoding.EncodeToString([]byte(sigPart)),
		"short signature":       payloadPart + "." + base64.RawURLEncoding.EncodeToString([]byte("short")),
		"wrong service":         mutated(func(p map[string]any) { p["svc"] = "otherservice" }),
		"missing service":       mutated(func(p map[string]any) { delete(p, "svc") }),
		"version 2":             mutated(func(p map[string]any) { p["v"] = 2 }),
		"version string":        mutated(func(p map[string]any) { p["v"] = "1" }),
		"missing version":       mutated(func(p map[string]any) { delete(p, "v") }),
		"missing exp":           mutated(func(p map[string]any) { delete(p, "exp") }),
		"exp string":            mutated(func(p map[string]any) { p["exp"] = "9999999999" }),
		"missing uid":           mutated(func(p map[string]any) { delete(p, "uid") }),
		"empty uid":             mutated(func(p map[string]any) { p["uid"] = "" }),
		"uid with space":        mutated(func(p map[string]any) { p["uid"] = "u 1" }),
		"uid too long":          mutated(func(p map[string]any) { p["uid"] = strings.Repeat("u", 129) }),
		"missing nonce":         mutated(func(p map[string]any) { delete(p, "nonce") }),
		"short nonce":           mutated(func(p map[string]any) { p["nonce"] = "abc" }),
		"nonce bad charset":     mutated(func(p map[string]any) { p["nonce"] = "AbCdEfGhIjKlMnOpQrSt+/" }),
		"signed non-json":       signRaw(t, testTicketKey, []byte("not json")),
		"signed trailing json":  signRaw(t, testTicketKey, append(mustJSON(t, validPayload()), []byte(` {}`)...)),
		"signed json array":     signRaw(t, testTicketKey, []byte(`[1]`)),
		"oversized":             strings.Repeat("a", 3000) + "." + sigPart,
		"signature over json":   base64.RawURLEncoding.EncodeToString(mustJSON(t, validPayload())) + "." + signatureOverRawJSON(t),
		"padded payload signed": signPaddedPayload(t),
		"expired one hour":      mutated(func(p map[string]any) { p["exp"] = testNow.Add(-time.Hour).Unix() }),
		"service case mismatch": mutated(func(p map[string]any) { p["svc"] = "ShareGPT" }),
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := verifier.Verify(raw); !errors.Is(err, ErrTicketInvalid) {
				t.Fatalf("expected ErrTicketInvalid, got %v", err)
			}
		})
	}
}

// signPaddedPayload 用带 "=" 填充的 base64url 载荷正确签名；本契约只接受无填充编码。
func signPaddedPayload(t *testing.T) string {
	t.Helper()
	payload := validPayload()
	for uid := "u_1"; ; uid += "x" {
		payload["uid"] = uid
		encoded := base64.URLEncoding.EncodeToString(mustJSON(t, payload))
		if strings.HasSuffix(encoded, "=") {
			mac := hmac.New(sha256.New, testTicketKey)
			mac.Write([]byte(encoded))
			return encoded + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
		}
	}
}

// signatureOverRawJSON 用 JSON 原文（而不是 payload_b64url）算签名，必须被拒绝。
func signatureOverRawJSON(t *testing.T) string {
	mac := hmac.New(sha256.New, testTicketKey)
	mac.Write(mustJSON(t, validPayload()))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestNewVerifierRequires32ByteKey(t *testing.T) {
	for _, key := range [][]byte{nil, bytes.Repeat([]byte{1}, 16), bytes.Repeat([]byte{1}, 33)} {
		if _, err := NewVerifier(key); err == nil {
			t.Fatalf("key of %d bytes must be rejected", len(key))
		}
	}
}

func TestMaskUsername(t *testing.T) {
	cases := map[string]string{
		"share-07@foo.com":     "share-07＠***.com",
		"share-07@mail.foo.cn": "share-07＠***.cn",
		"user@localhost":       "user＠***",
		"plainname":            "plai***",
		"ab":                   "a***",
	}
	for input, want := range cases {
		if got := MaskUsername(input); got != want {
			t.Fatalf("MaskUsername(%q)=%q want %q", input, got, want)
		}
		if strings.Contains(MaskUsername(input), "foo") || strings.Contains(MaskUsername(input), "mail") {
			t.Fatalf("masked username leaked domain: %q", MaskUsername(input))
		}
	}
}
