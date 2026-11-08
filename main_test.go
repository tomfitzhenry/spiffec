package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

const (
	testWait       = 5 * time.Second
	noReturnWindow = 100 * time.Millisecond
)

type pipeConn struct {
	r               io.ReadCloser
	w               io.WriteCloser
	writeErr        error
	closeWriteCalls atomic.Int32
	closeWriteOnce  sync.Once
	closeWriteDone  chan struct{}
}

func (c *pipeConn) Read(p []byte) (int, error) {
	return c.r.Read(p)
}

func (c *pipeConn) Write(p []byte) (int, error) {
	if c.writeErr != nil {
		return 0, c.writeErr
	}
	return c.w.Write(p)
}

func (c *pipeConn) CloseWrite() error {
	c.closeWriteCalls.Add(1)
	c.closeWriteOnce.Do(func() { close(c.closeWriteDone) })
	return c.w.Close()
}

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

type errWriter struct{ err error }

func (w errWriter) Write(p []byte) (int, error) { return 0, w.err }

type pumpFixture struct {
	conn      *pipeConn
	stdin     io.Reader
	stdinW    *io.PipeWriter
	stdout    io.Writer
	toProxy   *io.PipeReader
	fromProxy *io.PipeWriter
}

func newPumpFixture(t *testing.T) *pumpFixture {
	t.Helper()

	stdinR, stdinW := io.Pipe()
	clientToServerR, clientToServerW := io.Pipe()
	serverToClientR, serverToClientW := io.Pipe()

	f := &pumpFixture{
		conn: &pipeConn{
			r:              serverToClientR,
			w:              clientToServerW,
			closeWriteDone: make(chan struct{}),
		},
		stdin:     stdinR,
		stdinW:    stdinW,
		stdout:    &syncBuffer{},
		toProxy:   clientToServerR,
		fromProxy: serverToClientW,
	}
	t.Cleanup(func() {
		_ = f.stdinW.Close()
		_ = f.toProxy.Close()
		_ = f.fromProxy.Close()
	})
	return f
}

func (f *pumpFixture) start() <-chan error {
	errCh := make(chan error, 1)
	go func() { errCh <- pump(f.conn, f.stdin, f.stdout) }()
	return errCh
}

type requestResult struct {
	data []byte
	err  error
}

func readProxyAsync(r io.Reader) <-chan requestResult {
	ch := make(chan requestResult, 1)
	go func() {
		data, err := io.ReadAll(r)
		ch <- requestResult{data: data, err: err}
	}()
	return ch
}

func awaitProxyRequest(t *testing.T, ch <-chan requestResult) []byte {
	t.Helper()
	select {
	case res := <-ch:
		if res.err != nil {
			t.Fatalf("reading forwarded stdin: %v", res.err)
		}
		return res.data
	case <-time.After(testWait):
		t.Fatal("timed out waiting for pump to forward stdin and half-close")
		return nil
	}
}

func awaitPump(t *testing.T, ch <-chan error) error {
	t.Helper()
	select {
	case err := <-ch:
		return err
	case <-time.After(testWait):
		t.Fatal("timed out waiting for pump to return")
		return nil
	}
}

func requireStillRunning(t *testing.T, ch <-chan error) {
	t.Helper()
	select {
	case err := <-ch:
		t.Fatalf("pump returned before conn->stdout ended: %v", err)
	case <-time.After(noReturnWindow):
	}
}

func TestPumpHalfClosesStdinAndDrainsResponse(t *testing.T) {
	f := newPumpFixture(t)
	pumpErr := f.start()

	proxyRead := readProxyAsync(f.toProxy)

	const command = "A1 NOOP\r\n"
	if _, err := io.WriteString(f.stdinW, command); err != nil {
		t.Fatalf("writing stdin: %v", err)
	}
	if err := f.stdinW.Close(); err != nil {
		t.Fatalf("closing stdin: %v", err)
	}

	if got := awaitProxyRequest(t, proxyRead); string(got) != command {
		t.Fatalf("proxy received %q, want %q", got, command)
	}
	if got := f.conn.closeWriteCalls.Load(); got != 1 {
		t.Fatalf("CloseWrite called %d times, want 1", got)
	}

	const response = "A1 OK NOOP completed\r\n"
	if _, err := io.WriteString(f.fromProxy, response); err != nil {
		t.Fatalf("proxy writing response: %v", err)
	}
	requireStillRunning(t, pumpErr)

	if err := f.fromProxy.Close(); err != nil {
		t.Fatalf("proxy closing: %v", err)
	}
	if err := awaitPump(t, pumpErr); err != nil {
		t.Fatalf("pump ended with error: %v", err)
	}
	if got := f.stdout.(*syncBuffer).String(); got != response {
		t.Fatalf("stdout received %q, want %q", got, response)
	}
}

