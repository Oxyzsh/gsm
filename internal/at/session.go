package at

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"time"

	"go.bug.st/serial"
)

const (
	defaultReadTimeout = 200 * time.Millisecond
	defaultCommandWait = 3 * time.Second
)

type Config struct {
	Device      string
	BaudRate    int
	ReadTimeout time.Duration
	Logger      *slog.Logger
}

type Port interface {
	io.ReadWriteCloser
	SetReadTimeout(time.Duration) error
}

type Opener func() (Port, error)

type Session struct {
	cfg    Config
	opener Opener

	mu      sync.Mutex
	conn    *connection
	ready   bool
	closed  bool
	changed chan struct{}
	cancel  context.CancelFunc

	commands   chan struct{}
	runOnce    sync.Once
	done       chan struct{}
	closeOnce  sync.Once
	finishOnce sync.Once
}

type connection struct {
	port   Port
	events chan event
	dead   chan struct{}
	once   sync.Once
	errMu  sync.Mutex
	err    error
}

func Open(cfg Config) (*Session, error) {
	return New(cfg, func() (Port, error) {
		mode := &serial.Mode{
			BaudRate: cfg.BaudRate,
			DataBits: 8,
			Parity:   serial.NoParity,
			StopBits: serial.OneStopBit,
		}
		port, err := serial.Open(cfg.Device, mode)
		if err != nil {
			return nil, err
		}
		return port, nil
	}), nil
}

func New(cfg Config, opener Opener) *Session {
	if cfg.ReadTimeout <= 0 {
		cfg.ReadTimeout = defaultReadTimeout
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	return &Session{
		cfg:      cfg,
		opener:   opener,
		changed:  make(chan struct{}),
		commands: make(chan struct{}, 1),
		done:     make(chan struct{}),
	}
}

func (s *Session) Run(ctx context.Context) error {
	var started bool
	s.runOnce.Do(func() { started = true })
	if !started {
		return errors.New("AT session Run may only be called once")
	}

	runCtx, cancel := context.WithCancel(ctx)
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		cancel()
		s.finish()
		return nil
	}
	s.cancel = cancel
	s.mu.Unlock()
	defer func() {
		cancel()
		s.finish()
	}()

	backoff := time.Second
	for {
		if runCtx.Err() != nil {
			return nil
		}

		port, err := s.opener()
		if err != nil {
			s.cfg.Logger.Warn("serial open failed", "device", s.cfg.Device, "error", err, "retry_in", backoff)
			if !waitContext(runCtx, backoff) {
				return nil
			}
			backoff = nextBackoff(backoff)
			continue
		}
		if err := port.SetReadTimeout(s.cfg.ReadTimeout); err != nil {
			_ = port.Close()
			s.cfg.Logger.Warn("set serial read timeout failed", "error", err, "retry_in", backoff)
			if !waitContext(runCtx, backoff) {
				return nil
			}
			backoff = nextBackoff(backoff)
			continue
		}

		conn := newConnection(port)
		if !s.installConnection(conn) {
			conn.fail(ErrClosed)
			return nil
		}
		go s.readLoop(conn)

		initCtx, initCancel := context.WithTimeout(runCtx, defaultCommandWait)
		_, err = s.doOnConnection(initCtx, conn, "ATE0", true)
		initCancel()
		if err != nil {
			s.cfg.Logger.Warn("serial reinitialization failed", "device", s.cfg.Device, "error", err)
			conn.fail(err)
			s.markDown(conn)
			if !waitContext(runCtx, backoff) {
				return nil
			}
			backoff = nextBackoff(backoff)
			continue
		}
		if !s.markReady(conn) {
			conn.fail(ErrClosed)
			return nil
		}
		s.cfg.Logger.Info("serial port ready", "device", s.cfg.Device)
		backoff = time.Second

		select {
		case <-runCtx.Done():
			conn.fail(runCtx.Err())
			s.markDown(conn)
			return nil
		case <-conn.dead:
			s.cfg.Logger.Warn("serial port disconnected", "device", s.cfg.Device, "error", conn.closeError())
			s.markDown(conn)
			if !waitContext(runCtx, backoff) {
				return nil
			}
		}
	}
}

