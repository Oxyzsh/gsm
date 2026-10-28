package config

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type Duration time.Duration

func (d *Duration) UnmarshalYAML(value *yaml.Node) error {
	var raw string
	if err := value.Decode(&raw); err != nil {
		return err
	}
	parsed, err := time.ParseDuration(raw)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", raw, err)
	}
	*d = Duration(parsed)
	return nil
}

func (d Duration) Std() time.Duration {
	return time.Duration(d)
}

type Config struct {
	Serial    SerialConfig    `yaml:"serial"`
	Module    ModuleConfig    `yaml:"module"`
	APN       string          `yaml:"apn"`
	Keepalive KeepaliveConfig `yaml:"keepalive"`
	GRPC      GRPCConfig      `yaml:"grpc"`
	Log       LogConfig       `yaml:"log"`
}

type SerialConfig struct {
	Device   string `yaml:"device"`
	BaudRate int    `yaml:"baud_rate"`
}

type ModuleConfig struct {
	Type        string `yaml:"type"`
	VersionFile string `yaml:"version_file"`
	// DataMode 决定数据链路由谁拨：
	//   auto     —— 读 AT+QCFG="usbnet"，QMI(0) 视为外部拨号工具负责，其余走 AT 上下行；
	//   at       —— 强制由告警服务用 AT+cgact 之类命令做数据上下行；
	//   external —— 强制外部拨号工具（如 quectel-CM）负责，本服务不发任何拨号命令。
	DataMode string `yaml:"data_mode"`
}

type KeepaliveConfig struct {
	Enabled         bool     `yaml:"enabled"`
	CheckInterval   Duration `yaml:"check_interval"`
	RetryInterval   Duration `yaml:"retry_interval"`
	RedialThreshold int      `yaml:"redial_threshold"`
	ResetThreshold  int      `yaml:"reset_threshold"`
}

type GRPCConfig struct {
	Listen string `yaml:"listen"`
}

type LogConfig struct {
	Level  string `yaml:"level"`
	Format string `yaml:"format"`
}

func Default() Config {
	return Config{
		Serial: SerialConfig{BaudRate: 115200},
		Module: ModuleConfig{Type: "auto", VersionFile: "/etc/mmdc/4g_version", DataMode: "auto"},
		Keepalive: KeepaliveConfig{
			Enabled:         true,
			CheckInterval:   Duration(300 * time.Second),
			RetryInterval:   Duration(10 * time.Second),
			RedialThreshold: 5,
			ResetThreshold:  10,
		},
		GRPC: GRPCConfig{Listen: "127.0.0.1:50051"},
		Log:  LogConfig{Level: "info", Format: "text"},
	}
}

func Load(path string) (Config, error) {
	file, err := os.Open(path)
	if err != nil {
		return Config{}, fmt.Errorf("open config %q: %w", path, err)
	}
	defer file.Close()

	cfg := Default()
	decoder := yaml.NewDecoder(file)
	decoder.KnownFields(true)
	if err := decoder.Decode(&cfg); err != nil {
		return Config{}, fmt.Errorf("decode config %q: %w", path, err)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return Config{}, fmt.Errorf("decode config %q: multiple YAML documents are not supported", path)
		}
		return Config{}, fmt.Errorf("decode config %q: %w", path, err)
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func (c Config) Validate() error {
	if strings.TrimSpace(c.Serial.Device) == "" {
		return errors.New("serial.device is required")
	}
	if c.Serial.BaudRate < 110 || c.Serial.BaudRate > 4_000_000 {
		return fmt.Errorf("serial.baud_rate %d is outside 110..4000000", c.Serial.BaudRate)
	}
	switch strings.ToLower(strings.TrimSpace(c.Module.Type)) {
	case "auto", "em05-ce", "em05-cn", "em05-cn-a03", "em05-cn-a04", "fibocom", "fibocom-l610", "l610":
	default:
		return fmt.Errorf("module.type %q must be auto, em05-ce, em05-cn, em05-cn-a03, em05-cn-a04, fibocom, or fibocom-l610", c.Module.Type)
	}
	if strings.EqualFold(c.Module.Type, "auto") && strings.TrimSpace(c.Module.VersionFile) == "" {
		return errors.New("module.version_file is required when module.type is auto")
	}
	switch strings.ToLower(strings.TrimSpace(c.Module.DataMode)) {
	case "auto", "at", "external":
	default:
		return fmt.Errorf("module.data_mode %q must be auto, at, or external", c.Module.DataMode)
	}
	if strings.TrimSpace(c.APN) == "" {
		return errors.New("apn is required")
	}
	if c.Keepalive.CheckInterval.Std() < time.Second {
		return errors.New("keepalive.check_interval must be at least 1s")
	}
	if c.Keepalive.RetryInterval.Std() < time.Second {
		return errors.New("keepalive.retry_interval must be at least 1s")
	}
	if c.Keepalive.RedialThreshold < 1 {
		return errors.New("keepalive.redial_threshold must be at least 1")
	}
	if c.Keepalive.ResetThreshold < 1 {
		return errors.New("keepalive.reset_threshold must be at least 1")
	}
	host, portText, err := net.SplitHostPort(c.GRPC.Listen)
	if err != nil {
		return fmt.Errorf("grpc.listen %q is invalid: %w", c.GRPC.Listen, err)
	}
	_ = host
	port, err := strconv.Atoi(portText)
	if err != nil || port < 1 || port > 65535 {
		return fmt.Errorf("grpc.listen port %q is outside 1..65535", portText)
	}
	switch c.Log.Level {
	case "debug", "info", "warn", "error":
	default:
		return fmt.Errorf("log.level %q must be debug, info, warn, or error", c.Log.Level)
	}
	switch c.Log.Format {
	case "text", "json":
	default:
		return fmt.Errorf("log.format %q must be text or json", c.Log.Format)
	}
	return nil
}
