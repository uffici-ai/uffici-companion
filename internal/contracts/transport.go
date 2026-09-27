package contracts

import "context"

// 🔴 THE SEAM THAT DECIDES WHETHER THIS DAEMON IS TESTABLE.
//
// The local surface owns the protocol — hello, one attached plugin, frame
// validation, the close vocabulary — and the state machine owns the request
// lifecycle. Neither owns a socket. A `net.Listener` reached for directly would
// put a real Unix socket, a real WebSocket upgrade and a real port between
// every test and the logic it wants to exercise, which is how a Go daemon ends
// up tested through its integration path only and stuck around 60 % coverage.
//
// So the transport is an interface with three implementations: a Unix socket, a
// loopback TCP listener, and an in-memory pipe that every surface and
// state-machine test uses. The pipe is not a mock of the protocol — the real
// surface runs on it, byte for byte; only the bytes' journey is shortened.

// Conn is one plugin connection: text frames in, text frames out, and a close
// that names its reason.
type Conn interface {
	// Receive blocks for the next frame. It returns an error when the
	// connection is closed by either side or the context ends.
	Receive(ctx context.Context) ([]byte, error)

	// Send writes one frame.
	Send(ctx context.Context, frame []byte) error

	// Close ends the connection, telling the peer why.
	//
	// 🔴 The reason is a value from the wire's closed vocabulary, never free
	// text: it is the only thing the other side learns about what went wrong,
	// and a plugin author debugging a refused connection has nothing else.
	Close(reason string) error

	// RemoteHint is a non-identifying label for logs — the socket path or the
	// loopback address. Never a credential, never a header value.
	RemoteHint() string
}

// Listener accepts plugin connections.
type Listener interface {
	// Accept blocks for the next connection.
	Accept(ctx context.Context) (Conn, error)

	// Addr describes where it listens, for `/healthz` and for a startup log.
	Addr() string

	// Close stops accepting.
	Close() error
}

// TransportKind names how the surface listens.
type TransportKind string

const (
	// TransportSocket is the default: a Unix socket, owner-only, whose file
	// custody IS the authorization boundary.
	TransportSocket TransportKind = "socket"
	// TransportTCP is the pod shape, where the plugin and the companion share a
	// network namespace but not a filesystem.
	TransportTCP TransportKind = "tcp"
)