func (s *Session) Do(ctx context.Context, cmd string) (Reply, error) {
	conn, err := s.readyConnection()
	if err != nil {
		return Reply{}, err
	}
	return s.doOnConnection(ctx, conn, cmd, true)
}

func (s *Session) WaitPrompt(ctx context.Context, cmd string) error {
	conn, err := s.readyConnection()
	if err != nil {
		return err
	}
	if err := s.acquireCommand(ctx); err != nil {
		return err
	}
	defer s.releaseCommand()
	if !s.connectionReady(conn) {
		return ErrPortDown
	}
	s.drain(conn)
	if err := s.writeLine(conn, cmd); err != nil {
		return err
	}
	for {
		select {
		case ev := <-conn.events:
			switch ev.kind {
			case eventPrompt:
				return nil
			case eventOverflow:
				continue
			case eventLine:
				if strings.EqualFold(strings.TrimSpace(ev.line), cmd) {
					continue
				}
				if final := terminalFinal(ev.line); final != "" {
					return &TerminalError{Final: final, Line: strings.TrimSpace(ev.line)}
				}
			}
		case <-conn.dead:
			return ErrPortDown
		case <-ctx.Done():
			return contextError(ctx, cmd)
		}
	}
}

func (s *Session) SendRaw(ctx context.Context, data []byte) (Reply, error) {
	conn, err := s.readyConnection()
	if err != nil {
		return Reply{}, err
	}
	return s.doOnConnection(ctx, conn, string(data), false)
}

func (s *Session) Reopen(ctx context.Context) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return ErrClosed
	}
	previous := s.conn
	s.mu.Unlock()
	if previous != nil {
		previous.fail(errors.New("serial reopen requested"))
	}
	for {
		s.mu.Lock()
		ready := s.ready && s.conn != nil && s.conn != previous
		closed := s.closed
		changed := s.changed
		s.mu.Unlock()
		if closed {
			return ErrClosed
		}
		if ready {
			return nil
		}
		select {
		case <-ctx.Done():
			return contextError(ctx, "serial reopen")
		case <-changed:
		}
	}
}

func (s *Session) Close() error {
	var closeErr error
	s.closeOnce.Do(func() {
		s.mu.Lock()
		s.closed = true
		conn := s.conn
		s.conn = nil
		s.ready = false
		cancel := s.cancel
		s.notifyLocked()
		s.mu.Unlock()
		if cancel != nil {
			cancel()
		}
		if conn != nil {
			closeErr = conn.fail(ErrClosed)
		}
	})
	select {
	case <-s.done:
	case <-time.After(2 * time.Second):
	}
	s.finish()
	return closeErr
}

func (s *Session) doOnConnection(ctx context.Context, conn *connection, payload string, appendNewline bool) (Reply, error) {
	if err := s.acquireCommand(ctx); err != nil {
		return Reply{}, err
	}
	defer s.releaseCommand()
	if !s.connectionCurrent(conn) {
		return Reply{}, ErrPortDown
	}
	s.drain(conn)
	if appendNewline {
		if err := s.writeLine(conn, payload); err != nil {
			return Reply{}, err
		}
	} else if err := writeAll(conn.port, []byte(payload)); err != nil {
		conn.fail(err)
		return Reply{}, ErrPortDown
	}

	reply := Reply{}
	for {
		select {
		case ev := <-conn.events:
			switch ev.kind {
			case eventPrompt:
				continue
			case eventOverflow:
				s.cfg.Logger.Warn("AT reply line exceeded limit")
				continue
			case eventLine:
				if strings.EqualFold(strings.TrimSpace(ev.line), strings.TrimSpace(payload)) {
					continue
				}
				appendRaw(&reply, ev.line)
				if final := terminalFinal(ev.line); final != "" {
					reply._final = final
					if final != "OK" {
						return reply, &TerminalError{Final: final, Line: strings.TrimSpace(ev.line)}
					}
					return reply, nil
				}
				reply.Lines = append(reply.Lines, ev.line)
			}
		case <-conn.dead:
			return reply, ErrPortDown
		case <-ctx.Done():
			return reply, contextError(ctx, payload)
		}
	}
}

