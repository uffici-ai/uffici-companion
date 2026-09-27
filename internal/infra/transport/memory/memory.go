// Package memory is an in-process transport: the real surface, the real frames,
// no socket.
//
// 🔴 This is NOT a mock of the protocol. The surface, the validator and the
// state machine run on it unchanged and exchange exactly the bytes they would
// exchange over a Unix socket; only the journey is shortened. That distinction
// is what makes it usable as the transport under almost every test — a mock of
// the protocol would test the mock.
package memory

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/uffici-ai/uffici-companion/internal/contracts"
)

// ErrClosed is returned by Receive and Send once the connection is closed.
var ErrClosed = errors.New("memory transport: connection closed")

// Listener hands out connections created by Dial.
type Listener struct {
	mu       sync.Mutex
	incoming chan *Conn
	closed   bool
	addr     string
}

// NewListener returns a listener with no pending connections.
func NewListener(addr string) *Listener {
	return &Listener{incoming: make(chan *Conn, 4), addr: addr}
}

// Dial creates a connected pair and offers the server half to Accept, returning
// the client half — what a test plugin holds.
func (l *Listener) Dial() (*Conn, error) {
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		return nil, ErrClosed
	}
	l.mu.Unlock()

	toServer := make(chan []byte, 16)
	toClient := make(chan []byte, 16)
	shared := &closeState{done: make(chan struct{})}

	server := &Conn{in: toServer, out: toClient, state: shared, addr: l.addr, side: "server"}
	client := &Conn{in: toClient, out: toServer, state: shared, addr: l.addr, side: "client"}

	select {
	case l.incoming <- server:
		return client, nil
	case <-shared.done:
		return nil, ErrClosed
	}
}

// Accept returns the next connection offered by Dial.
func (l *Listener) Accept(ctx context.Context) (contracts.Conn, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case conn, ok := <-l.incoming:
		if !ok {
			return nil, ErrClosed
		}
		return conn, nil
	}
}

// Addr describes the listener, for logs and `/healthz`.
func (l *Listener) Addr() string { return l.addr }

// Close stops accepting.
func (l *Listener) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return nil
	}
	l.closed = true
	close(l.incoming)
	return nil
}

// closeState is shared by both halves so either side's Close ends both.
type closeState struct {
	once   sync.Once
	done   chan struct{}
	mu     sync.Mutex
	reason string
}

func (s *closeState) close(reason string) {
	s.once.Do(func() {
		s.mu.Lock()
		s.reason = reason
		s.mu.Unlock()
		close(s.done)
	})
}

// Reason reports why the connection closed, or "" while it is open.
func (s *closeState) Reason() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.reason
}

// Conn is one half of an in-process connection.
type Conn struct {
	in    chan []byte
	out   chan []byte
	state *closeState
	addr  string
	side  string
}

// Receive blocks for the next frame.
//
// 🔴 Closure is checked BEFORE the select, never only inside it. Both channels
// are buffered, so a select offering `done` alongside a ready channel picks
// between them at random — a closed connection would keep serving frames on
// roughly half of all runs, and the test that noticed would read as a flake.
func (c *Conn) Receive(ctx context.Context) ([]byte, error) {
	if err := c.closedErr(); err != nil {
		return nil, err
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-c.state.done:
		return nil, c.closedErr()
	case frame := <-c.in:
		return frame, nil
	}
}

// Send writes one frame.
//
// Same pre-check as Receive, for the same reason — and here the consequence is
// worse: a frame accepted after close is silently never read by anyone.
func (c *Conn) Send(ctx context.Context, frame []byte) error {
	if err := c.closedErr(); err != nil {
		return err
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-c.state.done:
		return c.closedErr()
	case c.out <- frame:
		return nil
	}
}

// closedErr reports the closure, or nil while the connection is open.
func (c *Conn) closedErr() error {
	select {
	case <-c.state.done:
		return fmt.Errorf("%w (%s)", ErrClosed, c.state.Reason())
	default:
		return nil
	}
}

// Close ends both halves, recording the reason the peer will see.
func (c *Conn) Close(reason string) error {
	c.state.close(reason)
	return nil
}

// CloseReason reports why the connection closed — what a test asserts on when
// the surface refuses a plugin.
func (c *Conn) CloseReason() string { return c.state.Reason() }

// Closed is closed when the connection ends, so a test can wait rather than poll.
func (c *Conn) Closed() <-chan struct{} { return c.state.done }

// RemoteHint is a non-identifying label for logs.
func (c *Conn) RemoteHint() string { return c.addr + "/" + c.side }
