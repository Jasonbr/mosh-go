package mosh

import (
	"bytes"
	"compress/zlib"
	"crypto/rand"
	"encoding/binary"
	"testing"
	"time"
)

func newTestPair(t *testing.T) (*Transport, *Transport) {
	t.Helper()
	key := make([]byte, 16)
	rand.Read(key)
	ocbS, err := NewOCB(key)
	if err != nil {
		t.Fatal(err)
	}
	ocbC, err := NewOCB(key)
	if err != nil {
		t.Fatal(err)
	}
	server := NewTransport(ocbS, true)
	client := NewTransport(ocbC, false)
	return server, client
}

func TestTransportBasicExchange(t *testing.T) {
	server, client := newTestPair(t)

	// Server sends a diff.
	server.SetPending([]byte("hello from server"))
	datagrams := server.Tick()
	if len(datagrams) == 0 {
		t.Fatal("no datagrams")
	}

	// Client receives.
	var diff []byte
	for _, dg := range datagrams {
		if d := client.Recv(dg); d != nil {
			diff = d
		}
	}
	if !bytes.Equal(diff, []byte("hello from server")) {
		t.Fatalf("diff = %q", diff)
	}

	// Client sends a diff.
	client.SetPending([]byte("hello from client"))
	datagrams = client.Tick()
	if len(datagrams) == 0 {
		t.Fatal("no datagrams")
	}

	// Server receives.
	diff = nil
	for _, dg := range datagrams {
		if d := server.Recv(dg); d != nil {
			diff = d
		}
	}
	if !bytes.Equal(diff, []byte("hello from client")) {
		t.Fatalf("diff = %q", diff)
	}
}

func TestTransportReplayRejected(t *testing.T) {
	server, client := newTestPair(t)

	server.SetPending([]byte("data"))
	datagrams := server.Tick()

	// First receive works.
	if d := client.Recv(datagrams[0]); d == nil {
		t.Fatal("first receive failed")
	}

	// Replay should be rejected.
	if d := client.Recv(datagrams[0]); d != nil {
		t.Fatal("replay should be rejected")
	}
}

func TestTransportWrongDirection(t *testing.T) {
	server, _ := newTestPair(t)

	server.SetPending([]byte("data"))
	datagrams := server.Tick()

	// Server receiving its own datagram (wrong direction).
	if d := server.Recv(datagrams[0]); d != nil {
		t.Fatal("wrong direction should be rejected")
	}
}

func TestTransportLargePayload(t *testing.T) {
	server, client := newTestPair(t)

	// Payload larger than one fragment.
	payload := make([]byte, maxFragmentPayload*3+42)
	rand.Read(payload)

	server.SetPending(payload)
	datagrams := server.Tick()
	if len(datagrams) < 2 {
		t.Fatalf("expected multiple datagrams, got %d", len(datagrams))
	}

	var diff []byte
	for _, dg := range datagrams {
		if d := client.Recv(dg); d != nil {
			diff = d
		}
	}
	if !bytes.Equal(diff, payload) {
		t.Fatal("large payload mismatch")
	}
}

func TestTransportHeartbeat(t *testing.T) {
	server, _ := newTestPair(t)

	// Force lastSend to be old enough to trigger heartbeat.
	server.mu.Lock()
	server.lastSend = time.Now().Add(-2 * server.rto)
	server.mu.Unlock()

	datagrams := server.Tick()
	if len(datagrams) == 0 {
		t.Fatal("expected heartbeat datagram")
	}
}

func TestTransportEmptyTick(t *testing.T) {
	server, _ := newTestPair(t)

	// No pending diff, recent send, no ack needed — should produce nothing.
	datagrams := server.Tick()
	if len(datagrams) != 0 {
		t.Fatalf("expected no datagrams, got %d", len(datagrams))
	}
}

