package modem

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"gsm/internal/at"
)

func newAPNTestController(t *testing.T, transport Transport) Controller {
	t.Helper()
	return NewController(transport, nil, Config{
		APN:          apnAuto,
		BaudRate:     115200,
		ModuleType:   "em05-ce",
		Logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		PollInterval: time.Millisecond,
		BusyInterval: time.Millisecond,
	})
}

// simTransport 模拟"卡已就绪 + 信号正常 + 指定 IMSI"的模块。
func simTransport(imsi string, commands *[]string) *fakeTransport {
	return &fakeTransport{
		do: func(cmd string) (at.Reply, error) {
			if commands != nil {
				*commands = append(*commands, cmd)
			}
			switch cmd {
			case "AT+CPIN?":
				return okReply("+CPIN: READY"), nil
			case "AT+CSQ":
				return okReply("+CSQ: 22,0"), nil
			case "AT+CIMI":
				if imsi == "" {
					return at.Reply{Lines: []string{"ERROR"}}, nil
				}
				return okReply(imsi), nil
			default:
				return okReply(), nil
			}
		},
	}
}

func TestOperatorFromIMSI(t *testing.T) {
	tests := []struct {
		imsi string
		want operatorClass
	}{
		{"460001234567890", operatorChinaMobile},
		{"460021234567890", operatorChinaMobile},
		{"460011234567890", operatorChinaUnicom},
		{"460111234567890", operatorChinaTelecom},
		{"460031234567890", operatorChinaTelecom},
		{"460991234567890", operatorOther},
		{"", operatorUnknown},
	}
	for _, tc := range tests {
		if got := operatorFromIMSI(tc.imsi); got != tc.want {
			t.Fatalf("operatorFromIMSI(%q)=%v want=%v", tc.imsi, got, tc.want)
		}
	}
}

func TestAutoAPNUsesCarrierDefault(t *testing.T) {
	tests := []struct {
		imsi string
		want string
	}{
		{"460001234567890", "cmnet"},
		{"460011234567890", "3gnet"},
		{"460111234567890", "ctnet"},
	}
	for _, tc := range tests {
		t.Run(tc.want, func(t *testing.T) {
			ctl := newAPNTestController(t, simTransport(tc.imsi, nil))
			if _, err := ctl.Status(context.Background()); err != nil {
				t.Fatal(err)
			}
			result, err := ctl.Status(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if result.Status != StatusOK {
				t.Fatalf("status=%v detail=%s", result.Status, result.Detail)
			}
			if got := ctl.(*controller).apnApplied; got != tc.want {
				t.Fatalf("applied APN=%q want=%q", got, tc.want)
			}
		})
	}
}

func TestAutoAPNAppliedOnlyOnce(t *testing.T) {
	var commands []string
	ctl := newAPNTestController(t, simTransport("460111234567890", &commands))
	for i := 0; i < 3; i++ {
		if _, err := ctl.Status(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	count := 0
	for _, cmd := range commands {
		if strings.HasPrefix(cmd, "AT+CGDCONT=1") {
			count++
			if !strings.Contains(cmd, "ctnet") {
				t.Fatalf("unexpected APN command: %s", cmd)
			}
		}
	}
	if count != 1 {
		t.Fatalf("APN command sent %d times, want 1 (raw calls: %v)", count, commands)
	}
}

func TestAutoAPNKeepsDefaultWhenIMSIMissing(t *testing.T) {
	ctl := newAPNTestController(t, simTransport("", nil))
	result, err := ctl.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != StatusOK {
		t.Fatalf("status=%v", result.Status)
	}
	if got := ctl.(*controller).apnApplied; got != "" {
		t.Fatalf("APN should not be applied, got %q", got)
	}
}

func TestExplicitAPNIsNotOverridden(t *testing.T) {
	transport := simTransport("460111234567890", nil)
	ctl := NewController(transport, nil, Config{
		APN:          "cmiot",
		BaudRate:     115200,
		ModuleType:   "em05-ce",
		Logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		PollInterval: time.Millisecond,
		BusyInterval: time.Millisecond,
	})
	if _, err := ctl.Status(context.Background()); err != nil {
		t.Fatal(err)
	}
	if transport.hasCall("AT+CIMI") {
		t.Fatal("explicit APN must not query IMSI")
	}
	if got := ctl.(*controller).apnApplied; got != "cmiot" {
		t.Fatalf("applied APN=%q want=cmiot", got)
	}
}

func TestIdentifyModuleFromL610FirmwareVersion(t *testing.T) {
	module, firmware, ok := identifyModule("16000.1000.00.06.74.05")
	if !ok || module != ModuleFibocom || firmware != FirmwareL610 {
		t.Fatalf("module=%v firmware=%v ok=%v", module, firmware, ok)
	}
}
