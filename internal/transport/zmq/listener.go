// Package zmq wraps a ZeroMQ ROUTER socket (via the pure-Go
// github.com/go-zeromq/zmq4 binding) into a small Listener type that
// internal/channel/zmqcli can drive. The wrapper is intentionally narrow:
// Recv returns the originating identity and the payload bytes; Send writes a
// payload back to a specific identity; Close honors an idempotent shutdown.
//
// The choice of pure-Go zmq4 (instead of the more feature-complete CGO
// pebbe/zmq4) is a project-wide constraint — see
// decided when the ZMQ daemon transport was added (see the listener docs).
package zmq

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/go-zeromq/zmq4"
	"github.com/rs/zerolog"
)

// ErrContextDone is returned by Recv when the listener's root context is
// canceled. Callers should treat it as a clean exit signal, not an error.
var ErrContextDone = errors.New("zmq listener: context done")

// ErrListenerClosed is returned by Recv / Send after Close has been called.
// Like ErrContextDone, this is a clean exit signal.
var ErrListenerClosed = errors.New("zmq listener: closed")

// Listener is the daemon-side wrapper around a ZeroMQ ROUTER socket. It is
// safe to call Send concurrently with Recv; Close is idempotent.
type Listener struct {
	// socket is the underlying ROUTER. zmq4's Socket implementation is
	// goroutine-safe across Send/Recv pairs; we additionally guard Close with
	// closeOnce to keep the close path idempotent.
	socket zmq4.Socket
	// addr is the resolved listen address (e.g. "tcp://127.0.0.1:50321"
	// after an ephemeral bind to tcp://127.0.0.1:0).
	addr string
	// logger is scoped to component=zmq-listener and used by Recv / Close.
	logger zerolog.Logger
	// closeOnce guards Close so multiple callers (channel manager shutdown +
	// SIGINT handler + Listener.Recv loop exit) do not double-close the socket.
	closeOnce sync.Once
	// closed is set under closeOnce to short-circuit Recv / Send after Close.
	closed  bool
	closeMu sync.RWMutex
	// ctx is the listener's root context. When it is canceled, Recv returns
	// ErrContextDone so the caller's loop can exit cleanly.
	ctx context.Context
}

// Compile-time interface assertion: Listener is an io.Closer.
var _ io.Closer = (*Listener)(nil)

// NewListener creates a ROUTER socket bound to bindAddr. The supplied logger
// is used as-is — the caller is responsible for scoping it via
// .Str("component", "zmq-listener") before passing it in (this avoids
// re-scoping on every call site).
//
// bindAddr supports the same forms as zmq4.Socket.Listen: tcp://host:port,
// tcp://host:0 for an ephemeral port (after which Addr() reports the
// resolved endpoint), ipc://path, inproc://name. Only tcp is exercised by
// the current home-agent flow.
//
// The pure-Go zmq4 binding does not expose the four ZMQ_TCP_KEEPALIVE_*
// socket options. TCP keepalive falls back to Go's net.Listen defaults
// (keepalive enabled on the accepted *net.TCPConn). The connection_idle_timeout
// watchdog inside zmqcli is the primary dead-peer detector — see the design doc.
func NewListener(ctx context.Context, bindAddr string, logger zerolog.Logger) (*Listener, error) {
	if bindAddr == "" {
		return nil, errors.New("zmq listener: bind address is required")
	}

	socket := zmq4.NewRouter(ctx)

	if err := socket.Listen(bindAddr); err != nil {
		// Best-effort close — Listen failures leave the socket in an undefined
		// state, but Close should still be safe to call.
		_ = socket.Close()

		return nil, fmt.Errorf("zmq listener: failed to listen on %q: %w", bindAddr, err)
	}

	resolved := bindAddr
	if addr := socket.Addr(); addr != nil {
		// For tcp://, Addr() returns a *net.TCPAddr whose String() reflects
		// the actual ephemeral port. Preserve the tcp:// prefix so callers see
		// a fully-formed ZMQ endpoint.
		resolved = "tcp://" + addr.String()
	}

	l := &Listener{
		socket: socket,
		addr:   resolved,
		logger: logger,
		ctx:    ctx,
	}

	l.logger.Info().Str("endpoint", resolved).Msg("zmq_listener_bound")

	return l, nil
}

