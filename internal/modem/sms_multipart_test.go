package modem

import (
	"context"
	"strings"
	"testing"
	"time"

	"gsm/internal/at"
)

// TestSendSMSMultipartSharesRefAndStopsOnFailure 覆盖长短信：多段共用一个 reference，
// 且某段失败后不再重试、不再继续发后面的段（失败文案要说明已提交几段）。
func TestSendSMSMultipartSharesRefAndStopsOnFailure(t *testing.T) {
	previousPause := segmentPause
	segmentPause = 0
	defer func() { segmentPause = previousPause }()

	submitted := 0
	transport := &fakeTransport{
		do: func(cmd string) (at.Reply, error) {
			switch cmd {
			case "AT+CPIN?":
				return okReply("+CPIN: READY"), nil
			case "AT+CSQ":
				return okReply("+CSQ: 24,0"), nil
			case "AT+CPAS":
				return okReply("+CPAS: 0"), nil
			default:
				return okReply(), nil
			}
		},
		sendRaw: func(data []byte) (at.Reply, error) {
			submitted++
			if submitted == 2 {
				// 第二段被模块拒绝：+CMS ERROR
				return at.Reply{Lines: []string{"+CMS ERROR: 500"}}, nil
			}
			return okReply("+CMGS: 1"), nil
		},
	}
	ctl := newTestController(t, transport, nil)

	result, err := ctl.SendSMS(context.Background(), "13612345678", strings.Repeat("啊", 71))
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != StatusOperationFailed {
		t.Fatalf("status=%v want OperationFailed", result.Status)
	}
	if !strings.Contains(result.Detail, "segment 2/2") {
		t.Fatalf("detail 应指出是第 2/2 段: %q", result.Detail)
	}
	if !strings.Contains(result.Detail, "前 1 段已提交") {
		t.Fatalf("detail 应说明已提交段数: %q", result.Detail)
	}
	// 严格两段：失败后不再重试、不再发第三段。
	if submitted != 2 {
		t.Fatalf("实际提交了 %d 段，应为 2 段（不重试）", submitted)
	}

	// 两段必须共用同一个 reference（UDH 05 00 03 <ref> <total> <seq>）。
	var refs []string
	for _, call := range transport.promptCalls {
		_ = call
	}
	for _, raw := range transport.rawCalls {
		pduHex := strings.TrimSuffix(strings.ToUpper(string(raw)), "\x1a")
		index := strings.Index(pduHex, "050003")
		if index < 0 {
			t.Fatalf("PDU 里没有 UDH: %s", pduHex)
		}
		refs = append(refs, pduHex[index+6:index+8])
	}
	if len(refs) != 2 || refs[0] != refs[1] {
		t.Fatalf("分段 reference 不一致: %v", refs)
	}
}

// TestSendSMSMultipartPacesSegments 段间要有间隔（运营商限速 + 模块缓冲）。
func TestSendSMSMultipartPacesSegments(t *testing.T) {
	previousPause := segmentPause
	segmentPause = 120 * time.Millisecond
	defer func() { segmentPause = previousPause }()

	transport := &fakeTransport{do: func(cmd string) (at.Reply, error) {
		switch cmd {
		case "AT+CPIN?":
			return okReply("+CPIN: READY"), nil
		case "AT+CSQ":
			return okReply("+CSQ: 24,0"), nil
		case "AT+CPAS":
			return okReply("+CPAS: 0"), nil
		default:
			return okReply(), nil
		}
	}}
	ctl := newTestController(t, transport, nil)

	started := time.Now()
	result, err := ctl.SendSMS(context.Background(), "13612345678", strings.Repeat("啊", 140))
	elapsed := time.Since(started)
	if err != nil || result.Status != StatusOK {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if result.Segments != 3 {
		t.Fatalf("segments=%d want 3", result.Segments)
	}
	if elapsed < 2*segmentPause {
		t.Fatalf("3 段之间应至少有 2 次间隔，实际耗时 %s", elapsed)
	}
}
