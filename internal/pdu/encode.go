package pdu

import (
	"encoding/hex"
	"fmt"
	"strings"
	"unicode/utf8"

	"golang.org/x/text/encoding/unicode"
	"golang.org/x/text/transform"
)

const (
	MaxSMSCharacters = 1072
	MaxTTSCharacters = 480
	singleSMSBytes   = 140
	concatSMSBytes   = 134
	maxSegments      = 16
)

// EncodeUCS2 converts UTF-8 to big-endian UCS2. Non-BMP characters are
// dropped, matching the legacy UCS-2//IGNORE behavior. The second return value
// is the number of characters retained.
func EncodeUCS2(text string) ([]byte, int, error) {
	if text == "" {
		return nil, 0, fmt.Errorf("%w: empty", ErrInvalidText)
	}
	if !utf8.ValidString(text) {
		return nil, 0, fmt.Errorf("%w: malformed UTF-8", ErrInvalidText)
	}

	var b strings.Builder
	retained := 0
	for _, r := range text {
		if r > 0xFFFF {
			continue
		}
		b.WriteRune(r)
		retained++
	}
	if retained == 0 {
		return nil, 0, fmt.Errorf("%w: no UCS2 characters remain", ErrInvalidText)
	}

	encoder := unicode.UTF16(unicode.BigEndian, unicode.IgnoreBOM).NewEncoder()
	encoded, _, err := transform.Bytes(encoder, []byte(b.String()))
	if err != nil {
		return nil, 0, fmt.Errorf("encode UCS2: %w", err)
	}
	return encoded, retained, nil
}

// EncodeTTS returns the uppercase UCS2 hex payload used by AT+QWTTS.
func EncodeTTS(text string) (string, error) {
	encoded, characters, err := EncodeUCS2(text)
	if err != nil {
		return "", err
	}
	if characters > MaxTTSCharacters {
		return "", fmt.Errorf("%w: %d BMP characters exceeds %d", ErrTextTooLong, characters, MaxTTSCharacters)
	}
	return strings.ToUpper(hex.EncodeToString(encoded)), nil
}

// EncodeSMS builds one or more complete SMS-SUBMIT PDUs for a destination.
func EncodeSMS(phone, text string) ([]string, error) {
	normalized, err := NormalizePhone(phone)
	if err != nil {
		return nil, err
	}
	encoded, characters, err := EncodeUCS2(text)
	if err != nil {
		return nil, err
	}
	if characters > MaxSMSCharacters {
		return nil, fmt.Errorf("%w: %d BMP characters exceeds %d", ErrTextTooLong, characters, MaxSMSCharacters)
	}

	if len(encoded) <= singleSMSBytes {
		return []string{buildPDU(normalized, encoded, 0, 0)}, nil
	}

	total := (len(encoded) + concatSMSBytes - 1) / concatSMSBytes
	if total > maxSegments {
		return nil, fmt.Errorf("%w: %d segments exceeds %d", ErrTextTooLong, total, maxSegments)
	}
	pdus := make([]string, 0, total)
	for sequence := 1; sequence <= total; sequence++ {
		start := (sequence - 1) * concatSMSBytes
		end := start + concatSMSBytes
		if end > len(encoded) {
			end = len(encoded)
		}
		pdus = append(pdus, buildPDU(normalized, encoded[start:end], total, sequence))
	}
	return pdus, nil
}

// CMGSLength returns the TP length argument for AT+CMGS, excluding SMSC.
func CMGSLength(pduHex string) (int, error) {
	if len(pduHex) < 4 || len(pduHex)%2 != 0 {
		return 0, fmt.Errorf("invalid PDU hex length %d", len(pduHex))
	}
	smscLength, err := hex.DecodeString(pduHex[:2])
	if err != nil {
		return 0, fmt.Errorf("decode SMSC length: %w", err)
	}
	totalBytes := len(pduHex) / 2
	smscFieldBytes := 1 + int(smscLength[0])
	if totalBytes <= smscFieldBytes {
		return 0, fmt.Errorf("PDU has %d bytes but SMSC field occupies %d bytes", totalBytes, smscFieldBytes)
	}
	return totalBytes - smscFieldBytes, nil
}

func buildPDU(phone string, payload []byte, total, sequence int) string {
	var b strings.Builder
	b.WriteString("00")
	userData := payload
	if total > 0 {
		b.WriteString("41")
	} else {
		b.WriteString("01")
	}
	b.WriteString("00")
	fmt.Fprintf(&b, "%02X", len(phone))
	b.WriteString("91")
	b.WriteString(bcdNumber(phone))
	b.WriteString("00")
	b.WriteString("08")

	if total > 0 {
		header := []byte{0x05, 0x00, 0x03, 0x01, byte(total), byte(sequence)}
		userData = make([]byte, 0, len(header)+len(payload))
		userData = append(userData, header...)
		userData = append(userData, payload...)
	}
	fmt.Fprintf(&b, "%02X", len(userData))
	b.WriteString(strings.ToUpper(hex.EncodeToString(userData)))
	return b.String()
}

func bcdNumber(number string) string {
	var b strings.Builder
	for i := 0; i < len(number); i += 2 {
		left := number[i]
		right := byte('F')
		if i+1 < len(number) {
			right = number[i+1]
		}
		b.WriteByte(right)
		b.WriteByte(left)
	}
	return b.String()
}

// DroppedNonBMP returns the number of non-BMP runes that UCS2 conversion
// will discard.
func DroppedNonBMP(text string) int {
	dropped := 0
	for _, r := range text {
		if r > 0xFFFF {
			dropped++
		}
	}
	return dropped
}
