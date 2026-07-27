package warpserver

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"warp/tunnel"
)

// These tests cover the warpserver.Server top seam (#2 / P1-A): the external
// behavior that used to live inline in main.go:314-381 — net.Listen + Accept
// loop with the exact Accept error handling, signal-driven graceful shutdown
// that awaits in-flight handlers, and the per-connection work delegated to a
// ProxyClient (so *tunnel.MasqueClient can be swapped for a double).
//
// The seam is black-box: external behavior only, never implementation details —
// we do not assert backoff constant values, only that a transient Accept error
// is retried and a later real connection then succeeds. No real QUIC, no mihomo:
// the ProxyClient is an echo double, and the mock WARP edge is just the double
// echoing bytes through the accepted TCP conn.
//
// Tests that need a controlled Accept error sequence use NewWithListener with a
// faultListener/timeoutListener — those exercise the Accept loop through the
// exact net.Listener interface the production path uses.

// quietLogf discards server log lines so a passing run does not spew
// Accept-backoff noise.
func quietLogf(string, ...any) {}

// echoPingPong is the wire payload the double echoes; distinct from any SOCKS5
// handshake byte so a future accidental real-handler mix never matches silently.
var echoPingPong = []byte("pingpong")

// echoProxy is the SOCKS5-opaque test double for ProxyClient. It accepts the
// conn the Server hands it, optionally parks on a drain gate (so graceful-
// shutdown tests can assert Close waits for in-flight handlers), then echoes a
// fixed 8-byte payload. The real *tunnel.MasqueClient does SOCKS5 + H3; here we
// only need to prove the Server accepts, runs a handler, and round-trips bytes
// — the B-1 injection seam's correctness lives in the tunnel package, not here.
//
// drain does NOT also select on ctx.Done(). That is deliberate: it models a
// handler doing real finish-work that should not be cut short by the shutdown
// context, so the test can assert Server.Close waits for it to retire on its
// own (the "收尾" property of the spec). How the *real* MasqueClient reacts to
// ctx cancellation (closing conn to unblock its relay goroutines) is its own
// contract, exercised in tunnel/masque.go — the Server's job here is only to
// not retire Wait before the handler returns.
type echoProxy struct {
	// connections counts HandleSOCKS5 invocations that actually started.
	connections int32
	// drain, when non-nil, holds the handler open until closed; used by graceful
	// shutdown tests to assert Close awaits in-flight handlers.
	drain chan struct{}
}

// HandleSOCKS5 reads 8 bytes and echoes them back — proving the conn is wired
// read/write end-to-end through the Server's delegate.
func (e *echoProxy) HandleSOCKS5(ctx context.Context, conn net.Conn, _ tunnel.SOCKS5Config) {
	atomic.AddInt32(&e.connections, 1)
	if e.drain != nil {
		// Block until the test releases the gate. We intentionally do NOT also
		// select ctx.Done() here: this models real in-flight work the shutdown
		// must wait out, not work that cancels itself on context — so the test
		// observes Server.Close actually waiting (not the handler bailing).
		<-e.drain
		if ctx.Err() != nil {
			// Shutdown won the race; still echo so the conn completes cleanly.
			return
		}
	}
	buf := make([]byte, len(echoPingPong))
	if _, err := io.ReadFull(conn, buf); err != nil {
		return
	}
	_, _ = conn.Write(buf)
}

// TestServer_EchoRoundTrip asserts the top-seam core: a SOCKS5-opaque client
// dials the Server, sends a payload, and the ProxyClient echo round-trips it
// back — proving Accept reached a handler goroutine and the conn is wired
// read/write end-to-end. This is the "echo SOCKS5 CONNECT data through the
// Server" slice deliberately without a real QUIC layer (that lives in the
// tunnel package's injection sub-seam, not here).
func TestServer_EchoRoundTrip(t *testing.T) {
	proxy := &echoProxy{}
	s := New("127.0.0.1:0", proxy, tunnel.SOCKS5Config{AllowUDP: true}, quietLogf)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := s.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer s.Close()

	conn, err := net.Dial("tcp", s.LocalAddr().String())
	if err != nil {
		t.Fatalf("dial server: %v", err)
	}
	defer conn.Close()
	if _, err := conn.Write(echoPingPong); err != nil {
		t.Fatalf("write: %v", err)
	}
	recvd := make([]byte, len(echoPingPong))
	if _, err := io.ReadFull(conn, recvd); err != nil {
		t.Fatalf("read echo: %v", err)
	}
	if !bytes.Equal(recvd, echoPingPong) {
		t.Fatalf("echo mismatch: got %q want %q", recvd, echoPingPong)
	}
	if n := atomic.LoadInt32(&proxy.connections); n != 1 {
		t.Fatalf("handler invoked %d times, want 1", n)
	}
}

