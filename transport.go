package mosh

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"io"
	"sync"
	"time"
)

// Transport implements the mosh State Synchronization Protocol (SSP).
//
// It manages sequence numbering, acknowledgements, retransmission timing,
// and the fragment/encrypt/decrypt pipeline. Both client and server use
// the same Transport with different direction bits.
//
// The caller provides state diffs (server: terminal output, client: keystrokes)
// and receives remote state updates.
type Transport struct {
	mu sync.Mutex

	ocb       *OCB
	toRemote  uint64 // direction bit for outgoing (dirToServer or dirToClient)
	toLocal   uint64 // direction bit for incoming

	// Outgoing state (SSP §3).
	sentNum      uint64 // newest state we've sent (new_num)
	ackedNum     uint64 // newest state the remote has acknowledged (ack from remote)
	pendingDiff  []byte // diff payload waiting to be sent

	// Incoming state.
	recvNum      uint64 // newest state we've received from remote (their new_num)
	recvAck      uint64 // what we've told the remote we received (our ack_num)
	throwawayNum uint64 // oldest state we still hold

	// Sequence counter for the crypto layer (independent of SSP state numbering).
	seqOut      uint64
	seqInMax    uint64
	seqInMaxSet bool // false until first datagram received

	// Timestamps.
	lastSend time.Time
	lastRecv time.Time
	lastTS   uint16 // last remote timestamp for echo

	// RTT estimation (Jacobson/Karels).
	srtt    time.Duration
	rttvar  time.Duration
	rto     time.Duration
	rttInit bool

	// Fragment assembler for incoming.
	assembler FragmentAssembler

	// Latch capability negotiation.
	localCaps  []byte
	remoteCaps []byte
}

const (
	initialRTO = 1000 * time.Millisecond
	minRTO     = 250 * time.Millisecond
	maxRTO     = 10 * time.Second
)

// NewTransport creates a transport. isServer determines direction bits.
func NewTransport(ocb *OCB, isServer bool) *Transport {
	t := &Transport{
		ocb:      ocb,
		rto:      initialRTO,
		lastSend: time.Now(),
		lastRecv: time.Now(),
	}
	if isServer {
		t.toRemote = dirToClient
		t.toLocal = dirToServer
	} else {
		t.toRemote = dirToServer
		t.toLocal = dirToClient
	}
	return t
}

func (t *Transport) SetCaps(caps []byte) {
	t.mu.Lock()
	t.localCaps = caps
	t.mu.Unlock()
}

func (t *Transport) RemoteCaps() []byte {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.remoteCaps
}

func (t *Transport) HasCap(bit byte) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.localCaps) == 0 || len(t.remoteCaps) == 0 {
		return false
	}
	idx := 0
	if idx >= len(t.localCaps) || idx >= len(t.remoteCaps) {
		return false
	}
	return (t.localCaps[idx] & t.remoteCaps[idx] & bit) != 0
}

// SetPending sets the diff payload to send on the next tick.
func (t *Transport) SetPending(diff []byte) {
	t.mu.Lock()
	t.pendingDiff = diff
	t.mu.Unlock()
}

// Tick produces outgoing wire datagrams if it's time to send.
// Returns nil if nothing to send.
func (t *Transport) Tick() [][]byte {
	t.mu.Lock()
	defer t.mu.Unlock()

	now := time.Now()

	// Decide if we should send.
	haveDiff := len(t.pendingDiff) > 0
	needAck := t.recvNum > t.recvAck
	expired := now.Sub(t.lastSend) >= t.rto

	if !haveDiff && !needAck && !expired {
		return nil
	}

	// Build TransportInstruction.
	if haveDiff {
		t.sentNum++
	}

	ti := TransportInstruction{
		ProtocolVersion: 2, // mosh protocol version
		OldNum:          t.ackedNum,
		NewNum:          t.sentNum,
		AckNum:          t.recvNum,
		ThrowawayNum:    t.throwawayNum,
		Diff:            t.pendingDiff,
		LatchCaps:       t.localCaps,
	}
	t.recvAck = t.recvNum
	t.pendingDiff = nil

	// Marshal → compress → fragment → encrypt.
	pbData := ti.Marshal()
	compressed := zlibCompress(pbData)
	frags := Fragmentize(t.sentNum, compressed)

	var datagrams [][]byte
	for i := range frags {
		wire := t.encryptFragment(&frags[i], now)
		datagrams = append(datagrams, wire)
	}

	t.lastSend = now
	return datagrams
}

