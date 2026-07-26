package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/user"
	"strings"
	"time"

	"chatgpt-monitor/internal/chatgpt"
	"chatgpt-monitor/internal/config"
	credentialcrypto "chatgpt-monitor/internal/crypto"
	"chatgpt-monitor/internal/monitor"
	"chatgpt-monitor/internal/store"
	"github.com/pquerna/otp"
	"github.com/pquerna/otp/totp"
)

func main() {
	signature := flag.String("signature", "", "exact evidence signature")
	decision := flag.String("decision", "", "confirm or reject")
	reason := flag.String("reason", "", "audit reason without raw upstream data")
	flag.Parse()
	if flag.NArg() != 0 {
		fatal("unexpected positional arguments")
	}
	cfg, err := config.Load()
	if err != nil {
		fatal("configuration rejected")
	}
	if err := monitor.ValidatePrivateDB(cfg.DBPath); err != nil {
		fatal(err.Error())
	}
	if err := monitor.ValidateEnvironmentFile(os.Getenv("ENVIRONMENT_FILE")); err != nil {
		fatal(err.Error())
	}
	lock, err := monitor.AcquireServiceLock(cfg.DBPath)
	if err != nil {
		fatal("service must be stopped before evidence review")
	}
	defer lock.Close()
	code, err := readTOTP(os.Stdin)
	if err != nil {
		fatal("current TOTP is required on stdin")
	}
	secret := strings.TrimSpace(os.Getenv("ADMIN_TOTP_SECRET"))
	valid := validateTOTP(code, secret, time.Now().UTC())
	code, secret = "", ""
	if !valid {
		fatal("TOTP verification failed")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	database, err := store.Open(ctx, cfg.DBPath, os.DirFS(cfg.MigrationsDir))
	if err != nil {
		fatal("database open failed")
	}
	defer database.Close()
	keyring, err := credentialcrypto.NewKeyring(cfg.CredentialMasterKeys, cfg.CredentialActiveKeyID)
	if err != nil {
		fatal("credential keyring rejected")
	}
	client := chatgpt.NewClient(chatgpt.Config{EvidenceLevel: chatgpt.EvidenceLiveVerified})
	service, err := monitor.New(database.DB(), client, keyring, monitor.DefaultConfig())
	if err != nil {
		fatal("review service initialization failed")
	}
	current, err := user.Current()
	if err != nil || current.Username == "" {
		fatal("operator identity unavailable")
	}
	request := monitor.ReviewRequest{Signature: *signature, Decision: monitor.ReviewDecision(*decision), Reason: *reason, Operator: current.Username}
	result, err := service.ReviewEvidence(ctx, request)
	if err != nil {
		fatal("evidence review rejected")
	}
	fmt.Printf("decision=%s accounts_affected=%d alerts_created=%d reviewed_at=%s\n", result.Decision, result.AccountsAffected, result.AlertsCreated, result.ReviewedAt.Format(time.RFC3339))
	if result.Decision == monitor.ReviewReject {
		fmt.Println("action_required=reopen_codex_review")
	}
}

func validateTOTP(code, secret string, now time.Time) bool {
	valid, err := totp.ValidateCustom(code, secret, now, totp.ValidateOpts{Period: 30, Skew: 1, Digits: otp.DigitsSix, Algorithm: otp.AlgorithmSHA1})
	return err == nil && valid
}

func readTOTP(reader io.Reader) (string, error) {
	text, err := bufio.NewReader(io.LimitReader(reader, 16)).ReadString('\n')
	if err != nil && err != io.EOF {
		return "", err
	}
	text = strings.TrimSpace(text)
	if len(text) != 6 {
		return "", fmt.Errorf("invalid code length")
	}
	for _, value := range text {
		if value < '0' || value > '9' {
			return "", fmt.Errorf("invalid code")
		}
	}
	return text, nil
}

func fatal(message string) {
	fmt.Fprintln(os.Stderr, "evidence-review:", message)
	os.Exit(1)
}