// TestServer_GracefulShutdown_AwaitsInFlight is the "signal → in-flight
// HandleSOCKS5 收尾" slice: a connection whose handler is parked on the drain
// gate must complete cleanly before Wait retires. The test drives shutdown with
// a fake signal channel (StartWithSignal) so no OS signal is sent, then releases
// the handler and asserts Wait actually waited for it.
func TestServer_GracefulShutdown_AwaitsInFlight(t *testing.T) {
	proxy := &echoProxy{drain: make(chan struct{})}
	s := New("127.0.0.1:0", proxy, tunnel.SOCKS5Config{}, quietLogf)

	sigc := make(chan os.Signal, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := s.StartWithSignal(ctx, sigc); err != nil {
		t.Fatalf("StartWithSignal: %v", err)
	}

	clientConn, err := net.Dial("tcp", s.LocalAddr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer clientConn.Close()

	// Poll the handler-started counter rather than guessing timing; it observes
	// the seam contract ("Close waits") honestly.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if atomic.LoadInt32(&proxy.connections) == 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if atomic.LoadInt32(&proxy.connections) != 1 {
		t.Fatalf("handler never started; connections=%d", atomic.LoadInt32(&proxy.connections))
	}

	// Trigger graceful shutdown via the fake signal. Wait must NOT retire until
	// the in-flight handler drains — race it against a short timeout.
	sigc <- syscall.SIGTERM
	waited := make(chan struct{})
	go func() {
		_ = s.Wait()
		close(waited)
	}()

	select {
	case <-waited:
		t.Fatal("Wait retired before in-flight handler drained — graceful shutdown did not await in-flight")
	case <-time.After(150 * time.Millisecond):
		// good: still parked, handler has not been released
	}

	// Release the handler; Wait should now retire promptly.
	close(proxy.drain)
	select {
	case <-waited:
	case <-time.After(2 * time.Second):
		t.Fatal("Wait did not retire after handler drained")
	}
}

// TestServer_GracefulShutdown_CtxCancel awaits the parent-context cancellation
// path: canceling the run context (what Serve does when the parent ctx is
// canceled) must retire Wait, not require a signal.
func TestServer_GracefulShutdown_CtxCancel(t *testing.T) {
	proxy := &echoProxy{}
	s := New("127.0.0.1:0", proxy, tunnel.SOCKS5Config{}, quietLogf)

	ctx, cancel := context.WithCancel(context.Background())
	if err := s.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer s.Close()

	cancel() // parent ctx canceled → Server should retire its loop on Wait

	waited := make(chan struct{})
	go func() {
		_ = s.Wait()
		close(waited)
	}()
	select {
	case <-waited:
	case <-time.After(2 * time.Second):
		t.Fatal("Wait did not retire after run context canceled")
	}
}

// TestServer_Accept_Transient_Retries asserts the "transient Accept error →
// exponential backoff capped at 1s → retry, ultimately accepts a real
// connection" branch of Accept error handling equivalence. Two transient errors
// (a plain error string that is neither a Timeout nor ErrClosed) are injected,
// then a real accepted conn is forwarded. The loop must retry past both and the
// real connection must round-trip. We do NOT assert backoff constant values.
func TestServer_Accept_Transient_Retries(t *testing.T) {
	proxy := &echoProxy{}
	fault := newFaultListener(2)
	s := NewWithListener(fault, proxy, tunnel.SOCKS5Config{}, quietLogf)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sigc := make(chan os.Signal, 1)
	defer close(sigc)
	if err := s.StartWithSignal(ctx, sigc); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer s.Close()

	conn, err := net.Dial("tcp", fault.addr())
	if err != nil {
		t.Fatalf("dial fault listener: %v", err)
	}
	defer conn.Close()
	if _, err := conn.Write(echoPingPong); err != nil {
		t.Fatalf("write: %v", err)
	}
	recvd := make([]byte, len(echoPingPong))
	if _, err := io.ReadFull(conn, recvd); err != nil {
		t.Fatalf("read echo: %v", err)
	}
	if !bytes.Equal(recvd, echoPingPong) {
		t.Fatalf("echo mismatch: got %q want %q", recvd, echoPingPong)
	}
	if got := atomic.LoadInt32(&proxy.connections); got != 1 {
		t.Fatalf("handler invoked %d times, want 1 (transient Accept must not double-dispatch)", got)
	}
}

// TestServer_Accept_Timeout_Continues asserts the "Timeout error → continue,
// no retry, no wait" branch: two Timeout errors are injected, then a real conn.
// The loop must not crash or back off and the real conn must round-trip.
func TestServer_Accept_Timeout_Continues(t *testing.T) {
	proxy := &echoProxy{}
	fault := newTimeoutListener(2)
	s := NewWithListener(fault, proxy, tunnel.SOCKS5Config{}, quietLogf)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sigc := make(chan os.Signal, 1)
	defer close(sigc)
	if err := s.StartWithSignal(ctx, sigc); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer s.Close()

	conn, err := net.Dial("tcp", fault.addr())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	if _, err := conn.Write(echoPingPong); err != nil {
		t.Fatalf("write: %v", err)
	}
	recvd := make([]byte, len(echoPingPong))
	if _, err := io.ReadFull(conn, recvd); err != nil {
		t.Fatalf("read echo: %v", err)
	}
	if !bytes.Equal(recvd, echoPingPong) {
		t.Fatalf("echo mismatch: got %q want %q", recvd, echoPingPong)
	}
}

// TestServer_Close_BeforeStart_IsNoOp asserts Close on a never-Started Server is
// a clean no-op and Wait returns immediately.
func TestServer_Close_BeforeStart_IsNoOp(t *testing.T) {
	s := New("127.0.0.1:0", &echoProxy{}, tunnel.SOCKS5Config{}, quietLogf)
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := s.Wait(); err != nil {
		t.Fatalf("Wait: %v", err)
	}
}

// TestServer_Start_BindFailure_ReturnsError asserts a listener bind failure
// surfaces from Start rather than panicking.
func TestServer_Start_BindFailure_ReturnsError(t *testing.T) {
	hold, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("hold: %v", err)
	}
	defer hold.Close()
	port := hold.Addr().(*net.TCPAddr).Port

	s := New(fmt.Sprintf("127.0.0.1:%d", port), &echoProxy{}, tunnel.SOCKS5Config{}, quietLogf)
	err = s.Start(context.Background())
	if err == nil {
		_ = s.Close()
		t.Fatal("Start on an already-bound port unexpectedly succeeded")
	}
}

