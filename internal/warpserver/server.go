// Package warpserver owns the SOCKS5 service the warp binary exposes: the net
// listener, the Accept loop with its exponential backoff on transient errors,
// and the signal-driven graceful shutdown. It is the external-behavior top seam
// for P1-A (#2) — what main.go held inline at main.go:314-381 is now observable
// here as a black box, with the per-connection SOCKS5 work delegated to a
// ProxyClient (so *tunnel.MasqueClient can be swapped for a test double).
//
// The Accept error handling is byte-for-byte the status quo:
//
//   - a transient Accept error (anything that isn't a Timeout and isn't
//     ErrClosed) doubles a backoff capped at one second and retries, so a
//     fleeting resource condition (running out of file descriptors, a client
//     vanishing between SYN and accept) never takes the whole process down;
//   - a Timeout error continues immediately, no retry, no wait — timeouts are
//     expected under load and adding backoff would amplify a stall;
//   - ErrClosed or the run context being done breaks the loop; on signal
//     shutdown the context is canceled and the listener closed to unblock
//     Accept, and in-flight HandleSOCKS5 calls are awaited before Close returns.
package warpserver

import (
	"context"
	"errors"
	"log"
	"net"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"warp/tunnel"
)

// ProxyClient is the per-connection SOCKS5 handler the Server drives. The real
// one is *tunnel.MasqueClient; tests pass a double. The interface lives here
// (not in tunnel) so the seam contract is named at its injection point.
type ProxyClient interface {
	// HandleSOCKS5 services one accepted TCP connection. ctx is the Server's run
	// context; on shutdown it is canceled and conn is closed, which unblocks any
	// relay goroutines inside the handler. The handler is responsible for
	// closing conn itself, so implementations must be safe to that double-close.
	HandleSOCKS5(ctx context.Context, conn net.Conn, cfg tunnel.SOCKS5Config)
}

// maxAcceptBackoff caps the exponential backoff between Accept retries on
// transient errors. It matches the inline constant that lived in main.go.
const maxAcceptBackoff = time.Second

// Server is the SOCKS5 service top seam. The zero value is not usable; build it
// with New.
type Server struct {
	addr     string
	proxy    ProxyClient
	socksCfg tunnel.SOCKS5Config
	logf     func(format string, args ...any)

	listener net.Listener

	// run coordinates Accept-loop shutdown. startOnce guards Start; closeOnce
	// serializes Close so a duplicate (double signal, defer after Close) is a
	// no-op and closes exactly once.
	startOnce sync.Once
	closeOnce sync.Once

	// inflightWg counts live HandleSOCKS5 goroutines so Close can wait for them
	// to drain before returning — the "restart loses no尾包" property.
	inflightWg sync.WaitGroup

	// done is closed the moment the Accept loop and all in-flight handlers have
	// retired (graceful shutdown complete). Wait blocks on it.
	done chan struct{}

	// runCtx / runCancel are the Accept-loop context handed to every handler.
	// Canceled by Close to fan the shutdown into in-flight handlers.
	runCtx    context.Context
	runCancel context.CancelFunc

	// started tracks whether Start built a listener, so Close on a never-started
	// Server is a clean no-op rather than touching nil fields.
	started bool
}

// New builds a Server listening on addr, serving SOCKS5 via proxy with cfg. The
// listener is not opened until Start/Serve. logf, when nil, defaults to log.Printf.
func New(addr string, proxy ProxyClient, cfg tunnel.SOCKS5Config, logf func(string, ...any)) *Server {
	if logf == nil {
		logf = log.Printf
	}
	return &Server{
		addr:     addr,
		proxy:    proxy,
		socksCfg: cfg,
		logf:     logf,
	}
}

// NewWithListener builds a Server that serves an already-opened listener. This
// is the seam tests use to inject a fault listener (one that returns a
// controlled Accept error sequence) to exercise the Accept-loop's error
// handling through the same net.Listener interface production uses. addr is
// kept for logging only; the listener's own LocalAddr is the source of truth.
func NewWithListener(ln net.Listener, proxy ProxyClient, cfg tunnel.SOCKS5Config, logf func(string, ...any)) *Server {
	if logf == nil {
		logf = log.Printf
	}
	return &Server{
		addr:     ln.Addr().String(),
		proxy:    proxy,
		socksCfg: cfg,
		logf:     logf,
		listener: ln,
	}
}

// Serve is the one-shot blocking entry: Start, then Wait. It owns the run
// context and the signal handler, so a caller that wants the "SIGINT/SIGTERM
// → graceful shutdown" behavior of the original main.go just does srv.Serve()
// and blocks. Returns nil on a clean shutdown, the listener error if Start
// failed, or the context cause if the (caller-provided) ctx was canceled first.
//
// When parent is canceled, Serve triggers the same shutdown path a signal would:
// cancel the run context, close the listener, and wait for in-flight handlers
// before returning.
func (s *Server) Serve(parent context.Context) error {
	if err := s.Start(parent); err != nil {
		return err
	}
	return s.Wait()
}

// Signal harness: tests inject a fake sigc so they can drive graceful shutdown
// deterministically without sending real OS signals. Production passes nil,
// which wires the real SIGINT/SIGTERM notifier.
func (s *Server) Start(parent context.Context) error {
	return s.start(parent, nil)
}

