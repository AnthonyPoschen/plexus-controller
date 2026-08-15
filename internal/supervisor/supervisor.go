package supervisor

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"time"
)

// DefaultMaxProcessRecoveries is the number of in-pod restarts after the first
// unexpected game-process exit. The next failure exits so Kubernetes can reschedule.
const DefaultMaxProcessRecoveries = 3

const defaultRecoverDelay = time.Second

// Adapter is one game's boot and graceful-stop contract. It must not watch or
// reconcile GameServer objects.
type Adapter interface {
	Name() string
	Command(ctx context.Context) (*exec.Cmd, error)
	GracefulStop(ctx context.Context, proc *os.Process) error
	GracePeriod() time.Duration
}

// Supervisor is PID 1 for a Plexus-owned game image. It boots the adapter's
// process from disk, recovers unexpected exits in-pod, and runs graceful stop
// on context cancellation (SIGTERM).
type Supervisor struct {
	Adapter              Adapter
	MaxProcessRecoveries int
	RecoverDelay         time.Duration
	Output               io.Writer
}

// Run starts the game and blocks until a graceful stop or recovery exhaustion.
// A nil error is a graceful exit (process exit code 0).
func (s Supervisor) Run(ctx context.Context) error {
	if s.Adapter == nil {
		return fmt.Errorf("game supervisor requires an adapter")
	}
	recoveries := s.MaxProcessRecoveries
	if recoveries < 0 {
		recoveries = 0
	}
	delay := s.RecoverDelay
	if delay <= 0 {
		delay = defaultRecoverDelay
	}

	for attempt := 1; ; attempt++ {
		if ctx.Err() != nil {
			s.log("stop requested before start")
			return nil
		}
		cmd, err := s.Adapter.Command(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("prepare %s: %w", s.Adapter.Name(), err)
		}
		if cmd.Stdout == nil {
			cmd.Stdout = s.writer()
		}
		if cmd.Stderr == nil {
			cmd.Stderr = s.writer()
		}
		if err := cmd.Start(); err != nil {
			return fmt.Errorf("start %s: %w", s.Adapter.Name(), err)
		}
		s.log("started %s pid=%d attempt=%d", s.Adapter.Name(), cmd.Process.Pid, attempt)

		waitErr := make(chan error, 1)
		go func() { waitErr <- cmd.Wait() }()

		select {
		case err := <-waitErr:
			if ctx.Err() != nil {
				s.log("game process exited during graceful stop")
				return nil
			}
			if attempt-1 >= recoveries {
				s.log("process recovery exhausted after %d attempts; exiting for reschedule", attempt)
				if err == nil {
					return fmt.Errorf("%s exited unexpectedly", s.Adapter.Name())
				}
				return fmt.Errorf("%s exited unexpectedly: %w", s.Adapter.Name(), err)
			}
			s.log("unexpected %s exit (recovery %d/%d): %v", s.Adapter.Name(), attempt, recoveries, err)
			timer := time.NewTimer(delay)
			select {
			case <-ctx.Done():
				timer.Stop()
				s.log("stop requested during process recovery")
				return nil
			case <-timer.C:
			}
		case <-ctx.Done():
			stopCtx, cancel := context.WithTimeout(context.Background(), s.stopBudget())
			stopErr := s.Adapter.GracefulStop(stopCtx, cmd.Process)
			exited, wait := waitWithContext(stopCtx, waitErr)
			cancel()
			if exited {
				s.log("graceful stop completed")
				return nil
			}
			if stopErr != nil {
				return fmt.Errorf("graceful stop failed: %w", stopErr)
			}
			return fmt.Errorf("graceful stop did not finish before the adapter grace period: %w", wait)
		}
	}
}

func (s Supervisor) stopBudget() time.Duration {
	grace := s.Adapter.GracePeriod()
	if grace <= 0 {
		return time.Second
	}
	const drain = 2 * time.Second
	if grace <= drain {
		return grace
	}
	return grace - drain
}

func (s Supervisor) log(format string, args ...any) {
	fmt.Fprintf(s.writer(), "game-supervisor: "+format+"\n", args...)
}

func (s Supervisor) writer() io.Writer {
	if s.Output != nil {
		return s.Output
	}
	return os.Stderr
}

func waitWithContext(ctx context.Context, waitErr <-chan error) (bool, error) {
	select {
	case err := <-waitErr:
		return true, err
	case <-ctx.Done():
		return false, ctx.Err()
	}
}
