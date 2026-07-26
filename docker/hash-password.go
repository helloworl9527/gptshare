package main

import (
	"bufio"
	"crypto/rand"
	"encoding/base32"
	"encoding/base64"
	"fmt"
	"os"
	"strings"

	"golang.org/x/crypto/bcrypt"
)

func main() {
	if len(os.Args) == 2 && os.Args[1] == "--bootstrap" {
		bootstrap()
		return
	}
	if len(os.Args) != 1 {
		fmt.Fprintln(os.Stderr, "usage: vitals-password-hash [--bootstrap]")
		os.Exit(2)
	}
	password, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && len(password) == 0 {
		fmt.Fprintln(os.Stderr, "failed to read password from stdin")
		os.Exit(1)
	}
	password = strings.TrimRight(password, "\r\n")
	if len(password) < 16 {
		fmt.Fprintln(os.Stderr, "password must contain at least 16 characters")
		os.Exit(1)
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), 12)
	if err != nil {
		fmt.Fprintln(os.Stderr, "failed to hash password")
		os.Exit(1)
	}
	fmt.Println(string(hash))
}

func bootstrap() {
	password := randomEncoded(24, base64.RawURLEncoding)
	hash, err := bcrypt.GenerateFromPassword([]byte(password), 12)
	if err != nil {
		fatal()
	}
	fmt.Printf("ADMIN_PASSWORD=%s\n", password)
	fmt.Printf("ADMIN_PASSWORD_HASH=%s\n", hash)
	fmt.Printf("MONITOR_KEY=%s\n", randomEncoded(32, base64.StdEncoding))
	fmt.Printf("ALLOCATION_KEY=%s\n", randomEncoded(32, base64.RawURLEncoding))
	fmt.Printf("JWT_KEY=%s\n", randomEncoded(32, base64.StdEncoding))
	fmt.Printf("RATE_LIMIT_KEY=%s\n", randomEncoded(32, base64.StdEncoding))
	fmt.Printf("TOTP_SECRET=%s\n", randomEncoded(20, base32.StdEncoding.WithPadding(base32.NoPadding)))
}

func randomEncoded(size int, encoding interface{ EncodeToString([]byte) string }) string {
	value := make([]byte, size)
	if _, err := rand.Read(value); err != nil {
		fatal()
	}
	return encoding.EncodeToString(value)
}

func fatal() {
	fmt.Fprintln(os.Stderr, "failed to generate secure deployment credentials")
	os.Exit(1)
}
