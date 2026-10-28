package keepalive

import (
	"context"
	"log/slog"
	"time"

	"go-pess/platform/user/pess/gsm/internal/modem"
)

type Config struct {
	Enabled         bool
	CheckInterval   time.Duration
	RetryInterval   time.Duration
	RedialThreshold int
	ResetThreshold  int
	InitialDelay    time.Duration
	Logger          *slog.Logger
}

type Runner struct {
	controller modem.Controller
	cfg        Config
	logger     *slog.Logger

	last            modem.Status
	redialCount     int
	resetCount      int
	exhaustedLogged bool
	interval        time.Duration
}

func New(controller modem.Controller, cfg Config) *Runner {
	if cfg.InitialDelay <= 0 {
		cfg.InitialDelay = 5 * time.Second
	}
	if cfg.CheckInterval <= 0 {
		cfg.CheckInterval = 300 * time.Second
	}
	if cfg.RetryInterval <= 0 {
		cfg.RetryInterval = 10 * time.Second
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	return &Runner{
		controller: controller,
		cfg:        cfg,
		logger:     cfg.Logger,
		last:       modem.StatusNoHW,
		interval:   cfg.InitialDelay,
	}
}

func (r *Runner) Run(ctx context.Context) {
	if !r.cfg.Enabled {
		return
	}
	for {
		if !wait(ctx, r.interval) {
			return
		}
		r.step(ctx)
	}
}

func (r *Runner) step(ctx context.Context) {
	result, err := r.controller.Status(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return
		}
		r.logger.Warn("GSM keepalive status check failed", "error", err)
		r.interval = r.cfg.RetryInterval
		return
	}
	status := result.Status
	r.logger.Debug("GSM keepalive status", "status", status.String(), "rssi", result.RSSI, "redial_count", r.redialCount, "reset_count", r.resetCount, "next_in", r.interval)

	// This ordering intentionally preserves the reviewed C behavior. Failure
	// states below StatusOK return here; the recovery branch remains reserved
	// for future failure states placed after StatusOK.
	if status < modem.StatusOK {
		if status != r.last {
			r.logStatusWarning(status)
			r.exhaustedLogged = false
		}
		r.last = status
		r.interval = r.cfg.CheckInterval
		return
	}

	failed := status != modem.StatusOK
	if failed {
		switch {
		case r.resetCount > r.cfg.ResetThreshold:
			if !r.exhaustedLogged {
				r.logger.Error("GSM auto recovery exhausted; manual intervention required", "status", status.String())
				r.exhaustedLogged = true
			}
			r.interval = r.cfg.CheckInterval
		case r.redialCount > r.cfg.RedialThreshold:
			r.redialCount = 0
			r.resetCount++
			r.logger.Info("GSM keepalive reinitializing serial session", "reset_count", r.resetCount)
			if err := r.controller.Reinitialize(ctx); err != nil && ctx.Err() == nil {
				r.logger.Warn("GSM keepalive reinitialization failed", "error", err)
			}
			r.interval = r.cfg.RetryInterval
		default:
			r.redialCount++
			r.logger.Info("GSM keepalive redialing", "redial_count", r.redialCount)
			if err := r.controller.Redial(ctx); err != nil && ctx.Err() == nil {
				r.logger.Warn("GSM keepalive redial failed", "error", err)
			}
			r.interval = r.cfg.RetryInterval
		}
	} else {
		if r.last != modem.StatusOK {
			r.logger.Warn("GSM module recovered to OK")
		}
		r.redialCount = 0
		r.resetCount = 0
		r.exhaustedLogged = false
		r.interval = r.cfg.CheckInterval
	}
	r.last = status
}

func (r *Runner) logStatusWarning(status modem.Status) {
	switch status {
	case modem.StatusNoHW:
		r.logger.Warn("GSM module is not found.")
	case modem.StatusNoCard:
		r.logger.Warn("SIM card is not inserted.")
	case modem.StatusNoSignal:
		r.logger.Warn("GSM signal is too weak.")
	case modem.StatusDisabled:
		r.logger.Info("GSM module is disabled.")
	default:
		r.logger.Warn("GSM module status is abnormal", "status", status.String())
	}
}

func wait(ctx context.Context, duration time.Duration) bool {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
