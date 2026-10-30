package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	gsmv1 "gsm/api/gen/gsm/v1"
	"gsm/internal/api"
	"gsm/internal/at"
	"gsm/internal/config"
	"gsm/internal/keepalive"
	"gsm/internal/modem"

	"google.golang.org/grpc"
	"google.golang.org/grpc/reflection"
)

var (
	version = "dev"
	commit  = "unknown"
	date    = "unknown"
)

func main() {
	os.Exit(run())
}

func run() int {
	configPath := flag.String("config", "/etc/gsm/gsm.yaml", "path to YAML configuration")
	debug := flag.Bool("debug", false, "override log level to debug")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Printf("gsmd %s (commit %s, built %s)\n", version, commit, date)
		return 0
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "configuration error: %v\n", err)
		return 2
	}
	level := new(slog.LevelVar)
	parsedLevel, err := parseLogLevel(cfg.Log.Level)
	if err != nil {
		fmt.Fprintf(os.Stderr, "configuration error: %v\n", err)
		return 2
	}
	level.Set(parsedLevel)
	if *debug {
		level.Set(slog.LevelDebug)
	}
	logger := newLogger(level, cfg.Log.Format)
	slog.SetDefault(logger)

	device, err := at.ResolveDevice(cfg.Serial.Device, logger)
	if err != nil {
		logger.Error("resolve AT port failed", "error", err, "configured", cfg.Serial.Device)
		return 1
	}
	cfg.Serial.Device = device

	session, err := at.Open(at.Config{
		Device:   cfg.Serial.Device,
		BaudRate: cfg.Serial.BaudRate,
		Logger:   logger,
	})
	if err != nil {
		logger.Error("create serial session failed", "error", err)
		return 1
	}

	rootCtx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	runCtx, cancelRun := context.WithCancel(rootCtx)
	defer cancelRun()

	sessionErr := make(chan error, 1)
	go func() {
		sessionErr <- session.Run(runCtx)
	}()

	controller := modem.NewController(session, session, modem.Config{
		APN:              cfg.APN,
		BaudRate:         cfg.Serial.BaudRate,
		ModuleType:       cfg.Module.Type,
		VersionFile:      cfg.Module.VersionFile,
		VersionFileWrite: cfg.Module.VersionFileWrite,
		DataMode:         cfg.Module.DataMode,
		Logger:           logger,
	})
	service := api.New(controller, level)

	listener, err := net.Listen("tcp", cfg.GRPC.Listen)
	if err != nil {
		logger.Error("gRPC listen failed", "address", cfg.GRPC.Listen, "error", err)
		cancelRun()
		_ = session.Close()
		return 1
	}
	grpcServer := grpc.NewServer()
	gsmv1.RegisterGsmServiceServer(grpcServer, service)
	if isLoopbackListen(cfg.GRPC.Listen) {
		reflection.Register(grpcServer)
	} else {
		logger.Warn("gRPC is listening on a non-loopback address without TLS or authentication", "address", cfg.GRPC.Listen)
	}

	serveErr := make(chan error, 1)
	go func() {
		serveErr <- grpcServer.Serve(listener)
	}()

	keepaliveRunner := keepalive.New(controller, keepalive.Config{
		Enabled:         cfg.Keepalive.Enabled,
		CheckInterval:   cfg.Keepalive.CheckInterval.Std(),
		RetryInterval:   cfg.Keepalive.RetryInterval.Std(),
		RedialThreshold: cfg.Keepalive.RedialThreshold,
		ResetThreshold:  cfg.Keepalive.ResetThreshold,
		Logger:          logger,
	})
	go keepaliveRunner.Run(runCtx)

	logger.Info("gsmd started", "version", version, "grpc", cfg.GRPC.Listen, "device", cfg.Serial.Device, "module_type", cfg.Module.Type)

	exitCode := 0
	select {
	case <-rootCtx.Done():
		logger.Info("shutdown signal received")
	case err := <-serveErr:
		if err != nil && !errors.Is(err, grpc.ErrServerStopped) {
			logger.Error("gRPC server stopped", "error", err)
			exitCode = 1
		}
	case err := <-sessionErr:
		if err != nil && !errors.Is(err, context.Canceled) {
			logger.Error("serial session stopped", "error", err)
			exitCode = 1
		}
	}

	service.BeginShutdown()
	cancelRun()
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()
	stopped := make(chan struct{})
	go func() {
		grpcServer.GracefulStop()
		close(stopped)
	}()
	select {
	case <-stopped:
	case <-shutdownCtx.Done():
		logger.Warn("gRPC graceful shutdown timed out; forcing stop")
		grpcServer.Stop()
	}
	if err := session.Close(); err != nil {
		logger.Warn("close serial session failed", "error", err)
	}
	logger.Info("gsmd stopped")
	return exitCode
}

func newLogger(level slog.Leveler, format string) *slog.Logger {
	options := &slog.HandlerOptions{Level: level}
	if strings.EqualFold(format, "json") {
		return slog.New(slog.NewJSONHandler(os.Stderr, options))
	}
	return slog.New(slog.NewTextHandler(os.Stderr, options))
}

func parseLogLevel(value string) (slog.Level, error) {
	switch strings.ToLower(value) {
	case "debug":
		return slog.LevelDebug, nil
	case "info":
		return slog.LevelInfo, nil
	case "warn":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return 0, fmt.Errorf("unsupported log level %q", value)
	}
}

func isLoopbackListen(address string) bool {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return false
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