// Recv processes an incoming wire datagram.
// Returns the diff payload if a complete message was reassembled, or nil.
func (t *Transport) Recv(wire []byte) []byte {
	if len(wire) < minDatagram {
		return nil
	}

	dirSeq := binary.BigEndian.Uint64(wire[:8])

	// Verify direction.
	if dirSeq&dirToClient != t.toLocal&dirToClient {
		return nil
	}

	seq := dirSeq & seqMask

	t.mu.Lock()
	if t.seqInMaxSet && seq <= t.seqInMax {
		t.mu.Unlock()
		return nil // replay
	}
	t.mu.Unlock()

	// Decrypt.
	var nonce [12]byte
	copy(nonce[4:], wire[:8])
	plaintext := t.ocb.Decrypt(nonce[:], wire[8:])
	if plaintext == nil {
		return nil
	}

	// Parse timestamp header (4 bytes).
	if len(plaintext) < 4 {
		return nil
	}
	remoteTS := binary.BigEndian.Uint16(plaintext[0:])
	// plaintext[2:4] is timestamp_reply — used for RTT.
	tsReply := binary.BigEndian.Uint16(plaintext[2:])
	payload := plaintext[4:]

	// Update crypto sequence.
	t.mu.Lock()
	t.seqInMax = seq
	t.seqInMaxSet = true
	t.lastRecv = time.Now()
	t.lastTS = remoteTS
	t.mu.Unlock()

	// RTT estimation from timestamp echo.
	if tsReply != 0 {
		t.updateRTT(tsReply)
	}

	// Parse fragment.
	if len(payload) < fragmentHeaderSize {
		// Heartbeat with no fragment — that's fine.
		return nil
	}
	frag, err := UnmarshalFragment(payload)
	if err != nil {
		return nil
	}

	// Reassemble.
	t.mu.Lock()
	msg := t.assembler.Add(frag)
	t.mu.Unlock()
	if msg == nil {
		return nil
	}

	// Decompress → parse TransportInstruction.
	decompressed := zlibDecompress(msg)
	if decompressed == nil {
		return nil
	}
	var ti TransportInstruction
	if err := ti.Unmarshal(decompressed); err != nil {
		return nil
	}

	if len(ti.LatchCaps) > 0 {
		t.mu.Lock()
		t.remoteCaps = ti.LatchCaps
		t.mu.Unlock()
	}

	// Process SSP fields.
	t.mu.Lock()
	if ti.AckNum > t.ackedNum {
		t.ackedNum = ti.AckNum
	}
	if ti.NewNum > t.recvNum {
		t.recvNum = ti.NewNum
	}
	if ti.ThrowawayNum > t.throwawayNum {
		t.throwawayNum = ti.ThrowawayNum
	}
	t.mu.Unlock()

	return ti.Diff
}

// LastRecv returns the time of the last received datagram.
func (t *Transport) LastRecv() time.Time {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.lastRecv
}

// RTO returns the current retransmission timeout.
func (t *Transport) RTO() time.Duration {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.rto
}

// encryptFragment encrypts a fragment and wraps it in the mosh wire format.
// Caller holds t.mu.
func (t *Transport) encryptFragment(f *Fragment, now time.Time) []byte {
	t.seqOut++
	seq := t.seqOut

	dirSeq := t.toRemote | (seq & seqMask)
	var dirSeqBytes [8]byte
	binary.BigEndian.PutUint64(dirSeqBytes[:], dirSeq)

	var nonce [12]byte
	copy(nonce[4:], dirSeqBytes[:])

	// Plaintext: [timestamp:2][timestamp_reply:2][fragment]
	fragWire := f.Marshal()
	ts := uint16(now.UnixMilli() & 0xffff)
	plaintext := make([]byte, 4+len(fragWire))
	binary.BigEndian.PutUint16(plaintext[0:], ts)
	binary.BigEndian.PutUint16(plaintext[2:], t.lastTS)
	copy(plaintext[4:], fragWire)

	tagAndCT := t.ocb.Encrypt(nonce[:], plaintext)

	wire := make([]byte, 8+len(tagAndCT))
	copy(wire[:8], dirSeqBytes[:])
	copy(wire[8:], tagAndCT)
	return wire
}

// updateRTT updates the RTT estimate from a timestamp echo.
func (t *Transport) updateRTT(tsReply uint16) {
	now16 := uint16(time.Now().UnixMilli() & 0xffff)
	// Compute RTT in milliseconds, handling 16-bit wraparound.
	rttMS := int(now16) - int(tsReply)
	if rttMS < 0 {
		rttMS += 65536
	}
	if rttMS > 30000 {
		return // implausible
	}
	rtt := time.Duration(rttMS) * time.Millisecond

	t.mu.Lock()
	defer t.mu.Unlock()

	if !t.rttInit {
		t.srtt = rtt
		t.rttvar = rtt / 2
		t.rttInit = true
	} else {
		// RFC 6298 Jacobson/Karels.
		delta := t.srtt - rtt
		if delta < 0 {
			delta = -delta
		}
		t.rttvar = (3*t.rttvar + delta) / 4
		t.srtt = (7*t.srtt + rtt) / 8
	}

	t.rto = t.srtt + 4*t.rttvar
	if t.rto < minRTO {
		t.rto = minRTO
	}
	if t.rto > maxRTO {
		t.rto = maxRTO
	}
}

// zlibCompress compresses data with zlib (default level).
func zlibCompress(data []byte) []byte {
	var buf bytes.Buffer
	w := zlib.NewWriter(&buf)
	w.Write(data)
	w.Close()
	return buf.Bytes()
}

// zlibDecompress decompresses zlib data. Returns nil on error.
// Limits output to 1 MiB to prevent decompression bombs.
func zlibDecompress(data []byte) []byte {
	r, err := zlib.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil
	}
	defer r.Close()
	out, err := io.ReadAll(io.LimitReader(r, 1<<20))
	if err != nil {
		return nil
	}
	return out
}
