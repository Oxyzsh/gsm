package at

import (
	"errors"
	"fmt"
)

var (
	ErrTimeout     = errors.New("AT command timeout")
	ErrPortDown    = errors.New("serial port is down")
	ErrClosed      = errors.New("AT session is closed")
	ErrDeviceBusy  = errors.New("AT device is held by another process")
)

type TerminalError struct {
	Final string
	Line  string
}

func (e *TerminalError) Error() string {
	if e.Line == "" {
		return e.Final
	}
	return fmt.Sprintf("%s: %s", e.Final, e.Line)
}

type Reply struct {
	Lines  []string
	Raw    string
	_final string
}

func (r Reply) Final() string {
	if r._final != "" {
		return r._final
	}
	for _, line := range r.Lines {
		if final := terminalFinal(line); final != "" {
			return final
		}
	}
	return ""
}

func (r Reply) OK() bool {
	return r.Final() == "OK"
}

func (r Reply) Contains(value string) bool {
	for _, line := range r.Lines {
		if containsFold(line, value) {
			return true
		}
	}
	return containsFold(r.Raw, value)
}

func containsFold(value, part string) bool {
	if len(part) == 0 {
		return true
	}
	for i := 0; i+len(part) <= len(value); i++ {
		if equalFoldASCII(value[i:i+len(part)], part) {
			return true
		}
	}
	return false
}

func equalFoldASCII(left, right string) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		a, b := left[i], right[i]
		if a >= 'a' && a <= 'z' {
			a -= 'a' - 'A'
		}
		if b >= 'a' && b <= 'z' {
			b -= 'a' - 'A'
		}
		if a != b {
			return false
		}
	}
	return true
}
