package modem

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"go-pess/platform/user/pess/gsm/internal/at"
)

type fakeTransport struct {
	mu          sync.Mutex
	doCalls     []string
	promptCalls []string
	rawCalls    [][]byte
	do          func(string) (at.Reply, error)
	sendRaw     func([]byte) (at.Reply, error)
}

func okReply(lines ...string) at.Reply {
	return at.Reply{Lines: append(lines, "OK")}
}

func (f *fakeTransport) Do(_ context.Context, cmd string) (at.Reply, error) {
	f.mu.Lock()
	f.doCalls = append(f.doCalls, cmd)
	handler := f.do
	f.mu.Unlock()
	if handler != nil {
		return handler(cmd)
	}
	return okReply(), nil
}

func (f *fakeTransport) WaitPrompt(_ context.Context, cmd string) error {
	f.mu.Lock()
	f.promptCalls = append(f.promptCalls, cmd)
	f.mu.Unlock()
	return nil
}

func (f *fakeTransport) SendRaw(_ context.Context, data []byte) (at.Reply, error) {
	f.mu.Lock()
	f.rawCalls = append(f.rawCalls, append([]byte(nil), data...))
	handler := f.sendRaw
	f.mu.Unlock()
	if handler != nil {
		return handler(data)
	}
	return okReply(), nil
}

func (f *fakeTransport) hasCall(value string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, call := range f.doCalls {
		if call == value {
			return true
		}
	}
	return false
}

type fakeReopener struct {
	calls int
	err   error
}

func (f *fakeReopener) Reopen(context.Context) error {
	f.calls++
	return f.err
}

func newTestController(t *testing.T, transport Transport, reopener Reopener) Controller {
	t.Helper()
	return NewController(transport, reopener, Config{
		APN:          "cmnet",
		BaudRate:     115200,
		ModuleType:   "em05-ce",
		Logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		PollInterval: time.Millisecond,
		BusyInterval: time.Millisecond,
	})
}

func TestStatusBranches(t *testing.T) {
	tests := []struct {
		name string
		do   func(string) (at.Reply, error)
		want Status
	}{
		{"no hardware", func(cmd string) (at.Reply, error) {
			if cmd == "AT" {
				return at.Reply{}, at.ErrPortDown
			}
			return okReply(), nil
		}, StatusNoHW},
		{"no card", func(cmd string) (at.Reply, error) {
			if cmd == "AT+CPIN?" {
				return okReply("+CPIN: NOT READY"), nil
			}
			return okReply(), nil
		}, StatusNoCard},
		{"weak signal", func(cmd string) (at.Reply, error) {
			if cmd == "AT+CSQ" {
				return okReply("+CSQ: 16,0"), nil
			}
			if cmd == "AT+CPIN?" {
				return okReply("+CPIN: READY"), nil
			}
			return okReply(), nil
		}, StatusNoSignal},
		{"ok", func(cmd string) (at.Reply, error) {
			if cmd == "AT+CSQ" {
				return okReply("+CSQ: 17,2"), nil
			}
			if cmd == "AT+CPIN?" {
				return okReply("+CPIN: READY"), nil
			}
			return okReply(), nil
		}, StatusOK},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			controller := newTestController(t, &fakeTransport{do: tc.do}, nil)
			result, err := controller.Status(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if result.Status != tc.want {
				t.Fatalf("status=%v want=%v detail=%s", result.Status, tc.want, result.Detail)
			}
		})
	}
}
func TestSendSMSSingleSegment(t *testing.T) {
	transport := &fakeTransport{do: func(cmd string) (at.Reply, error) {
		switch cmd {
		case "AT+CPIN?":
			return okReply("+CPIN: READY"), nil
		case "AT+CSQ":
			return okReply("+CSQ: 24,99"), nil
		case "AT+CPAS":
			return okReply("+CPAS: 0"), nil
		default:
			return okReply(), nil
		}
	}}
	controller := newTestController(t, transport, nil)
	result, err := controller.SendSMS(context.Background(), "13612345678", "你好")
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != StatusOK || result.Segments != 1 {
		t.Fatalf("result=%+v", result)
	}
	if len(transport.promptCalls) != 1 || transport.promptCalls[0] != "AT+CMGS=19" {
		t.Fatalf("prompt calls=%v", transport.promptCalls)
	}
	if len(transport.rawCalls) != 1 || !strings.HasSuffix(string(transport.rawCalls[0]), "\x1a") {
		t.Fatalf("raw calls=%q", transport.rawCalls)
	}
}