func (s *Session) writeLine(conn *connection, cmd string) error {
	if err := writeAll(conn.port, []byte(cmd+"\r\n")); err != nil {
		conn.fail(err)
		return ErrPortDown
	}
	return nil
}

func (s *Session) readLoop(conn *connection) {
	framer := &framer{}
	buffer := make([]byte, 512)
	for {
		n, err := conn.port.Read(buffer)
		if n > 0 {
			for _, ev := range framer.feed(buffer[:n]) {
				select {
				case conn.events <- ev:
				case <-conn.dead:
					return
				default:
					s.cfg.Logger.Debug("dropping AT event because command queue is full")
				}
			}
		}
		if err != nil {
			if !errors.Is(err, io.EOF) && !strings.Contains(strings.ToLower(err.Error()), "closed") {
				conn.fail(err)
			} else {
				conn.fail(io.EOF)
			}
			return
		}
	}
}

func (s *Session) readyConnection() (*connection, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, ErrClosed
	}
	if !s.ready || s.conn == nil {
		return nil, ErrPortDown
	}
	return s.conn, nil
}

func (s *Session) connectionCurrent(conn *connection) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return !s.closed && s.conn == conn
}

func (s *Session) connectionReady(conn *connection) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return !s.closed && s.ready && s.conn == conn
}

func (s *Session) installConnection(conn *connection) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return false
	}
	s.conn = conn
	s.ready = false
	s.notifyLocked()
	return true
}

func (s *Session) markReady(conn *connection) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.conn != conn {
		return false
	}
	s.ready = true
	s.notifyLocked()
	return true
}

func (s *Session) markDown(conn *connection) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.conn == conn {
		s.conn = nil
		s.ready = false
		s.notifyLocked()
	}
}

func (s *Session) notifyLocked() {
	close(s.changed)
	s.changed = make(chan struct{})
}

func (s *Session) acquireCommand(ctx context.Context) error {
	select {
	case s.commands <- struct{}{}:
		return nil
	case <-ctx.Done():
		return contextError(ctx, "wait for AT command slot")
	}
}

func (s *Session) releaseCommand() {
	<-s.commands
}

func (s *Session) drain(conn *connection) {
	for {
		select {
		case ev := <-conn.events:
			s.cfg.Logger.Debug("discarding stale AT event", "kind", ev.kind, "line", ev.line)
		default:
			return
		}
	}
}

func (s *Session) finish() {
	s.finishOnce.Do(func() { close(s.done) })
}

func newConnection(port Port) *connection {
	return &connection{
		port:   port,
		events: make(chan event, 256),
		dead:   make(chan struct{}),
	}
}

func (c *connection) fail(err error) error {
	c.once.Do(func() {
		c.errMu.Lock()
		c.err = err
		c.errMu.Unlock()
		close(c.dead)
		_ = c.port.Close()
	})
	return nil
}

func (c *connection) closeError() error {
	c.errMu.Lock()
	defer c.errMu.Unlock()
	return c.err
}

func writeAll(writer io.Writer, data []byte) error {
	for len(data) > 0 {
		n, err := writer.Write(data)
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
		data = data[n:]
	}
	return nil
}

func appendRaw(reply *Reply, line string) {
	if reply.Raw != "" {
		reply.Raw += "\r\n"
	}
	reply.Raw += line
}

func contextError(ctx context.Context, operation string) error {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return fmt.Errorf("%w: %s: %w", ErrTimeout, operation, ctx.Err())
	}
	return ctx.Err()
}

func waitContext(ctx context.Context, duration time.Duration) bool {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func nextBackoff(current time.Duration) time.Duration {
	next := current * 2
	if next > 30*time.Second {
		return 30 * time.Second
	}
	return next
}