func TestTransportRTOBounds(t *testing.T) {
	server, _ := newTestPair(t)

	rto := server.RTO()
	if rto < minRTO || rto > maxRTO {
		t.Fatalf("RTO = %v, not in [%v, %v]", rto, minRTO, maxRTO)
	}
}

func TestTransportMultipleExchanges(t *testing.T) {
	server, client := newTestPair(t)

	for i := 0; i < 10; i++ {
		payload := make([]byte, 100)
		rand.Read(payload)

		server.SetPending(payload)
		for _, dg := range server.Tick() {
			client.Recv(dg)
		}

		client.SetPending(payload)
		for _, dg := range client.Tick() {
			server.Recv(dg)
		}
	}

	// Verify sequence numbers advanced.
	server.mu.Lock()
	if server.sentNum < 10 {
		t.Fatalf("sentNum = %d", server.sentNum)
	}
	server.mu.Unlock()
}

// TestTransportCompressionBomb verifies that a zlib-compressed payload
// expanding to >1 MiB is rejected without crashing.
func TestTransportCompressionBomb(t *testing.T) {
	key := make([]byte, 16)
	rand.Read(key)
	ocbS, err := NewOCB(key)
	if err != nil {
		t.Fatal(err)
	}
	ocbC, err := NewOCB(key)
	if err != nil {
		t.Fatal(err)
	}
	server := NewTransport(ocbS, true)
	client := NewTransport(ocbC, false)

	// Create a zlib payload that expands to >1 MiB (zeros compress very well).
	bigPayload := make([]byte, 2<<20) // 2 MiB of zeros
	var zbuf bytes.Buffer
	w := zlib.NewWriter(&zbuf)
	w.Write(bigPayload)
	w.Close()
	compressed := zbuf.Bytes()

	// Wrap in a single fragment (final=true, fragment_num=0, id=1).
	var fragWire []byte
	fragWire = make([]byte, fragmentHeaderSize+len(compressed))
	binary.BigEndian.PutUint64(fragWire[0:], 1)                          // id
	binary.BigEndian.PutUint16(fragWire[8:], fragmentFinalBit|uint16(0)) // final, frag 0
	copy(fragWire[fragmentHeaderSize:], compressed)

	// Encrypt as a server->client datagram.
	server.mu.Lock()
	server.seqOut++
	seq := server.seqOut
	dirSeq := dirToClient | (seq & seqMask)
	var dirSeqBytes [8]byte
	binary.BigEndian.PutUint64(dirSeqBytes[:], dirSeq)
	var nonce [12]byte
	copy(nonce[4:], dirSeqBytes[:])

	ts := uint16(time.Now().UnixMilli() & 0xffff)
	plaintext := make([]byte, 4+len(fragWire))
	binary.BigEndian.PutUint16(plaintext[0:], ts)
	binary.BigEndian.PutUint16(plaintext[2:], 0)
	copy(plaintext[4:], fragWire)
	server.mu.Unlock()

	tagAndCT := ocbS.Encrypt(nonce[:], plaintext)
	wire := make([]byte, 8+len(tagAndCT))
	copy(wire[:8], dirSeqBytes[:])
	copy(wire[8:], tagAndCT)

	// Client should reject the bomb — Recv returns nil.
	result := client.Recv(wire)
	if result != nil {
		t.Fatalf("compression bomb should be rejected, got %d bytes", len(result))
	}
}