func TestPumpEndsWhenProxyClosesFirst(t *testing.T) {
	f := newPumpFixture(t)
	pumpErr := f.start()

	if err := f.fromProxy.Close(); err != nil {
		t.Fatalf("proxy closing: %v", err)
	}
	if err := awaitPump(t, pumpErr); err != nil {
		t.Fatalf("pump ended with error: %v", err)
	}
	if got := f.conn.closeWriteCalls.Load(); got != 0 {
		t.Fatalf("CloseWrite called %d times, want 0", got)
	}
}

func TestPumpReportsStdoutWriteError(t *testing.T) {
	f := newPumpFixture(t)
	boom := errors.New("boom")
	f.stdout = errWriter{err: boom}
	pumpErr := f.start()

	proxyRead := readProxyAsync(f.toProxy)
	const command = "A1 NOOP\r\n"
	if _, err := io.WriteString(f.stdinW, command); err != nil {
		t.Fatalf("writing stdin: %v", err)
	}
	if err := f.stdinW.Close(); err != nil {
		t.Fatalf("closing stdin: %v", err)
	}
	if got := awaitProxyRequest(t, proxyRead); string(got) != command {
		t.Fatalf("proxy received %q, want %q", got, command)
	}

	if _, err := io.WriteString(f.fromProxy, "A1 OK\r\n"); err != nil {
		t.Fatalf("proxy writing response: %v", err)
	}
	if err := f.fromProxy.Close(); err != nil {
		t.Fatalf("proxy closing: %v", err)
	}
	err := awaitPump(t, pumpErr)
	if err == nil {
		t.Fatal("pump returned nil for conn->stdout write error")
	}
	if !errors.Is(err, boom) || !strings.Contains(err.Error(), "copy endpoint to stdout") {
		t.Fatalf("pump error = %v, want wrapped %q", err, boom)
	}
}

func TestPumpTreatsEPIPEOnStdinCopyAsClean(t *testing.T) {
	f := newPumpFixture(t)
	f.conn.writeErr = syscall.EPIPE
	f.stdin = strings.NewReader("A1 NOOP\r\n")
	pumpErr := f.start()

	select {
	case <-f.conn.closeWriteDone:
	case err := <-pumpErr:
		t.Fatalf("pump returned on EPIPE before conn->stdout ended: %v", err)
	case <-time.After(testWait):
		t.Fatal("timed out waiting for half-close after EPIPE")
	}
	requireStillRunning(t, pumpErr)

	if err := f.fromProxy.Close(); err != nil {
		t.Fatalf("proxy closing: %v", err)
	}
	if err := awaitPump(t, pumpErr); err != nil {
		t.Fatalf("EPIPE on stdin copy must be clean, got %v", err)
	}
}

func TestIsCleanConnEnd(t *testing.T) {
	generic := errors.New("protocol explosion")
	tests := []struct {
		name  string
		err   error
		clean bool
	}{
		{"nil", nil, true},
		{"EOF", io.EOF, true},
		{"unexpected EOF", io.ErrUnexpectedEOF, true},
		{"net.ErrClosed", net.ErrClosed, true},
		{"os.ErrClosed", os.ErrClosed, true},
		{"EPIPE", syscall.EPIPE, true},
		{"ECONNRESET", syscall.ECONNRESET, true},
		{"wrapped EOF", fmt.Errorf("read: %w", io.EOF), true},
		{"wrapped EPIPE", fmt.Errorf("write: %w", syscall.EPIPE), true},
		{"generic error", generic, false},
		{"wrapped generic error", fmt.Errorf("copy: %w", generic), false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := isCleanConnEnd(tc.err); got != tc.clean {
				t.Fatalf("isCleanConnEnd(%v) = %v, want %v", tc.err, got, tc.clean)
			}
		})
	}
}

func TestWorkloadAPIAddr(t *testing.T) {
	if _, err := workloadAPIAddr(""); err == nil {
		t.Fatal("expected error for empty socket")
	}
	got, err := workloadAPIAddr("unix:///run/spire/agent/public/api.sock")
	if err != nil || got != "unix:///run/spire/agent/public/api.sock" {
		t.Fatalf("workloadAPIAddr URI = %q, %v", got, err)
	}
	got, err = workloadAPIAddr("/run/spire/agent/public/api.sock")
	if err != nil || got != "unix:///run/spire/agent/public/api.sock" {
		t.Fatalf("workloadAPIAddr path = %q, %v", got, err)
	}
}
