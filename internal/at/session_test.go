package at

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakePort struct {
	mu     sync.Mutex
	cond   *sync.Cond
	buffer bytes.Buffer
	closed bool
	writes []string
}

func newFakePort() *fakePort {
	p := &fakePort{}
	p.cond = sync.NewCond(&p.mu)
	return p
}

func (p *fakePort) Read(dst []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for p.buffer.Len() == 0 && !p.closed {
		p.cond.Wait()
	}
	if p.buffer.Len() == 0 && p.closed {
		return 0, io.EOF
	}
	return p.buffer.Read(dst)
}

func (p *fakePort) Write(data []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return 0, io.ErrClosedPipe
	}
	command := string(data)
	p.writes = append(p.writes, command)
	trimmed := strings.TrimSpace(strings.TrimSuffix(command, "\r\n"))
	switch {
	case strings.HasSuffix(command, "\x1a"):
		p.buffer.WriteString("\r\n+CMGS: 1\r\n\r\nOK\r\n")
	case strings.HasPrefix(trimmed, "AT+CMGS="):
		p.buffer.WriteString("\r\n> ")
	case trimmed == "ATE0":
		p.buffer.WriteString("\r\nOK\r\n")
	case trimmed == "AT":
		p.buffer.WriteString("\r\nAT\r\n\r\nOK\r\n")
	case trimmed == "AT+ERROR":
		p.buffer.WriteString("\r\n+CME ERROR: 10\r\n")
	case trimmed == "AT+SLOW":
		time.AfterFunc(50*time.Millisecond, func() {
			p.mu.Lock()
			defer p.mu.Unlock()
			if !p.closed {
				p.buffer.WriteString("\r\nOK\r\n")
				p.cond.Broadcast()
			}
		})
	default:
		p.buffer.WriteString("\r\nOK\r\n")
	}
	p.cond.Broadcast()
	return len(data), nil
}

func (p *fakePort) Close() error {
	p.mu.Lock()
	p.closed = true
	p.cond.Broadcast()
	p.mu.Unlock()
	return nil
}

func (p *fakePort) SetReadTimeout(time.Duration) error { return nil }

func startSession(t *testing.T) (*Session, *fakePort) {
	t.Helper()
	port := newFakePort()
	session := New(Config{
		Device:   "fake",
		BaudRate: 115200,
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
	}, func() (Port, error) { return port, nil })
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		cancel()
		_ = session.Close()
	})
	go func() { _ = session.Run(ctx) }()
	deadline := time.Now().Add(time.Second)
	for {
		session.mu.Lock()
		ready := session.ready
		session.mu.Unlock()
		if ready {
			return session, port
		}
		if time.Now().After(deadline) {
			t.Fatal("session did not become ready")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestSessionDoIgnoresEchoAndReturnsPrecedingLines(t *testing.T) {
	session, _ := startSession(t)
	reply, err := session.Do(context.Background(), "AT")
	if err != nil {
		t.Fatal(err)
	}
	if !reply.OK() || reply.Final() != "OK" {
		t.Fatalf("reply=%+v final=%q", reply, reply.Final())
	}
}

func TestSessionTerminalError(t *testing.T) {
	session, _ := startSession(t)
	reply, err := session.Do(context.Background(), "AT+ERROR")
	var terminal *TerminalError
	if !errors.As(err, &terminal) || terminal.Final != "+CME ERROR" {
		t.Fatalf("error=%v reply=%+v", err, reply)
	}
	if !strings.Contains(reply.Raw, "+CME ERROR: 10") {
		t.Fatalf("raw reply lost terminal line: %q", reply.Raw)
	}
}

func TestSessionPromptAndRawSubmission(t *testing.T) {
	session, port := startSession(t)
	if err := session.WaitPrompt(context.Background(), "AT+CMGS=20"); err != nil {
		t.Fatal(err)
	}
	reply, err := session.SendRaw(context.Background(), []byte("0011\x1a"))
	if err != nil || !reply.OK() {
		t.Fatalf("SendRaw reply=%+v err=%v", reply, err)
	}
	port.mu.Lock()
	defer port.mu.Unlock()
	last := port.writes[len(port.writes)-1]
	if last != "0011\x1a" {
		t.Fatalf("raw write = %q, want no CRLF", last)
	}
}

func TestSessionTimeoutAndStaleReplyDrain(t *testing.T) {
	session, _ := startSession(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := session.Do(ctx, "AT+SLOW"); !errors.Is(err, ErrTimeout) {
		t.Fatalf("timeout error = %v", err)
	}
	time.Sleep(50 * time.Millisecond)
	reply, err := session.Do(context.Background(), "AT")
	if err != nil || !reply.OK() {
		t.Fatalf("next command polluted by stale reply: %+v %v", reply, err)
	}
}