func TestSendSMSBusy(t *testing.T) {
	transport := &fakeTransport{do: func(cmd string) (at.Reply, error) {
		switch cmd {
		case "AT+CPIN?":
			return okReply("+CPIN: READY"), nil
		case "AT+CSQ":
			return okReply("+CSQ: 24,0"), nil
		case "AT+CPAS":
			return okReply("+CPAS: 4"), nil
		default:
			return okReply(), nil
		}
	}}
	controller := newTestController(t, transport, nil)
	result, err := controller.SendSMS(context.Background(), "13612345678", "hello")
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != StatusBusy {
		t.Fatalf("result=%+v", result)
	}
	if len(transport.promptCalls) != 0 {
		t.Fatal("busy request should not submit SMS")
	}
}
func TestSendVoiceSuccessAlwaysHangsUp(t *testing.T) {
	transport := &fakeTransport{do: func(cmd string) (at.Reply, error) {
		switch cmd {
		case "AT+CPIN?":
			return okReply("+CPIN: READY"), nil
		case "AT+CSQ":
			return okReply("+CSQ: 24,0"), nil
		case "AT+CPAS":
			return okReply("+CPAS: 0"), nil
		case "AT+CLCC":
			return okReply("+CLCC: 1,0,0,0,0"), nil
		case "AT+QWTTS?":
			return okReply("+QWTTS: 0"), nil
		default:
			return okReply(), nil
		}
	}}
	controller := newTestController(t, transport, nil)
	result, err := controller.SendVoice(context.Background(), "13612345678", "告警测试")
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != StatusOK || !transport.hasCall("ATH") {
		t.Fatalf("result=%+v calls=%v", result, transport.doCalls)
	}
	if !transport.hasCall(`AT+QWTTS=1,1,1,"544A8B666D4B8BD5"`) {
		t.Fatalf("unexpected QWTTS call: %v", transport.doCalls)
	}
}

func TestSendVoiceCancellationHangsUp(t *testing.T) {
	transport := &fakeTransport{do: func(cmd string) (at.Reply, error) {
		switch cmd {
		case "AT+CPIN?":
			return okReply("+CPIN: READY"), nil
		case "AT+CSQ":
			return okReply("+CSQ: 24,0"), nil
		case "AT+CPAS":
			return okReply("+CPAS: 0"), nil
		case "AT+CLCC":
			return okReply(), nil
		default:
			return okReply(), nil
		}
	}}
	controller := newTestController(t, transport, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	_, err := controller.SendVoice(ctx, "13612345678", "test")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error=%v, want deadline exceeded", err)
	}
	if !transport.hasCall("ATH") {
		t.Fatalf("hangup missing after cancellation: %v", transport.doCalls)
	}
}

