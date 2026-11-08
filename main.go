// Command spiffec bridges stdin/stdout to a SPIFFE mTLS TCP endpoint for tools
// that take a per-connection transport command (e.g. OpenSSH ProxyCommand or
// mbsync Tunnel).
//
// stdout carries only tunnelled bytes; diagnostics go to stderr so the calling
// protocol parser is never corrupted.
package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/spiffe/go-spiffe/v2/spiffeid"
	"github.com/spiffe/go-spiffe/v2/spiffetls/tlsconfig"
	"github.com/spiffe/go-spiffe/v2/workloadapi"
)

const (
	sourceTimeout          = 30 * time.Second
	dialTimeout            = 15 * time.Second
	handshakeTimeout       = 15 * time.Second
	defaultSPIRESocketPath = "/run/spire/agent/public/api.sock"
)

func defaultSPIRESocket() string {
	if socket := os.Getenv("SPIFFE_ENDPOINT_SOCKET"); socket != "" {
		return socket
	}
	return defaultSPIRESocketPath
}

func main() {
	var (
		addr        = flag.String("addr", "", "mTLS endpoint address (host:port)")
		serverID    = flag.String("server-id", "", "expected server SPIFFE ID")
		spireSocket = flag.String("spire-socket", defaultSPIRESocket(), "SPIRE Workload API socket path")
	)
	flag.Parse()

	if flag.NArg() != 0 {
		usageError("unexpected positional arguments")
	}
	if *addr == "" {
		usageError("-addr is required")
	}
	if _, _, err := net.SplitHostPort(*addr); err != nil {
		usageError("-addr must be host:port: %v", err)
	}
	if *serverID == "" {
		usageError("-server-id is required")
	}
	serverSPIFFEID, err := spiffeid.FromString(*serverID)
	if err != nil {
		usageError("-server-id is not a valid SPIFFE ID: %v", err)
	}
	workloadAPIEndpoint, err := workloadAPIAddr(*spireSocket)
	if err != nil {
		usageError("invalid -spire-socket: %v", err)
	}

	// Ignore SIGPIPE so a client closing stdout while final bytes are in flight
	// surfaces as EPIPE rather than killing the process with a signal.
	signal.Ignore(syscall.SIGPIPE)

	ctx := context.Background()

	srcCtx, cancelSrc := context.WithTimeout(ctx, sourceTimeout)
	source, err := workloadapi.NewX509Source(srcCtx, workloadapi.WithClientOptions(
		workloadapi.WithAddr(workloadAPIEndpoint),
	))
	cancelSrc()
	if err != nil {
		fatal("cannot obtain X509-SVID from Workload API at %s: %v", *spireSocket, err)
	}
	// Keep the source open for the session so SVIDs rotate during long-lived tunnels.
	defer source.Close()

	tlsCfg := tlsconfig.MTLSClientConfig(source, source, tlsconfig.AuthorizeID(serverSPIFFEID))

	if err := tunnel(ctx, *addr, tlsCfg); err != nil {
		fatal("%v", err)
	}
}

func tunnel(ctx context.Context, addr string, tlsCfg *tls.Config) error {
	d := &net.Dialer{Timeout: dialTimeout}
	raw, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return fmt.Errorf("dial %s: %w", addr, err)
	}
	tc := tls.Client(raw, tlsCfg)
	hsCtx, cancel := context.WithTimeout(ctx, handshakeTimeout)
	err = tc.HandshakeContext(hsCtx)
	cancel()
	if err != nil {
		_ = tc.Close()
		return fmt.Errorf("TLS handshake with %s: %w", addr, err)
	}
	defer tc.Close()

	return pump(tc, os.Stdin, os.Stdout)
}

type halfCloseConn interface {
	io.Reader
	io.Writer
	CloseWrite() error
}

// pump forwards stdin EOF via CloseWrite rather than closing the whole
// connection so the server can flush its final response before teardown.
func pump(conn halfCloseConn, stdin io.Reader, stdout io.Writer) error {
	stdinErr := make(chan error, 1)
	stdoutErr := make(chan error, 1)

	go func() {
		_, err := io.Copy(conn, stdin)
		if err != nil && !isCleanConnEnd(err) {
			stdinErr <- fmt.Errorf("copy stdin to endpoint: %w", err)
			return
		}
		if err := conn.CloseWrite(); err != nil && !isCleanConnEnd(err) {
			stdinErr <- fmt.Errorf("half-close TLS connection: %w", err)
			return
		}
		stdinErr <- nil
	}()

	go func() {
		if _, err := io.Copy(stdout, conn); err != nil {
			stdoutErr <- fmt.Errorf("copy endpoint to stdout: %w", err)
			return
		}
		stdoutErr <- nil
	}()

	for {
		select {
		case err := <-stdinErr:
			stdinErr = nil
			if err != nil {
				return err
			}
			if stdoutErr == nil {
				return nil
			}
		case err := <-stdoutErr:
			stdoutErr = nil
			if err != nil && !isCleanConnEnd(err) {
				return err
			}
			return nil
		}
	}
}

// isCleanConnEnd treats peer disconnects without a TLS close_notify alert as
// normal session termination rather than transport errors.
func isCleanConnEnd(err error) bool {
	return err == nil ||
		errors.Is(err, io.EOF) ||
		errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, net.ErrClosed) ||
		errors.Is(err, os.ErrClosed) ||
		errors.Is(err, syscall.EPIPE) ||
		errors.Is(err, syscall.ECONNRESET)
}

// workloadAPIAddr converts bare filesystem paths into unix:// URIs required by
// go-spiffe's workloadapi.WithAddr.
func workloadAPIAddr(socket string) (string, error) {
	if socket == "" {
		return "", errors.New("empty socket path")
	}
	if strings.Contains(socket, "://") {
		return socket, nil
	}
	abs, err := filepath.Abs(socket)
	if err != nil {
		return "", err
	}
	return "unix://" + abs, nil
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "spiffec: "+format+"\n", args...)
	os.Exit(1)
}

// usageError exits 2 so callers can distinguish configuration errors from
// runtime network/SPIFFE failures (exit 1).
func usageError(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "spiffec: "+format+"\n", args...)
	flag.Usage()
	os.Exit(2)
}
