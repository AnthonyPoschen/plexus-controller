package supervisor

import (
	"context"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

type stubAdapter struct {
	mu      sync.Mutex
	starts  int
	failFor int
	grace   time.Duration
	stopErr error
	stopped chan struct{}
}

func (a *stubAdapter) Name() string { return "stub" }

func (a *stubAdapter) GracePeriod() time.Duration {
	if a.grace > 0 {
		return a.grace
	}
	return time.Second
}

func (a *stubAdapter) Command(context.Context) (*exec.Cmd, error) {
	a.mu.Lock()
	a.starts++
	fail := a.starts <= a.failFor
	a.mu.Unlock()
	if fail {
		return exec.Command("/bin/sh", "-c", "exit 3"), nil
	}
	cmd := exec.Command("/bin/sh", "-c", "trap 'exit 0' TERM; echo started; while true; do sleep 0.05; done")
	return cmd, nil
}

func (a *stubAdapter) GracefulStop(_ context.Context, proc *os.Process) error {
	if a.stopped != nil {
		select {
		case <-a.stopped:
		default:
			close(a.stopped)
		}
	}
	if a.stopErr != nil {
		return a.stopErr
	}
	if proc != nil {
		return proc.Signal(syscall.SIGTERM)
	}
	return nil
}

func (a *stubAdapter) startCount() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.starts
}

func TestRunRestartsThenExitsForReschedule(t *testing.T) {
	adapter := &stubAdapter{failFor: 100}
	err := Supervisor{Adapter: adapter, MaxProcessRecoveries: 2, RecoverDelay: time.Millisecond, Output: io.Discard}.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "exited unexpectedly") {
		t.Fatalf("recovery exhaustion = %v", err)
	}
	if got := adapter.startCount(); got != 3 {
		t.Fatalf("starts = %d, want 3 (initial plus 2 recoveries)", got)
	}
}

func TestRunRecoversUnexpectedExitThenGracefulStop(t *testing.T) {
	adapter := &stubAdapter{failFor: 2, stopped: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- Supervisor{Adapter: adapter, MaxProcessRecoveries: 3, RecoverDelay: time.Millisecond, Output: io.Discard}.Run(ctx)
	}()

	deadline := time.Now().Add(2 * time.Second)
	for adapter.startCount() < 3 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if adapter.startCount() < 3 {
		cancel()
		t.Fatalf("starts = %d, want recovered third start", adapter.startCount())
	}
	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("graceful stop after recovery = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("supervisor did not exit after graceful stop")
	}
	if adapter.startCount() != 3 {
		t.Fatalf("starts = %d, want 3", adapter.startCount())
	}
}

func TestRunSIGTERMRunsGracefulStop(t *testing.T) {
	adapter := &stubAdapter{stopped: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- Supervisor{Adapter: adapter, RecoverDelay: time.Millisecond, Output: io.Discard}.Run(ctx)
	}()
	deadline := time.Now().Add(2 * time.Second)
	for adapter.startCount() < 1 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if adapter.startCount() < 1 {
		cancel()
		t.Fatal("game process never started")
	}
	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("SIGTERM graceful stop = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("supervisor did not exit 0 after SIGTERM")
	}
	select {
	case <-adapter.stopped:
	default:
		t.Fatal("adapter graceful stop was not invoked")
	}
}

func TestRunStopDuringRecoveryIsGraceful(t *testing.T) {
	adapter := &stubAdapter{failFor: 100}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- Supervisor{Adapter: adapter, MaxProcessRecoveries: 5, RecoverDelay: 50 * time.Millisecond, Output: io.Discard}.Run(ctx)
	}()
	deadline := time.Now().Add(2 * time.Second)
	for adapter.startCount() < 1 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("stop during recovery = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("supervisor did not exit after stop during recovery")
	}
}

func TestRunTreatsAnyExitAfterSIGTERMAsGraceful(t *testing.T) {
	adapter := &nonzeroStopAdapter{started: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- Supervisor{Adapter: adapter, RecoverDelay: time.Millisecond, Output: io.Discard}.Run(ctx)
	}()
	select {
	case <-adapter.started:
	case <-time.After(2 * time.Second):
		cancel()
		t.Fatal("game process never started")
	}
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("non-zero exit after SIGTERM should still be graceful: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("supervisor did not exit after SIGTERM")
	}
}

func TestRunGracefulStopTimeoutIsNotZero(t *testing.T) {
	adapter := &hangingStopAdapter{started: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- Supervisor{Adapter: adapter, RecoverDelay: time.Millisecond, Output: io.Discard}.Run(ctx)
	}()
	select {
	case <-adapter.started:
	case <-time.After(2 * time.Second):
		cancel()
		t.Fatal("game process never started")
	}
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "grace period") {
			t.Fatalf("timeout = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("supervisor did not exit after grace period")
	}
	if adapter.proc != nil {
		_ = adapter.proc.Kill()
	}
}

type nonzeroStopAdapter struct{ started chan struct{} }

func (a *nonzeroStopAdapter) Name() string               { return "stub" }
func (a *nonzeroStopAdapter) GracePeriod() time.Duration { return time.Second }
func (a *nonzeroStopAdapter) Command(context.Context) (*exec.Cmd, error) {
	cmd := exec.Command("/bin/sh", "-c", "trap 'exit 7' TERM; echo started; while true; do sleep 0.05; done")
	notifyOnce(a.started)
	return cmd, nil
}
func (a *nonzeroStopAdapter) GracefulStop(_ context.Context, proc *os.Process) error {
	return proc.Signal(syscall.SIGTERM)
}

type hangingStopAdapter struct {
	started chan struct{}
	proc    *os.Process
}

func (a *hangingStopAdapter) Name() string               { return "stub" }
func (a *hangingStopAdapter) GracePeriod() time.Duration { return 80 * time.Millisecond }
func (a *hangingStopAdapter) Command(context.Context) (*exec.Cmd, error) {
	cmd := exec.Command("/bin/sh", "-c", "trap '' TERM; echo started; while true; do sleep 0.05; done")
	notifyOnce(a.started)
	return cmd, nil
}
func (a *hangingStopAdapter) GracefulStop(_ context.Context, proc *os.Process) error {
	a.proc = proc
	return nil
}

func notifyOnce(ch chan struct{}) {
	select {
	case <-ch:
	default:
		close(ch)
	}
}

func TestPackagesDoNotDependOnKubernetes(t *testing.T) {
	cmd := exec.Command("go", "list", "-deps",
		"github.com/AnthonyPoschen/plexus-controller/cmd/game-supervisor",
		"github.com/AnthonyPoschen/plexus-controller/internal/supervisor",
		"github.com/AnthonyPoschen/plexus-controller/internal/supervisor/factorio",
	)
	cmd.Dir = "../.."
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list: %v", err)
	}
	for _, line := range strings.Split(string(out), "\n") {
		if strings.HasPrefix(line, "k8s.io/") || strings.Contains(line, "sigs.k8s.io/controller-runtime") {
			t.Fatalf("supervisor must not watch or reconcile GameServer CRs; depends on %q", line)
		}
	}
}