func TestVoiceUnsupportedByFibocom(t *testing.T) {
	transport := &fakeTransport{}
	controller := NewController(transport, nil, Config{
		APN: "cmnet", BaudRate: 115200, ModuleType: "fibocom",
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	_, err := controller.SendVoice(context.Background(), "13612345678", "test")
	if !errors.Is(err, ErrVoiceUnsupported) {
		t.Fatalf("error=%v", err)
	}
	if len(transport.doCalls) != 0 {
		t.Fatalf("unsupported voice should not touch modem: %v", transport.doCalls)
	}
}
func TestSetEnabledFalseResetsAndDisconnects(t *testing.T) {
	transport := &fakeTransport{}
	reopener := &fakeReopener{}
	controller := newTestController(t, transport, reopener)
	result, err := controller.SetEnabled(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != StatusDisabled || reopener.calls != 1 {
		t.Fatalf("result=%+v reopens=%d", result, reopener.calls)
	}
	for _, command := range []string{"AT^reset", `AT+cgdcont=1,"IP","cmnet"`, "AT+qnetdevctl=1,1,1", "AT+ipr=115200", "AT+cgact=0,1"} {
		if !transport.hasCall(command) {
			t.Fatalf("missing call %q in %v", command, transport.doCalls)
		}
	}
}

func TestAutoModuleType(t *testing.T) {
	path := filepath.Join(t.TempDir(), "4g_version")
	if err := os.WriteFile(path, []byte("EM05CNFDR08A04"), 0o600); err != nil {
		t.Fatal(err)
	}
	controller := NewController(&fakeTransport{}, nil, Config{
		APN: "cmnet", BaudRate: 115200, ModuleType: "auto", VersionFile: path,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	result, err := controller.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if result.HWType != "em05-cn" || result.Firmware != FirmwareA04 {
		t.Fatalf("identity=%q/%q", result.HWType, result.Firmware)
	}
}

func TestFibocomRedialUsesGTRNDIS(t *testing.T) {
	transport := &fakeTransport{}
	controller := NewController(transport, nil, Config{
		APN: "ctnet", BaudRate: 115200, ModuleType: "fibocom-l610",
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err := controller.Redial(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !transport.hasCall("AT+GTRNDIS=0,1") || !transport.hasCall("AT+GTRNDIS=1,1") {
		t.Fatalf("L610 redial did not use GTRNDIS: %v", transport.doCalls)
	}
	if transport.hasCall("AT+cgact=0,1") || transport.hasCall("AT+cgact=1,1") {
		t.Fatalf("L610 redial used Quectel CGACT: %v", transport.doCalls)
	}
}

func TestFibocomResetUsesCFUN15(t *testing.T) {
	transport := &fakeTransport{}
	reopener := &fakeReopener{}
	controller := NewController(transport, reopener, Config{
		APN: "ctnet", BaudRate: 115200, ModuleType: "fibocom-l610",
		PollInterval: time.Millisecond, BusyInterval: time.Millisecond,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	result, err := controller.SetEnabled(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != StatusDisabled || reopener.calls != 1 {
		t.Fatalf("result=%+v reopens=%d", result, reopener.calls)
	}
	if !transport.hasCall("AT+CFUN=15") || transport.hasCall("AT^reset") {
		t.Fatalf("unexpected L610 reset commands: %v", transport.doCalls)
	}
	if transport.hasCall("AT+qnetdevctl=1,1,1") {
		t.Fatalf("L610 must not receive Quectel qnetdevctl: %v", transport.doCalls)
	}
}

func TestEM05ResetFallsBackToCFUN(t *testing.T) {
	transport := &fakeTransport{do: func(cmd string) (at.Reply, error) {
		if cmd == "AT^reset" {
			return at.Reply{}, &at.TerminalError{Final: "ERROR", Line: "ERROR"}
		}
		return okReply(), nil
	}}
	reopener := &fakeReopener{}
	controller := newTestController(t, transport, reopener)
	if _, err := controller.SetEnabled(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	if !transport.hasCall("AT+CFUN=1,1") || reopener.calls != 1 {
		t.Fatalf("fallback reset not used: calls=%v reopens=%d", transport.doCalls, reopener.calls)
	}
}

func TestAutoDetectsFirmwareFromATIdentity(t *testing.T) {
	transport := &fakeTransport{do: func(cmd string) (at.Reply, error) {
		switch cmd {
		case "ATI":
			return okReply("Quectel EM05-CN"), nil
		case "AT+CGMM":
			return okReply("EM05-CN"), nil
		case "AT+CGMR":
			return okReply("EM05CNFDR08A04M1G_ND_BETA250904"), nil
		case "AT+CPIN?":
			return okReply("+CPIN: READY"), nil
		case "AT+CSQ":
			return okReply("+CSQ: 24,0"), nil
		default:
			return okReply(), nil
		}
	}}
	controller := NewController(transport, nil, Config{
		APN: "cmnet", BaudRate: 115200, ModuleType: "auto",
		VersionFile: filepath.Join(t.TempDir(), "missing-version"),
		Logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	result, err := controller.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if result.HWType != ModuleEM05CN || result.Firmware != FirmwareA04 {
		t.Fatalf("detected identity = %s/%s", result.HWType, result.Firmware)
	}
}

func TestEM05CNa03RejectsUnicomVoice(t *testing.T) {
	transport := &fakeTransport{do: func(cmd string) (at.Reply, error) {
		switch cmd {
		case "AT+CPIN?":
			return okReply("+CPIN: READY"), nil
		case "AT+CSQ":
			return okReply("+CSQ: 24,0"), nil
		case "AT+COPS?":
			return okReply(`+COPS: 0,0,"CHN-UNICOM",7`), nil
		case "AT+CPAS":
			return okReply("+CPAS: 0"), nil
		default:
			return okReply(), nil
		}
	}}
	controller := NewController(transport, nil, Config{
		APN: "cmnet", BaudRate: 115200, ModuleType: "em05-cn-a03",
		PollInterval: time.Millisecond, BusyInterval: time.Millisecond,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	_, err := controller.SendVoice(context.Background(), "13612345678", "test")
	if !errors.Is(err, ErrVoiceUnsupported) {
		t.Fatalf("error=%v, want ErrVoiceUnsupported", err)
	}
	for _, call := range transport.doCalls {
		if strings.HasPrefix(call, "ATD") {
			t.Fatalf("A03 Unicom voice should not dial: %v", transport.doCalls)
		}
	}
}

func TestEM05CNa04AllowsUnicomVoice(t *testing.T) {
	transport := &fakeTransport{do: func(cmd string) (at.Reply, error) {
		switch cmd {
		case "AT+CPIN?":
			return okReply("+CPIN: READY"), nil
		case "AT+CSQ":
			return okReply("+CSQ: 24,0"), nil
		case "AT+COPS?":
			return okReply(`+COPS: 0,0,"CHN-UNICOM",7`), nil
		case "AT+CPAS":
			return okReply("+CPAS: 0"), nil
		case "AT+CLCC":
			return okReply("+CLCC: 1,0,0,0,0"), nil
		case "AT+QWTTS?":
			return okReply("+QWTTS: 0"), nil
		default:
			return okReply(), nil
		}
	}}
	controller := NewController(transport, nil, Config{
		APN: "cmnet", BaudRate: 115200, ModuleType: "em05-cn-a04",
		PollInterval: time.Millisecond, BusyInterval: time.Millisecond,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	result, err := controller.SendVoice(context.Background(), "13612345678", "test")
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != StatusOK || !transport.hasCall("ATH") {
		t.Fatalf("result=%+v calls=%v", result, transport.doCalls)
	}
}

func TestEM05CERejectsTelecomVoice(t *testing.T) {
	transport := &fakeTransport{do: func(cmd string) (at.Reply, error) {
		switch cmd {
		case "AT+CPIN?":
			return okReply("+CPIN: READY"), nil
		case "AT+CSQ":
			return okReply("+CSQ: 24,0"), nil
		case "AT+COPS?":
			return okReply(`+COPS: 0,0,"CHN-CT",7`), nil
		default:
			return okReply(), nil
		}
	}}
	controller := newTestController(t, transport, nil)
	_, err := controller.SendVoice(context.Background(), "13612345678", "test")
	if !errors.Is(err, ErrVoiceUnsupported) {
		t.Fatalf("error=%v, want ErrVoiceUnsupported", err)
	}
}

func TestFibocomSMSSkipsQuectelStorageCommand(t *testing.T) {
	transport := &fakeTransport{do: func(cmd string) (at.Reply, error) {
		switch cmd {
		case "AT+CPIN?":
			return okReply("+CPIN: READY"), nil
		case "AT+CSQ":
			return okReply("+CSQ: 24,0"), nil
		case "AT+CPAS":
			return okReply("+CPAS: 0"), nil
		case "AT+COPS?":
			return okReply(`+COPS: 0,0,"CHN-CT",7`), nil
		default:
			return okReply(), nil
		}
	}}
	controller := NewController(transport, nil, Config{
		APN: "ctnet", BaudRate: 115200, ModuleType: "fibocom-l610",
		PollInterval: time.Millisecond, BusyInterval: time.Millisecond,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	result, err := controller.SendSMS(context.Background(), "13612345678", "hello")
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != StatusOK || len(transport.promptCalls) != 1 {
		t.Fatalf("result=%+v prompts=%v", result, transport.promptCalls)
	}
	for _, call := range transport.doCalls {
		if strings.HasPrefix(call, "AT+QCFG=") || call == "AT+CPAS" {
			t.Fatalf("L610 must not receive Quectel-specific or unverified busy commands: %v", transport.doCalls)
		}
	}
}

// usbnetTransport 模拟模块对 AT+QCFG="usbnet" 的应答，其余命令一律 OK。
func usbnetTransport(usbnet string) *fakeTransport {
	return &fakeTransport{do: func(cmd string) (at.Reply, error) {
		if strings.EqualFold(strings.TrimSpace(cmd), `AT+QCFG="usbnet"`) {
			return okReply(`+QCFG: "usbnet",` + usbnet), nil
		}
		return okReply(), nil
	}}
}

func TestExternalDataModeSkipsATDialCommands(t *testing.T) {
	transport := &fakeTransport{}
	controller := NewController(transport, &fakeReopener{}, Config{
		APN: "cmnet", BaudRate: 115200, ModuleType: "em05-ce", DataMode: "external",
		PollInterval: time.Millisecond, BusyInterval: time.Millisecond,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if _, err := controller.SetEnabled(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	for _, command := range []string{"AT+cgact=0,1", "AT+qnetdevctl=1,1,1"} {
		if transport.hasCall(command) {
			t.Fatalf("external data mode must not send %q: %v", command, transport.doCalls)
		}
	}
}

func TestAutoDataModeDetectsQMIAndSkipsDial(t *testing.T) {
	transport := usbnetTransport("0")
	controller := NewController(transport, nil, Config{
		APN: "cmnet", BaudRate: 115200, ModuleType: "em05-ce", DataMode: "auto",
		PollInterval: time.Millisecond, BusyInterval: time.Millisecond,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if _, err := controller.Status(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !transport.hasCall(`AT+QCFG="usbnet"`) {
		t.Fatalf("data mode was not probed: %v", transport.doCalls)
	}
	if err := controller.Redial(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, command := range []string{"AT+cgact=0,1", "AT+cgact=1,1"} {
		if transport.hasCall(command) {
			t.Fatalf("QMI mode must not send AT data commands, got %q in %v", command, transport.doCalls)
		}
	}
}

func TestAutoDataModeKeepsATDialWhenNotQMI(t *testing.T) {
	transport := usbnetTransport("1")
	controller := NewController(transport, nil, Config{
		APN: "cmnet", BaudRate: 115200, ModuleType: "em05-ce", DataMode: "auto",
		PollInterval: time.Millisecond, BusyInterval: time.Millisecond,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if _, err := controller.Status(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := controller.Redial(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !transport.hasCall("AT+cgact=1,1") {
		t.Fatalf("ECM mode should keep AT data dialing: %v", transport.doCalls)
	}
}

func ttsTransport(currentLibrary string) *fakeTransport {
	return &fakeTransport{do: func(cmd string) (at.Reply, error) {
		switch strings.ToUpper(strings.TrimSpace(cmd)) {
		case "AT+CPIN?":
			return okReply("+CPIN: READY"), nil
		case "AT+CSQ":
			return okReply("+CSQ: 22,99"), nil
		case "AT+QTTSETUP=2,3":
			return okReply("+QTTSETUP: 2,3," + currentLibrary), nil
		}
		return okReply(), nil
	}}
}

func TestTTSLibrarySwitchedWhenNotChinese(t *testing.T) {
	transport := ttsTransport("1")
	controller := newTestController(t, transport, nil)
	if _, err := controller.Status(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !transport.hasCall("AT+QTTSETUP=1,3,0") {
		t.Fatalf("english TTS library was not switched to chinese: %v", transport.doCalls)
	}
}

func TestTTSLibraryChineseNotTouched(t *testing.T) {
	transport := ttsTransport("0")
	controller := newTestController(t, transport, nil)
	if _, err := controller.Status(context.Background()); err != nil {
		t.Fatal(err)
	}
	if transport.hasCall("AT+QTTSETUP=1,3,0") {
		t.Fatalf("chinese TTS library must not be reconfigured: %v", transport.doCalls)
	}
}

// identityTransport 模拟一个 EM05-CE 模块：ATI 带 Revision、CPIN 就绪、信号正常。
func identityTransport() *fakeTransport {
	return &fakeTransport{do: func(cmd string) (at.Reply, error) {
		switch strings.ToUpper(strings.TrimSpace(cmd)) {
		case "ATI":
			return okReply("Quectel", "EM05", "Revision: EM05CEFCR06A06M1G_ND"), nil
		case "AT+CGMM":
			return okReply("EM05"), nil
		case "AT+CGMR":
			return okReply("EM05CEFCR06A06M1G_ND"), nil
		case "AT+CPIN?":
			return okReply("+CPIN: READY"), nil
		case "AT+CSQ":
			return okReply("+CSQ: 22,99"), nil
		}
		return okReply(), nil
	}}
}

func TestVersionFileRewrittenFromAT(t *testing.T) {
	path := filepath.Join(t.TempDir(), "4g_version")
	// 换过模块的场景：文件里还留着上一块 EM05-CN 的版本号。
	if err := os.WriteFile(path, []byte("EM05CNFDR08A04M1G\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	controller := NewController(identityTransport(), nil, Config{
		APN: "cmnet", BaudRate: 115200, ModuleType: "auto", VersionFile: path, VersionFileWrite: true,
		PollInterval: time.Millisecond, BusyInterval: time.Millisecond,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	result, err := controller.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if result.HWType != ModuleEM05CE {
		t.Fatalf("hw_type = %q, want %q（AT 实测应覆盖过期的版本文件）", result.HWType, ModuleEM05CE)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(content)); got != "EM05CEFCR06A06M1G_ND" {
		t.Fatalf("version file = %q, want the revision reported by AT", got)
	}
}

func TestVersionFileWriteCanBeDisabled(t *testing.T) {
	path := filepath.Join(t.TempDir(), "4g_version")
	if err := os.WriteFile(path, []byte("EM05CNFDR08A04M1G\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	controller := NewController(identityTransport(), nil, Config{
		APN: "cmnet", BaudRate: 115200, ModuleType: "auto", VersionFile: path, VersionFileWrite: false,
		PollInterval: time.Millisecond, BusyInterval: time.Millisecond,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if _, err := controller.Status(context.Background()); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(content)); got != "EM05CNFDR08A04M1G" {
		t.Fatalf("version file was modified although writes are disabled: %q", got)
	}
}

func TestExplicitModuleTypeIsNotOverriddenByAT(t *testing.T) {
	path := filepath.Join(t.TempDir(), "4g_version")
	controller := NewController(identityTransport(), nil, Config{
		APN: "cmnet", BaudRate: 115200, ModuleType: "em05-cn-a04", VersionFile: path, VersionFileWrite: false,
		PollInterval: time.Millisecond, BusyInterval: time.Millisecond,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	result, err := controller.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if result.HWType != ModuleEM05CN || result.Firmware != FirmwareA04 {
		t.Fatalf("explicit module.type was overridden: %q/%q", result.HWType, result.Firmware)
	}
}
