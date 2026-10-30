package keepalive

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"gsm/internal/modem"
)

type fakeController struct {
	mu       sync.Mutex
	statuses []modem.Status
	redials  int
	reinit   int
}

func (f *fakeController) Status(context.Context) (modem.StatusResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	status := modem.StatusOK
	if len(f.statuses) > 0 {
		status = f.statuses[0]
		f.statuses = f.statuses[1:]
	}
	return modem.StatusResult{Status: status}, nil
}

func (f *fakeController) SetEnabled(context.Context, bool) (modem.StatusResult, error) {
	return modem.StatusResult{}, nil
}
func (f *fakeController) SendSMS(context.Context, string, string) (modem.SendResult, error) {
	return modem.SendResult{}, nil
}
func (f *fakeController) SendVoice(context.Context, string, string) (modem.StatusResult, error) {
	return modem.StatusResult{}, nil
}
func (f *fakeController) Redial(context.Context) error {
	f.mu.Lock()
	f.redials++
	f.mu.Unlock()
	return nil
}
func (f *fakeController) Reinitialize(context.Context) error {
	f.mu.Lock()
	f.reinit++
	f.mu.Unlock()
	return nil
}

func testRunner(controller modem.Controller) *Runner {
	return New(controller, Config{
		Enabled: true, CheckInterval: 10 * time.Millisecond, RetryInterval: time.Millisecond,
		RedialThreshold: 2, ResetThreshold: 2,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
}

func TestRunnerUsesCheckIntervalAndResetsOnRecovery(t *testing.T) {
	controller := &fakeController{statuses: []modem.Status{modem.StatusNoSignal, modem.StatusOK}}
	runner := testRunner(controller)
	runner.last = modem.StatusNoHW
	runner.step(context.Background())
	if runner.interval != 10*time.Millisecond || runner.last != modem.StatusNoSignal {
		t.Fatalf("after no signal: interval=%v last=%v", runner.interval, runner.last)
	}
	runner.step(context.Background())
	if runner.interval != 10*time.Millisecond || runner.last != modem.StatusOK || runner.redialCount != 0 {
		t.Fatalf("after recovery: interval=%v last=%v redial=%d", runner.interval, runner.last, runner.redialCount)
	}
}

func TestRunnerRecoveryBranchThresholds(t *testing.T) {
	controller := &fakeController{statuses: []modem.Status{modem.StatusOperationFailed, modem.StatusOperationFailed, modem.StatusOperationFailed, modem.StatusOperationFailed}}
	runner := testRunner(controller)
	runner.redialCount = 3
	runner.step(context.Background())
	if controller.reinit != 1 || runner.resetCount != 1 || runner.redialCount != 0 {
		t.Fatalf("first threshold: reinit=%d reset=%d redial=%d", controller.reinit, runner.resetCount, runner.redialCount)
	}
	runner.step(context.Background())
	if controller.redials != 1 {
		t.Fatalf("expected a redial, got %d", controller.redials)
	}
	runner.resetCount = 3
	runner.step(context.Background())
	if !runner.exhaustedLogged {
		t.Fatal("exhaustion should be logged")
	}
	runner.step(context.Background())
	if controller.redials != 1 || controller.reinit != 1 {
		t.Fatal("exhausted runner must not perform more actions")
	}
}

func TestRunnerDisabledDoesNotRun(t *testing.T) {
	controller := &fakeController{statuses: []modem.Status{modem.StatusNoSignal}}
	runner := New(controller, Config{Enabled: false, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	runner.Run(ctx)
	controller.mu.Lock()
	defer controller.mu.Unlock()
	if len(controller.statuses) != 1 {
		t.Fatal("disabled runner should not query status")
	}
}
