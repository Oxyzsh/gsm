package pdu

import (
	"errors"
	"strings"
	"testing"
)

func TestNormalizePhone(t *testing.T) {
	tests := map[string]string{
		"13612345678":     "8613612345678",
		"+8613612345678":  "8613612345678",
		"08613612345678":  "8613612345678",
		"008613612345678": "8613612345678",
		"8613612345678":   "8613612345678",
	}
	for input, want := range tests {
		got, err := NormalizePhone(input)
		if err != nil || got != want {
			t.Fatalf("NormalizePhone(%q) = %q, %v; want %q", input, got, err, want)
		}
	}
	for _, input := range []string{"", "1361234567", "+1 555 0100", "86136123456X"} {
		if _, err := NormalizePhone(input); !errors.Is(err, ErrInvalidPhone) {
			t.Fatalf("NormalizePhone(%q) error = %v, want ErrInvalidPhone", input, err)
		}
	}
}

func TestVoicePhone(t *testing.T) {
	if got, err := NormalizeVoicePhone("13612345678"); err != nil || got != "13612345678" {
		t.Fatalf("valid voice number: %q %v", got, err)
	}
	if _, err := NormalizeVoicePhone("+8613612345678"); !errors.Is(err, ErrInvalidPhone) {
		t.Fatalf("international voice number should fail: %v", err)
	}
}

func TestSMSGoldenPDU(t *testing.T) {
	tests := []struct {
		text    string
		wantPDU string
	}{
		{"你好", "0001000D91683116325476F80008044F60597D"},
		{"OK", "0001000D91683116325476F8000804004F004B"},
	}
	for _, tc := range tests {
		pdus, err := EncodeSMS("13612345678", tc.text)
		if err != nil {
			t.Fatal(err)
		}
		if len(pdus) != 1 || pdus[0] != tc.wantPDU {
			t.Fatalf("EncodeSMS(%q) = %#v, want %q", tc.text, pdus, tc.wantPDU)
		}
		length, err := CMGSLength(pdus[0])
		if err != nil || length != 18 {
			t.Fatalf("CMGSLength = %d, %v; want 18", length, err)
		}
	}
}

func TestSMSBoundaries(t *testing.T) {
	seventy := repeatRune('啊', 70)
	pdus, err := EncodeSMS("13612345678", seventy)
	if err != nil || len(pdus) != 1 || parseUDL(t, pdus[0]) != 140 {
		t.Fatalf("70-char SMS: segments=%d err=%v", len(pdus), err)
	}

	seventyOne := repeatRune('啊', 71)
	pdus, err = encodeSMS("13612345678", seventyOne, 0x2A)
	if err != nil || len(pdus) != 2 {
		t.Fatalf("71-char SMS: segments=%d err=%v", len(pdus), err)
	}
	if parseUDL(t, pdus[0]) != 140 || parseUDL(t, pdus[1]) != 14 {
		t.Fatalf("unexpected multipart UDL: %v", pdus)
	}
	// UDH：05 00 03 <ref> <total> <seq>，同一条长短信所有段共用一个 ref。
	if !contains(pdus[0], "0500032A0201") || !contains(pdus[1], "0500032A0202") {
		t.Fatalf("unexpected multipart UDH: %v", pdus)
	}

	if pdus, err = EncodeSMS("13612345678", repeatRune('啊', 1072)); err != nil || len(pdus) != 16 {
		t.Fatalf("1072-char SMS: segments=%d err=%v", len(pdus), err)
	}
	if _, err = EncodeSMS("13612345678", repeatRune('啊', 1073)); !errors.Is(err, ErrTextTooLong) {
		t.Fatalf("1073-char SMS error = %v, want ErrTextTooLong", err)
	}
}

