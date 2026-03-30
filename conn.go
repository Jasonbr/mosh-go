package mosh

import "time"

// Conn is a datagram-oriented connection used by the mosh transport.
// UDP, WebTransport, and other datagram transports implement this interface.
type Conn interface {
	// Read reads a datagram. Returns the number of bytes read.
	// Must respect deadlines set by SetReadDeadline.
	Read(b []byte) (int, error)

	// Write writes a datagram.
	Write(b []byte) (int, error)

	// SetReadDeadline sets the deadline for Read calls.
	SetReadDeadline(t time.Time) error

	// Close closes the connection.
	Close() error
}

const (
	// Direction bits in the 64-bit nonce header.
	dirToServer = uint64(0)          // client → server
	dirToClient = uint64(1) << 63    // server → client
	seqMask     = ^(uint64(1) << 63) // lower 63 bits

	// Minimum wire datagram: 8 (nonce) + 16 (tag) = 24 bytes.
	minDatagram = 24

	// Maximum payload per UDP datagram.
	maxPayload = 16384

	// Tick interval matches mosh's SEND_MINDELAY (8ms) for keystroke batching.
	tickInterval = 8 * time.Millisecond
)