// ---- fault-listener scaffolding --------------------------------------------
//
// Not general-purpose net.Listener doubles; they exist only to feed the
// Server's Accept loop a controlled error sequence and then forward a real
// accepted conn. The Accept loop never sees these types — only net.Listener's
// Accept signature — so the loop's error handling is tested through the same
// interface production uses.

// timeoutErr is a net.Error that reports Timeout()==true and Temporary()==false,
// to exercise the `continue` branch of Accept (no backoff, no wait).
type timeoutErr struct{}

func (timeoutErr) Error() string   { return "i/o timeout" }
func (timeoutErr) Timeout() bool   { return true }
func (timeoutErr) Temporary() bool { return false }

// faultListener forwards Accept to a real listener after returning a plain
// (non-Timeout, non-ErrClosed) error `transient` times — exercising the
// exponential-backoff-retry branch.
type faultListener struct {
	real      net.Listener
	transient int
	mu        sync.Mutex
}

func newFaultListener(transient int) *faultListener {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		panic(fmt.Sprintf("faultListener: listen: %v", err))
	}
	return &faultListener{real: ln, transient: transient}
}

func (f *faultListener) addr() string { return f.real.Addr().String() }

func (f *faultListener) Accept() (net.Conn, error) {
	f.mu.Lock()
	if f.transient > 0 {
		f.transient--
		f.mu.Unlock()
		// A plain error skips both the Timeout and ErrClosed branches of the loop,
		// so the loop backs off and retries — exactly the "transient → backoff"
		// path.
		return nil, errors.New("transient accept hiccup")
	}
	f.mu.Unlock()
	return f.real.Accept()
}

func (f *faultListener) Close() error   { return f.real.Close() }
func (f *faultListener) Addr() net.Addr { return f.real.Addr() }

// timeoutListener is like faultListener but returns a Timeout error — the
// `continue` branch (no backoff, no wait).
type timeoutListener struct {
	real     net.Listener
	timeouts int
	mu       sync.Mutex
}

func newTimeoutListener(timeouts int) *timeoutListener {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		panic(fmt.Sprintf("timeoutListener: listen: %v", err))
	}
	return &timeoutListener{real: ln, timeouts: timeouts}
}

func (f *timeoutListener) addr() string { return f.real.Addr().String() }

func (f *timeoutListener) Accept() (net.Conn, error) {
	f.mu.Lock()
	if f.timeouts > 0 {
		f.timeouts--
		f.mu.Unlock()
		return nil, timeoutErr{}
	}
	f.mu.Unlock()
	return f.real.Accept()
}

func (f *timeoutListener) Close() error   { return f.real.Close() }
func (f *timeoutListener) Addr() net.Addr { return f.real.Addr() }
