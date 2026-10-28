package pdu

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

var (
	ErrInvalidPhone = errors.New("invalid phone number")
	ErrInvalidText  = errors.New("invalid text")
	ErrTextTooLong  = errors.New("text too long")
)

var voicePhonePattern = regexp.MustCompile(`^[0-9]{11}$`)

// NormalizePhone normalizes a mainland China SMS destination to the 13-digit
// international form used by the C implementation.
func NormalizePhone(phone string) (string, error) {
	normalized := strings.TrimSpace(phone)
	normalized = strings.TrimPrefix(normalized, "+")
	if strings.HasPrefix(normalized, "0086") {
		normalized = normalized[1:]
	}
	if strings.HasPrefix(normalized, "086") {
		normalized = normalized[1:]
	}
	if normalized == "" {
		return "", fmt.Errorf("%w: empty", ErrInvalidPhone)
	}
	for _, r := range normalized {
		if r < '0' || r > '9' {
			return "", fmt.Errorf("%w: contains non-ASCII digit", ErrInvalidPhone)
		}
	}
	if len(normalized) == 11 {
		normalized = "86" + normalized
	}
	if len(normalized) != 13 || !strings.HasPrefix(normalized, "86") {
		return "", fmt.Errorf("%w: expected 11 or 13 digits with country code 86", ErrInvalidPhone)
	}
	return normalized, nil
}

// NormalizeVoicePhone preserves the legacy voice-dial rule: exactly eleven
// ASCII digits with no country-code conversion.
func NormalizeVoicePhone(phone string) (string, error) {
	normalized := strings.TrimSpace(phone)
	if !voicePhonePattern.MatchString(normalized) {
		return "", fmt.Errorf("%w: voice number must be exactly 11 digits", ErrInvalidPhone)
	}
	return normalized, nil
}
