package e2e

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"chatgpt-monitor/internal/chatgpt"
	credentialcrypto "chatgpt-monitor/internal/crypto"
	"chatgpt-monitor/internal/monitor"
	"chatgpt-monitor/internal/store"
)

const (
	resourceAccountCount  = 100
	resourceFailedAccount = "resource-acct-017"
)

type resourceClient struct {
	delay     time.Duration
	active    atomic.Int32
	maxActive atomic.Int32
	calls     atomic.Int32
}

func (client *resourceClient) ExchangeCredential(context.Context, chatgpt.CredentialKind, string) (chatgpt.TokenSet, error) {
	return chatgpt.TokenSet{}, fmt.Errorf("resource test credentials must not require refresh")
}

func (client *resourceClient) FetchStatus(ctx context.Context, access string) (chatgpt.StatusResult, error) {
	pid, err := resourceTokenPID(access)
	if err != nil {
		return chatgpt.StatusResult{}, err
	}
	client.calls.Add(1)
	active := client.active.Add(1)
	defer client.active.Add(-1)
	for {
		maximum := client.maxActive.Load()
		if active <= maximum || client.maxActive.CompareAndSwap(maximum, active) {
			break
		}
	}
	timer := time.NewTimer(client.delay)
	defer timer.Stop()
	select {
	case <-timer.C:
	case <-ctx.Done():
		return chatgpt.StatusResult{}, ctx.Err()
	}
	if pid == resourceFailedAccount {
		return chatgpt.StatusResult{}, &chatgpt.TypedError{
			Kind:                  chatgpt.ErrorPermissionDenied,
			EvidenceCode:          "http_403",
			EvidenceLevel:         chatgpt.EvidenceContractVerifiedLivePending,
			PreserveBusinessState: true,
		}
	}
	expiry := time.Now().UTC().Add(45 * 24 * time.Hour)
	return chatgpt.StatusResult{
		ProviderAccountID:  pid,
		RawPlan:            "chatgptplusplan",
		Plan:               chatgpt.PlanPlus,
		SubscriptionExpiry: &expiry,
		AccountState:       chatgpt.StateActive,
		EvidenceCode:       "access_claim+accounts_check_2xx",
		EvidenceLevel:      chatgpt.EvidenceLiveVerified,
	}, nil
}

