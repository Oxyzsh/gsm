package modem

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"go-pess/platform/user/pess/gsm/internal/at"
)

func newVoiceTestController(t *testing.T, transport Transport, moduleType string) Controller {
	t.Helper()
	return NewController(transport, nil, Config{
		APN:          "auto",
		BaudRate:     115200,
		ModuleType:   moduleType,
		Logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		PollInterval: time.Millisecond,
		BusyInterval: time.Millisecond,
	})
}

// callTransport 模拟"卡正常 + 拨号后立即接通"的模块。
func callTransport(activeCallLine string, ttsCommand string, ttsProgress string) *fakeTransport {
	return &fakeTransport{do: func(cmd string) (at.Reply, error) {
		switch {
		case cmd == "AT+CPIN?":
			return okReply("+CPIN: READY"), nil
		case cmd == "AT+CSQ":
			return okReply("+CSQ: 24,0"), nil
		case cmd == "AT+CPAS":
			return okReply("+CPAS: 0"), nil
		case cmd == "AT+COPS?":
			return okReply(`+COPS: 0,0,"CHN-UNICOM",7`), nil
		case cmd == "AT+CLCC":
			return okReply(activeCallLine), nil
		case cmd == "AT+QWTTS?" || cmd == "AT+GTTS?":
			return okReply(ttsProgress), nil
		default:
			return okReply(), nil
		}
	}}
}

// L610 的 AT+GTTS 参数未确认前，语音通知应当明确拒绝而不是"响铃不出声"。
func TestSendVoiceFibocomRejectedUntilGTTSConfirmed(t *testing.T) {
	transport := callTransport(`+CLCC: 1,0,0,0,0,"13612345678",129`, "", "+GTTS: 0")
	ctl := newVoiceTestController(t, transport, ModuleFibocom)

	_, err := ctl.SendVoice(context.Background(), "13612345678", "测试")
	if err == nil {
		t.Fatal("L610 在 GTTS 参数确认前应拒绝语音")
	}
	if len(transport.doCalls) != 0 {
		t.Fatalf("拒绝时不应碰模块: %v", transport.doCalls)
	}
}

// 方言到 AT 命令的映射（不依赖硬件）：参数含义确认后只需打开 profile 开关。
func TestVoiceTTSCommandStyles(t *testing.T) {
	if got := voiceTTSCommand(TTSStyleQWTTS, "4F60597D", "测试"); got != `AT+QWTTS=1,1,1,"4F60597D"` {
		t.Fatalf("qwt ts 命令 = %q", got)
	}
	if got := voiceTTSCommand(TTSStyleGTTS, "4F60597D", "测试"); got != `AT+GTTS=1,"测试",0` {
		t.Fatalf("gtts 命令 = %q", got)
	}
}

func TestSendVoiceQuectelStillUsesQWTTS(t *testing.T) {
	transport := callTransport(`+CLCC: 1,0,0,0,0,"13612345678",129`, "", "+QWTTS: 0")
	ctl := newVoiceTestController(t, transport, ModuleEM05CN)

	result, err := ctl.SendVoice(context.Background(), "13612345678", "测试")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if result.Status != StatusOK {
		t.Fatalf("status=%v detail=%s", result.Status, result.Detail)
	}
	if !hasPrefixCall(transport.doCalls, "AT+QWTTS=1,1,1,") {
		t.Fatalf("移远应发 QWTTS 命令: %v", transport.doCalls)
	}
	if hasPrefixCall(transport.doCalls, "AT+GTTS=1") {
		t.Fatalf("移远不应发广和通的 GTTS 命令: %v", transport.doCalls)
	}
}

func TestSendVoiceUnknownModuleIsRejected(t *testing.T) {
	ctl := newVoiceTestController(t, callTransport("", "", ""), "unknown-module")
	_, err := ctl.SendVoice(context.Background(), "13612345678", "测试")
	if err == nil {
		t.Fatal("未知模块应拒绝语音")
	}
}

func TestSanitizeTTSText(t *testing.T) {
	got, err := sanitizeTTSText("第一行\n第二行 \"带引号\" 和\\反斜杠")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if strings.ContainsAny(got, "\r\n") || strings.Contains(got, "\"") {
		t.Fatalf("应清成单行且去掉引号: %q", got)
	}
	if _, err := sanitizeTTSText("   "); err == nil {
		t.Fatal("空白文本应报错")
	}
	if _, err := sanitizeTTSText(strings.Repeat("啊", 481)); err == nil {
		t.Fatal("超过 480 字应报错")
	}
}

func hasPrefixCall(calls []string, prefix string) bool {
	for _, call := range calls {
		if strings.HasPrefix(call, prefix) {
			return true
		}
	}
	return false
}

func TestTTSIdlePatterns(t *testing.T) {
	if !qwttsPattern.MatchString("+QWTTS: 0") {
		t.Fatal("QWTTS idle pattern did not match")
	}
	if !gttsIdlePattern.MatchString("+GTTS: 0") {
		t.Fatal("GTTS idle pattern did not match")
	}
}

func TestHasActiveCallForNumberIgnoresPhantomEntries(t *testing.T) {
	lines := []string{
		`+CLCC: 1,1,0,1,0,"",128`,
		`+CLCC: 2,1,0,1,0,"",128`,
		`+CLCC: 3,0,2,0,0,"19974960989",129`,
	}
	if hasActiveCallForNumber(lines, "19974960989") {
		t.Fatal("dialing call must not be treated as answered")
	}
	lines[2] = `+CLCC: 3,0,0,0,0,"19974960989",129`
	if !hasActiveCallForNumber(lines, "19974960989") {
		t.Fatal("matching outgoing active call was not detected")
	}
	if hasActiveCallForNumber(lines, "13612345678") {
		t.Fatal("different phone number must not match")
	}
}
