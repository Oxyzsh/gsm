package at

import (
	"strings"
	"testing"
)

func TestFramerRecognizesLinesAndPrompt(t *testing.T) {
	f := &framer{}
	events := f.feed([]byte("\r\nOK\r\n+CLCC: 1,0,0\r\n> "))
	if len(events) != 4 {
		t.Fatalf("events = %#v", events)
	}
	if events[0].kind != eventLine || events[0].line != "" || events[1].line != "OK" {
		t.Fatalf("unexpected line events: %#v", events)
	}
	if events[2].line != "+CLCC: 1,0,0" || events[3].kind != eventPrompt {
		t.Fatalf("unexpected prompt events: %#v", events)
	}
}

func TestFramerOverflowRecoversAtNewline(t *testing.T) {
	f := &framer{}
	events := f.feed([]byte(strings.Repeat("x", maxLineBytes+1)))
	if len(events) != 0 {
		t.Fatalf("overflow before newline should not emit: %#v", events)
	}
	events = f.feed([]byte("tail\r\nOK\r\n"))
	if len(events) != 2 || events[0].kind != eventOverflow || events[1].line != "OK" {
		t.Fatalf("events after recovery = %#v", events)
	}
}