// StartWithSignal is the Start variant tests use to drive shutdown: a signal
// arriving on sigc triggers Close exactly like a real SIGINT/SIGTERM. Passing a
// fake channel lets a test close the server deterministically without sending OS
// signals. Production code always uses Start.
func (s *Server) StartWithSignal(parent context.Context, sigc chan os.Signal) error {
	return s.start(parent, sigc)
}

// Close triggers graceful shutdown if it hasn't already run: cancel the run
// context, close the listener to unblock Accept, then (via Wait) await the
// in-flight handlers. Safe to call concurrently and more than once.
func (s *Server) Close() error {
	s.closeOnce.Do(func() { s.shutdown() })
	return nil
}

// Wait blocks until the Accept loop and all in-flight HandleSOCKS5 calls have
// retired (graceful shutdown complete or Start never succeeded). Returns nil on
// a clean shutdown, or a Start error if Start failed before Wait was reachable.
func (s *Server) Wait() error {
	if s.done != nil {
		<-s.done
	}
	return nil
}

// LocalAddr returns the listener's local address after Start, or nil before.
func (s *Server) LocalAddr() net.Addr {
	if s.listener == nil {
		return nil
	}
	return s.listener.Addr()
}

// start is the shared Start used by Serve and tests. sigc lets tests inject a
// controlled signal channel; nil installs the real SIGINT/SIGTERM notifier.
func (s *Server) start(parent context.Context, sigc chan os.Signal) error {
	var startErr error
	s.startOnce.Do(func() {
		// NewWithListener pre-installs s.listener; otherwise open one from addr.
		if s.listener == nil {
			ln, err := net.Listen("tcp", s.addr)
			if err != nil {
				startErr = err
				s.done = make(chan struct{})
				close(s.done)
				return
			}
			s.listener = ln
		}
		s.started = true
		s.done = make(chan struct{})

		// runCtx derives from parent so a parent cancellation triggers our
		// shutdown path too. We keep runCancel to fan the same signal into
		// in-flight handlers.
		s.runCtx, s.runCancel = context.WithCancel(parent)

		// Signal wiring mirrors main.go: SIGINT/SIGTERM drive graceful close.
		// When nil (production) install the real notifier; tests pass a fake
		// channel so they don't have to send real signals.
		if sigc == nil {
			sigc = make(chan os.Signal, 1)
			signal.Notify(sigc, syscall.SIGINT, syscall.SIGTERM)
		}
		go func() {
			<-sigc
			s.logf("正在关闭...")
			s.Close()
		}()

		// Parent-context watcher: a canceled parent triggers the same graceful
		// shutdown a signal would, so a caller of Serve(parent) can cancel parent
		// to retire the server without sending a signal. It also retires cleanly
		// on a signal-driven shutdown: s.done is closed by acceptLoop's defer, so
		// this goroutine exits via that branch instead of leaking.
		go func() {
			select {
			case <-parent.Done():
				s.Close()
			case <-s.done:
			}
		}()

		// The Accept loop runs in its own goroutine so Start returns; Wait blocks
		// on done. This also lets Close unblock Accept by closing the listener.
		go s.acceptLoop()
	})
	return startErr
}

// acceptLoop drives the Accept/backoff behavior. It must be byte-for-byte the
// status quo (see package doc): transient → exponential backoff capped at 1s;
// Timeout → continue; ErrClosed or runCtx done → break and retire.
func (s *Server) acceptLoop() {
	defer close(s.done)
	defer s.inflightWg.Wait()

	var acceptBackoff time.Duration
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			if s.runCtx.Err() != nil {
				return // graceful shutdown
			}
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				continue
			}
			if errors.Is(err, net.ErrClosed) {
				return // listener gone (shutdown we initiated or external close)
			}
			if acceptBackoff == 0 {
				acceptBackoff = 5 * time.Millisecond
			} else if acceptBackoff *= 2; acceptBackoff > maxAcceptBackoff {
				acceptBackoff = maxAcceptBackoff
			}
			s.logf("Accept 出错：%v，%s 后重试", err, acceptBackoff)
			select {
			case <-time.After(acceptBackoff):
			case <-s.runCtx.Done():
				return
			}
			continue
		}
		acceptBackoff = 0
		s.inflightWg.Add(1)
		go func(c net.Conn) {
			defer s.inflightWg.Done()
			defer c.Close()
			s.proxy.HandleSOCKS5(s.runCtx, c, s.socksCfg)
		}(conn)
	}
}

// shutdown is the actual close path (guarded by closeOnce). It cancels the run
// context (so in-flight handlers unblock) and closes the listener (so Accept
// returns). The acceptLoop's deferred inflightWg.Wait plus the deferred close(s.done)
// handle waiting for handlers before Wait returns — Close itself returns fast.
func (s *Server) shutdown() {
	if s.runCancel != nil {
		s.runCancel()
	}
	if s.listener != nil {
		_ = s.listener.Close()
	} else if s.done != nil {
		// Never started a listener: nothing to wait on, unblock Wait immediately.
		select {
		case <-s.done:
		default:
			close(s.done)
		}
	}
}
