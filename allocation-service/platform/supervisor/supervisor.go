package supervisor

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"sync"
	"time"
)

const (
	defaultPanicThreshold = 3
	defaultInitialBackoff = 100 * time.Millisecond
	defaultMaximumBackoff = 5 * time.Second
)

type TaskFunc func(context.Context) error

type Config struct {
	PanicThreshold int
	InitialBackoff time.Duration
	MaximumBackoff time.Duration
}

type TaskState struct {
	Module            string
	Task              string
	State             string
	ConsecutivePanics int
}

type Supervisor struct {
	logger *slog.Logger
	cfg    Config
	mu     sync.RWMutex
	tasks  map[string]TaskState
	wg     sync.WaitGroup
}

func New(logger *slog.Logger, cfg Config) *Supervisor {
	if logger == nil {
		logger = slog.Default()
	}
	if cfg.PanicThreshold <= 0 {
		cfg.PanicThreshold = defaultPanicThreshold
	}
	if cfg.InitialBackoff <= 0 {
		cfg.InitialBackoff = defaultInitialBackoff
	}
	if cfg.MaximumBackoff <= 0 {
		cfg.MaximumBackoff = defaultMaximumBackoff
	}
	if cfg.MaximumBackoff < cfg.InitialBackoff {
		cfg.MaximumBackoff = cfg.InitialBackoff
	}
	return &Supervisor{logger: logger, cfg: cfg, tasks: make(map[string]TaskState)}
}

func (s *Supervisor) GoManaged(ctx context.Context, name, module string, fn TaskFunc) error {
	if ctx == nil || name == "" || module == "" || fn == nil {
		return errors.New("managed task requires context, name, module, and function")
	}
	key := module + "/" + name
	s.mu.Lock()
	if _, exists := s.tasks[key]; exists {
		s.mu.Unlock()
		return fmt.Errorf("managed task %s is already registered", key)
	}
	s.tasks[key] = TaskState{Module: module, Task: name, State: "starting"}
	s.mu.Unlock()
	s.wg.Add(1)
	go s.run(ctx, key, fn)
	return nil
}

func (s *Supervisor) run(ctx context.Context, key string, fn TaskFunc) {
	defer s.wg.Done()
	consecutivePanics := 0
	for attempt := 1; ; attempt++ {
		if ctx.Err() != nil {
			s.setState(key, "stopped", consecutivePanics)
			return
		}
		s.setState(key, "running", consecutivePanics)
		runID := newRunID()
		panicked, panicType, err := invoke(ctx, fn)
		if ctx.Err() != nil {
			s.setState(key, "stopped", consecutivePanics)
			return
		}
		state := s.state(key)
		if panicked {
			consecutivePanics++
			s.logger.Error("managed background task panicked",
				"module", state.Module,
				"task", state.Task,
				"panic_type", panicType,
				"error_code", "background_task_panic",
				"run_id", runID,
				"consecutive_panics", consecutivePanics,
			)
			if consecutivePanics >= s.cfg.PanicThreshold {
				s.setState(key, "degraded", consecutivePanics)
				s.logger.Error("managed background task entered terminal degraded state",
					"module", state.Module,
					"task", state.Task,
					"error_code", "background_task_terminal_degraded",
					"run_id", runID,
					"consecutive_panics", consecutivePanics,
				)
				return
			}
		} else {
			consecutivePanics = 0
			code := "background_task_stopped"
			if err != nil {
				code = "background_task_failed"
			}
			s.logger.Error("managed background task exited unexpectedly",
				"module", state.Module,
				"task", state.Task,
				"error_code", code,
				"run_id", runID,
			)
		}
		s.setState(key, "restarting", consecutivePanics)
		if !sleep(ctx, s.backoff(attempt)) {
			s.setState(key, "stopped", consecutivePanics)
			return
		}
	}
}

func invoke(ctx context.Context, fn TaskFunc) (panicked bool, panicType string, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			panicked = true
			panicType = reflect.TypeOf(recovered).String()
			if panicType == "" {
				panicType = "unknown"
			}
		}
	}()
	err = fn(ctx)
	return false, "", err
}

func (s *Supervisor) BackgroundStatus() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, task := range s.tasks {
		if task.State == "degraded" {
			return "degraded"
		}
	}
	return "ok"
}

func (s *Supervisor) States() []TaskState {
	s.mu.RLock()
	defer s.mu.RUnlock()
	states := make([]TaskState, 0, len(s.tasks))
	for _, state := range s.tasks {
		states = append(states, state)
	}
	return states
}

func (s *Supervisor) Wait() {
	s.wg.Wait()
}

func (s *Supervisor) state(key string) TaskState {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.tasks[key]
}

func (s *Supervisor) setState(key, state string, consecutivePanics int) {
	s.mu.Lock()
	item := s.tasks[key]
	item.State = state
	item.ConsecutivePanics = consecutivePanics
	s.tasks[key] = item
	s.mu.Unlock()
}

func (s *Supervisor) backoff(attempt int) time.Duration {
	delay := s.cfg.InitialBackoff
	for i := 1; i < attempt && delay < s.cfg.MaximumBackoff; i++ {
		delay *= 2
		if delay > s.cfg.MaximumBackoff {
			return s.cfg.MaximumBackoff
		}
	}
	return delay
}

func sleep(ctx context.Context, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func newRunID() string {
	var value [8]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "unavailable"
	}
	return hex.EncodeToString(value[:])
}