// TestResource100Accounts30Minutes is skipped in ordinary regression runs. STEP-11
// invokes it explicitly with RUN_STEP11_RESOURCE=1 and a 30 minute duration.
func TestResource100Accounts30Minutes(t *testing.T) {
	if os.Getenv("RUN_STEP11_RESOURCE") != "1" {
		t.Skip("set RUN_STEP11_RESOURCE=1 for the explicit STEP-11 resource run")
	}
	duration := resourceDuration(t, "STEP11_RESOURCE_DURATION", 30*time.Minute)
	upstreamDelay := resourceDuration(t, "STEP11_RESOURCE_UPSTREAM_DELAY", 9*time.Second)
	if duration < 30*time.Minute && os.Getenv("STEP11_RESOURCE_SMOKE") != "1" {
		t.Fatalf("STEP-11 resource duration must be at least 30 minutes, got %s", duration)
	}

	previousProcs := runtime.GOMAXPROCS(2)
	defer runtime.GOMAXPROCS(previousProcs)
	previousLimit := debug.SetMemoryLimit(2500 << 20)
	defer debug.SetMemoryLimit(previousLimit)

	started := time.Now()
	workDir := t.TempDir()
	if err := os.Chmod(workDir, 0o700); err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(workDir, "resource.db")
	projectRoot := strings.TrimSpace(os.Getenv("STEP11_PROJECT_ROOT"))
	if projectRoot == "" {
		projectRoot = filepath.Join("..", "..")
	}
	migrationsPath, err := filepath.Abs(filepath.Join(projectRoot, "migrations"))
	if err != nil {
		t.Fatal(err)
	}
	database, err := store.Open(context.Background(), dbPath, os.DirFS(migrationsPath))
	if err != nil {
		t.Fatal(err)
	}
	keyring, err := credentialcrypto.NewKeyring(map[string][]byte{"resource": bytes.Repeat([]byte{0x2a}, 32)}, "resource")
	if err != nil {
		t.Fatal(err)
	}
	seedResourceAccounts(t, database.DB(), keyring, time.Now().UTC())

	client := &resourceClient{delay: upstreamDelay}
	service, err := monitor.New(database.DB(), client, keyring, monitor.DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	writeResourcePhase(t, "poll")
	pollCPUStart := resourceCPUTime(t)
	pollStarted := time.Now()
	run, err := service.RunScheduled(context.Background())
	pollElapsed := time.Since(pollStarted)
	pollCPU := resourceCPUTime(t) - pollCPUStart
	if err != nil {
		t.Fatal(err)
	}
	if run.State != "completed" || run.AccountsTotal != resourceAccountCount || run.AccountsOK != resourceAccountCount-1 || run.AccountsFailed != 1 {
		t.Fatalf("unexpected resource run: %+v", run)
	}
	if client.calls.Load() != resourceAccountCount || client.maxActive.Load() != 3 {
		t.Fatalf("calls=%d max_active=%d", client.calls.Load(), client.maxActive.Load())
	}
	var alive, errorsCount int
	if err := database.DB().QueryRow(`SELECT
		sum(status='alive'),
		sum(last_check_state='error')
		FROM accounts WHERE deleted_at IS NULL`).Scan(&alive, &errorsCount); err != nil {
		t.Fatal(err)
	}
	if alive != resourceAccountCount || errorsCount != 1 {
		t.Fatalf("single account failure affected cohort: alive=%d errors=%d", alive, errorsCount)
	}

	if _, err := database.DB().Exec(`INSERT INTO poll_runs
		(id,started_at,state,accounts_total,trigger_type,error_counts_json)
		VALUES ('resource-interrupted',?,'running',100,'scheduled','{}')`, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := store.Open(context.Background(), dbPath, os.DirFS(migrationsPath))
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	restarted, err := monitor.New(reopened.DB(), client, keyring, monitor.DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	if err := restarted.RecoverInterrupted(context.Background()); err != nil {
		t.Fatal(err)
	}
	recovered, err := restarted.GetRun(context.Background(), "resource-interrupted")
	if err != nil || recovered.State != "interrupted" || recovered.ErrorCode != "startup_interrupted" {
		t.Fatalf("restart recovery=%+v err=%v", recovered, err)
	}
	var accountRows, epochRows int
	if err := reopened.DB().QueryRow("SELECT count(*) FROM accounts WHERE deleted_at IS NULL").Scan(&accountRows); err != nil {
		t.Fatal(err)
	}
	if err := reopened.DB().QueryRow("SELECT count(*) FROM authorization_epochs WHERE ended_at IS NULL").Scan(&epochRows); err != nil {
		t.Fatal(err)
	}
	if accountRows != resourceAccountCount || epochRows != resourceAccountCount {
		t.Fatalf("restart persistence accounts=%d epochs=%d", accountRows, epochRows)
	}

	writeResourcePhase(t, "idle")
	idleCPUStart := resourceCPUTime(t)
	idleStarted := time.Now()
	if remaining := duration - time.Since(started); remaining > 0 {
		timer := time.NewTimer(remaining)
		<-timer.C
	}
	idleElapsed := time.Since(idleStarted)
	idleCPU := resourceCPUTime(t) - idleCPUStart
	writeResourcePhase(t, "done")

	t.Logf("RESOURCE_RESULT accounts=%d ok=%d failed=%d max_workers=%d poll_seconds=%.3f poll_cpu_single_core_pct=%.3f idle_seconds=%.3f idle_cpu_single_core_pct=%.3f total_seconds=%.3f restart_recovered=1",
		resourceAccountCount, run.AccountsOK, run.AccountsFailed, client.maxActive.Load(),
		pollElapsed.Seconds(), cpuPercent(pollCPU, pollElapsed),
		idleElapsed.Seconds(), cpuPercent(idleCPU, idleElapsed), time.Since(started).Seconds())
}

func seedResourceAccounts(t *testing.T, db *sql.DB, keyring *credentialcrypto.Keyring, now time.Time) {
	t.Helper()
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	for index := 1; index <= resourceAccountCount; index++ {
		id := int64(index)
		pid := fmt.Sprintf("resource-acct-%03d", index)
		access := resourceJWT(pid, now.Add(2*time.Hour))
		payload, err := json.Marshal(map[string]string{"access": access})
		if err != nil {
			t.Fatal(err)
		}
		envelope, err := keyring.Seal(payload, credentialcrypto.CredentialAAD(id, "access"))
		if err != nil {
			t.Fatal(err)
		}
		authExpiry := now.Add(24 * time.Hour).Format(time.RFC3339Nano)
		imported := now.Add(-24 * time.Hour).Format(time.RFC3339Nano)
		if _, err := tx.Exec(`INSERT INTO accounts
			(id,provider_account_id,label,token_type,enc_credentials,credential_key_id,
			 plan,raw_plan,current_expiry,auth_expiry,status,last_alive_at,import_time,last_check_state,updated_at)
			VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			id, pid, "Synthetic "+strconv.Itoa(index), "access", envelope, keyring.ActiveKeyID(),
			"plus", "chatgptplusplan", now.Add(30*24*time.Hour).Format(time.RFC3339Nano), authExpiry,
			"alive", imported, imported, "ok", now.Format(time.RFC3339Nano)); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(`INSERT INTO authorization_epochs
			(id,account_id,started_at,auth_expiry) VALUES (?,?,?,?)`, id, id, imported, authExpiry); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

func resourceJWT(pid string, expiry time.Time) string {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`))
	payload, _ := json.Marshal(map[string]any{"pid": pid, "exp": expiry.Unix()})
	return header + "." + base64.RawURLEncoding.EncodeToString(payload) + ".fixture"
}

func resourceTokenPID(token string) (string, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return "", fmt.Errorf("invalid resource token")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", err
	}
	var claims struct {
		PID string `json:"pid"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil || claims.PID == "" {
		return "", fmt.Errorf("invalid resource token claims")
	}
	return claims.PID, nil
}

func resourceDuration(t *testing.T, name string, fallback time.Duration) time.Duration {
	t.Helper()
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback
	}
	parsed, err := time.ParseDuration(value)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return parsed
}

func writeResourcePhase(t *testing.T, phase string) {
	t.Helper()
	path := strings.TrimSpace(os.Getenv("STEP11_RESOURCE_PHASE_FILE"))
	if path == "" {
		return
	}
	if err := os.WriteFile(path, []byte(phase+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func resourceCPUTime(t *testing.T) time.Duration {
	t.Helper()
	var usage syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &usage); err != nil {
		t.Fatal(err)
	}
	return timevalDuration(usage.Utime) + timevalDuration(usage.Stime)
}

func timevalDuration(value syscall.Timeval) time.Duration {
	return time.Duration(value.Sec)*time.Second + time.Duration(value.Usec)*time.Microsecond
}

func cpuPercent(cpu, wall time.Duration) float64 {
	if wall <= 0 {
		return 0
	}
	return 100 * float64(cpu) / float64(wall)
}