// TestConcatReferenceIsPerMessageUnique 保证连续两条长短信不会共用同一个 ref：
// 共用时收端会把两条的片段当成同一条混拼（串行/丢字/整条不显示）。
func TestConcatReferenceIsPerMessageUnique(t *testing.T) {
	text := repeatRune('啊', 140) // 3 段
	first, err := EncodeSMS("13612345678", text)
	if err != nil {
		t.Fatal(err)
	}
	second, err := EncodeSMS("13612345678", text)
	if err != nil {
		t.Fatal(err)
	}

	firstRef := extractConcatRef(t, first[0])
	secondRef := extractConcatRef(t, second[0])
	if firstRef == secondRef {
		t.Fatalf("两条长短信共用了 ref=%s", firstRef)
	}
	for _, pduHex := range first {
		if got := extractConcatRef(t, pduHex); got != firstRef {
			t.Fatalf("同一条长短信的 ref 不一致: %s vs %s", got, firstRef)
		}
	}
}

// extractConcatRef 从 PDU 里取 8-bit concat 的 reference number。
func extractConcatRef(t *testing.T, pduHex string) string {
	t.Helper()
	marker := "050003"
	index := strings.Index(pduHex, marker)
	if index < 0 {
		t.Fatalf("PDU 里找不到 UDH: %s", pduHex)
	}
	start := index + len(marker)
	return pduHex[start : start+2]
}

func TestEncodeUCS2DropsNonBMP(t *testing.T) {
	encoded, count, err := EncodeUCS2("A😀你")
	if err != nil {
		t.Fatal(err)
	}
	if count != 2 || string(encoded) != "\x00A\x4F\x60" {
		t.Fatalf("encoded=%X count=%d", encoded, count)
	}
	if _, _, err := EncodeUCS2("😀"); !errors.Is(err, ErrInvalidText) {
		t.Fatalf("emoji-only error = %v", err)
	}
}

func TestEncodeTTSLimit(t *testing.T) {
	if _, err := EncodeTTS(repeatRune('啊', 480)); err != nil {
		t.Fatalf("480 TTS characters: %v", err)
	}
	if _, err := EncodeTTS(repeatRune('啊', 481)); !errors.Is(err, ErrTextTooLong) {
		t.Fatalf("481 TTS characters error = %v, want ErrTextTooLong", err)
	}
}

func repeatRune(r rune, n int) string {
	result := make([]rune, n)
	for i := range result {
		result[i] = r
	}
	return string(result)
}

func parseUDL(t *testing.T, pduHex string) int {
	t.Helper()
	// SMSC(2) + first-octet(2) + MR(2) + DA-len(2) +
	// DA-length TP-DA bytes + PID/DCS/VP(6).
	daLength, err := hexByte(pduHex[6:8])
	if err != nil {
		t.Fatal(err)
	}
	offset := 10 + ((int(daLength)+1)/2)*2 + 4
	udl, err := hexByte(pduHex[offset : offset+2])
	if err != nil {
		t.Fatal(err)
	}
	return int(udl)
}

func hexByte(s string) (byte, error) {
	var value byte
	for i := 0; i < len(s); i++ {
		value <<= 4
		switch {
		case s[i] >= '0' && s[i] <= '9':
			value += s[i] - '0'
		case s[i] >= 'A' && s[i] <= 'F':
			value += s[i] - 'A' + 10
		default:
			return 0, errors.New("invalid hex")
		}
	}
	return value, nil
}

func contains(value, part string) bool {
	for i := 0; i+len(part) <= len(value); i++ {
		if value[i:i+len(part)] == part {
			return true
		}
	}
	return false
}

func TestCMGSLengthExcludesSMSCField(t *testing.T) {
	cases := []struct {
		pdu  string
		want int
	}{
		{"0001000B917195569060F200080A00680065006C006C006F", 23},
		{"0011000D91689171335642F40009AA0A00680065006C006C006F", 25},
	}
	for _, tc := range cases {
		got, err := CMGSLength(tc.pdu)
		if err != nil || got != tc.want {
			t.Fatalf("CMGSLength(%q) = %d, %v; want %d", tc.pdu, got, err, tc.want)
		}
	}
}