// Addr returns the resolved listen address (e.g. "tcp://127.0.0.1:50321"
// after an ephemeral bind). Useful for tests that bind to port 0.
func (l *Listener) Addr() string {
	return l.addr
}

// Recv blocks until the ROUTER receives a complete message, then returns the
// originating identity frame (Frames[0]) and the payload (last non-empty
// application frame). Supports both REQ peers (which insert an empty delimiter
// frame: [identity, "", payload]) and DEALER peers (which send only
// [identity, payload]).
//
// On context cancellation Recv returns ErrContextDone. After Close has been
// called, Recv returns ErrListenerClosed. Transient transport errors (any
// other non-nil return from socket.Recv) are wrapped with fmt.Errorf and
// returned as-is so the caller can decide whether to log-and-continue.
func (l *Listener) Recv() (identity, payload []byte, err error) {
	if l.isClosed() {
		return nil, nil, ErrListenerClosed
	}

	msg, err := l.socket.Recv()
	if err != nil {
		// Context cancellation surfaces here as ctx.Err() wrapped by the
		// binding; distinguish it from real transport errors so the caller's
		// loop can exit cleanly.
		if l.ctx.Err() != nil {
			return nil, nil, ErrContextDone
		}

		if l.isClosed() {
			return nil, nil, ErrListenerClosed
		}

		return nil, nil, fmt.Errorf("zmq listener: recv: %w", err)
	}

	if len(msg.Frames) < 2 {
		// A ROUTER receiving a single frame means the binding failed to split
		// identity from payload. Treat as malformed.
		return nil, nil, fmt.Errorf("zmq listener: recv returned %d frames, expected >= 2", len(msg.Frames))
	}

	identity = msg.Frames[0]
	// DEALER sends [identity, payload] (2 frames).
	// REQ sends [identity, "", payload] (3+ frames with empty delimiter).
	// In both cases the application payload is the last frame.
	payload = msg.Frames[len(msg.Frames)-1]

	return identity, payload, nil
}

// Send writes payload back to identity. The framing is [identity, payload]
// which works for both DEALER and REQ peers: go-zeromq's ROUTER automatically
// inserts the empty delimiter for REQ peers at the transport level.
func (l *Listener) Send(identity, payload []byte) error {
	if l.isClosed() {
		return ErrListenerClosed
	}

	msg := zmq4.NewMsgFrom(identity, []byte{}, payload)

	if err := l.socket.Send(msg); err != nil {
		return fmt.Errorf("zmq listener: send: %w", err)
	}

	return nil
}

// Close closes the underlying ROUTER socket. It is idempotent and safe to
// call from any goroutine. The shutdown handshake (best-effort
// Response{Status: "shutdown"} to the active client, if any) is the
// responsibility of the channel layer — Listener.Close only releases the
// transport.
func (l *Listener) Close() error {
	var closeErr error

	l.closeOnce.Do(func() {
		l.closeMu.Lock()
		l.closed = true
		l.closeMu.Unlock()

		// zmq4.Socket.Close is itself idempotent under the binding, but we
		// only call it once anyway to keep the log line single.
		if err := l.socket.Close(); err != nil {
			closeErr = fmt.Errorf("zmq listener: close: %w", err)
		}

		l.logger.Debug().Str("endpoint", l.addr).Msg("zmq_listener_closed")
	})

	return closeErr
}

// isClosed reports whether Close has been called. Wraps the RWMutex read so
// the hot path (Recv) doesn't take a write lock.
func (l *Listener) isClosed() bool {
	l.closeMu.RLock()
	defer l.closeMu.RUnlock()

	return l.closed
}