// TestTransportConcurrentSendRecv launches multiple goroutines sending
// from the client and receiving on the server simultaneously to verify
// there are no data races under -race.
func TestTransportConcurrentSendRecv(t *testing.T) {
	server, client := newTestPair(t)

	const numGoroutines = 10
	errs := make(chan error, numGoroutines*2)

	// Launch sender goroutines — each sends a unique payload.
	for i := 0; i < numGoroutines; i++ {
		go func(id int) {
			payload := make([]byte, 64)
			// Fill with unique pattern so corruption is detectable.
			for j := range payload {
				payload[j] = byte(id)
			}
			client.SetPending(payload)
			datagrams := client.Tick()
			if len(datagrams) == 0 {
				errs <- nil // no datagram is fine if another goroutine won the race
				return
			}
			errs <- nil
		}(i)
	}

	// Launch receiver goroutines — each tries to receive datagrams.
	for i := 0; i < numGoroutines; i++ {
		go func(id int) {
			// Generate a datagram from the client side to receive on server.
			payload := make([]byte, 32)
			for j := range payload {
				payload[j] = byte(id + 100)
			}
			client.SetPending(payload)
			datagrams := client.Tick()
			for _, dg := range datagrams {
				server.Recv(dg)
			}
			errs <- nil
		}(i)
	}

	// Collect all errors.
	for i := 0; i < numGoroutines*2; i++ {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
}

func TestTransportCapsNegotiation(t *testing.T) {
	server, client := newTestPair(t)

	server.SetCaps([]byte{CapSessionControl})
	client.SetCaps([]byte{CapSessionControl})

	// Exchange a message so caps are transmitted.
	server.SetPending([]byte("hello"))
	for _, dg := range server.Tick() {
		client.Recv(dg)
	}
	client.SetPending([]byte("world"))
	for _, dg := range client.Tick() {
		server.Recv(dg)
	}

	// Both should see remote caps now.
	if rc := client.RemoteCaps(); len(rc) == 0 {
		t.Fatal("client has no remote caps")
	}
	if rc := server.RemoteCaps(); len(rc) == 0 {
		t.Fatal("server has no remote caps")
	}

	// Both should agree on the capability.
	if !server.HasCap(CapSessionControl) {
		t.Fatal("server should have CapSessionControl")
	}
	if !client.HasCap(CapSessionControl) {
		t.Fatal("client should have CapSessionControl")
	}

	// Test intersection: server has bit 0x03, client has 0x01 → intersect = 0x01.
	server.SetCaps([]byte{0x03})
	client.SetCaps([]byte{0x01})

	server.SetPending([]byte("a"))
	for _, dg := range server.Tick() {
		client.Recv(dg)
	}
	client.SetPending([]byte("b"))
	for _, dg := range client.Tick() {
		server.Recv(dg)
	}

	if !server.HasCap(0x01) {
		t.Fatal("server should have bit 0x01")
	}
	if server.HasCap(0x02) {
		t.Fatal("server should not have bit 0x02 (client lacks it)")
	}
}

func TestTransportNoCaps(t *testing.T) {
	server, client := newTestPair(t)

	// No caps set — HasCap should return false.
	if server.HasCap(CapSessionControl) {
		t.Fatal("should be false with no caps")
	}
	if client.HasCap(CapSessionControl) {
		t.Fatal("should be false with no caps")
	}

	// Exchange without caps.
	server.SetPending([]byte("data"))
	for _, dg := range server.Tick() {
		client.Recv(dg)
	}

	// RemoteCaps should still be nil.
	if rc := client.RemoteCaps(); rc != nil {
		t.Fatalf("expected nil remote caps, got %x", rc)
	}
}

// TestTransportHighSequenceNumbers verifies that the transport works
// correctly when sequence numbers are very large (near 2^62).
func TestTransportHighSequenceNumbers(t *testing.T) {
	server, client := newTestPair(t)

	// Set server's outgoing sequence counter to near 2^62.
	server.mu.Lock()
	server.seqOut = (1 << 62) - 1
	server.mu.Unlock()

	server.SetPending([]byte("high seq test"))
	datagrams := server.Tick()
	if len(datagrams) == 0 {
		t.Fatal("no datagrams produced")
	}

	var diff []byte
	for _, dg := range datagrams {
		if d := client.Recv(dg); d != nil {
			diff = d
		}
	}
	if !bytes.Equal(diff, []byte("high seq test")) {
		t.Fatalf("diff = %q, want %q", diff, "high seq test")
	}
}
