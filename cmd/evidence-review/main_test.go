package main

import (
	"strings"
	"testing"
	"time"

	"github.com/pquerna/otp/totp"
)

func TestReadTOTPOnlyAcceptsSixDigitsFromStdin(t *testing.T) {
	code, err := readTOTP(strings.NewReader("123456\n"))
	if err != nil || code != "123456" {
		t.Fatalf("code=%q err=%v", code, err)
	}
	for _, input := range []string{"12345\n", "12345x\n", "1234567\n"} {
		if _, err := readTOTP(strings.NewReader(input)); err == nil {
			t.Fatalf("accepted %q", input)
		}
	}
}

func TestValidateCurrentTOTP(t *testing.T) {
	secret := "JBSWY3DPEHPK3PXPJBSWY3DPEHPK3PXP"
	now := time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC)
	code, err := totp.GenerateCode(secret, now)
	if err != nil {
		t.Fatal(err)
	}
	if !validateTOTP(code, secret, now) {
		t.Fatal("current TOTP was rejected")
	}
	if validateTOTP("000000", secret, now) {
		t.Fatal("incorrect TOTP was accepted")
	}
}
