package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "gsm.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadDefaultsAndDurations(t *testing.T) {
	path := writeConfig(t, `
serial:
  device: /dev/ttyUSB2
module:
  type: auto
  version_file: /etc/mmdc/4g_version
apn: cmnet
grpc:
  listen: 127.0.0.1:50051
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Serial.BaudRate != 115200 || !cfg.Keepalive.Enabled {
		t.Fatalf("defaults not applied: %+v", cfg)
	}
	if cfg.Keepalive.CheckInterval.Std() != 300*time.Second {
		t.Fatalf("check interval = %v", cfg.Keepalive.CheckInterval.Std())
	}
}

func TestLoadRejectsUnknownField(t *testing.T) {
	path := writeConfig(t, `
serial:
  device: /dev/ttyUSB2
  typo_baud: 9600
apn: cmnet
`)
	_, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), "typo_baud") {
		t.Fatalf("expected unknown-field error, got %v", err)
	}
}

func TestDurationRejectsBadValue(t *testing.T) {
	path := writeConfig(t, `
serial:
  device: /dev/ttyUSB2
apn: cmnet
keepalive:
  check_interval: soon
`)
	_, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), "invalid duration") {
		t.Fatalf("expected duration error, got %v", err)
	}
}

func TestValidation(t *testing.T) {
	cfg := Default()
	cfg.Serial.Device = "/dev/ttyUSB2"
	cfg.APN = "cmnet"
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	cfg.GRPC.Listen = "127.0.0.1:0"
	if err := cfg.Validate(); err == nil {
		t.Fatal("port zero should be rejected")
	}
	cfg = Default()
	cfg.Serial.Device = "/dev/ttyUSB2"
	cfg.APN = "cmnet"
	cfg.Module.Type = "unsupported"
	if err := cfg.Validate(); err == nil {
		t.Fatal("unknown module type should be rejected")
	}
}

func TestModuleTypeAliases(t *testing.T) {
	for _, moduleType := range []string{"em05-ce", "em05-cn", "em05-cn-a03", "em05-cn-a04", "fibocom", "fibocom-l610"} {
		cfg := Default()
		cfg.Serial.Device = "/dev/ttyUSB2"
		cfg.APN = "cmnet"
		cfg.Module.Type = moduleType
		if err := cfg.Validate(); err != nil {
			t.Fatalf("module.type %q rejected: %v", moduleType, err)
		}
	}
}
