package at

import "strings"

const maxLineBytes = 4096

type eventKind int

const (
	eventLine eventKind = iota
	eventPrompt
	eventOverflow
)

type event struct {
	kind eventKind
	line string
}

type framer struct {
	line     strings.Builder
	overflow bool
}

func (f *framer) feed(chunk []byte) []event {
	events := make([]event, 0, 4)
	for _, b := range chunk {
		if f.overflow {
			if b == '\n' {
				f.overflow = false
				f.line.Reset()
				events = append(events, event{kind: eventOverflow})
			}
			continue
		}
		switch b {
		case '\n':
			line := strings.TrimRight(f.line.String(), "\r")
			events = append(events, event{kind: eventLine, line: line})
			f.line.Reset()
		case '>':
			events = append(events, event{kind: eventPrompt})
			f.line.Reset()
		default:
			f.line.WriteByte(b)
			if f.line.Len() > maxLineBytes {
				f.overflow = true
				f.line.Reset()
			}
		}
	}
	return events
}

func terminalFinal(line string) string {
	trimmed := strings.TrimSpace(line)
	upper := strings.ToUpper(trimmed)
	switch {
	case upper == "OK":
		return "OK"
	case upper == "ERROR":
		return "ERROR"
	case strings.HasPrefix(upper, "+CME ERROR:"):
		return "+CME ERROR"
	case strings.HasPrefix(upper, "+CMS ERROR:"):
		return "+CMS ERROR"
	default:
		return ""
	}
}
