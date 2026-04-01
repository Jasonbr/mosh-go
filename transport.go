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
	ackedByRemote uint64 // newest state the remote has acknowledged
	pendingDiff    []byte // diff payload waiting to be sent
	diffSent       bool   // true = pendingDiff has been sent at least once
	diffOldNum     uint64 // locked oldNum for all diffs until base advances
	hasPendingBase bool   // true = diffOldNum is locked
	pendingDataAck bool   // true = send ack ASAP (received data, not just ack)

	// Incoming state — list of received state nums for old_num validation.
	receivedNums   []uint64 // ordered list of state nums we have
	ackNum         uint64   // latest received state num
	sentAckNum     uint64   // last ackNum we actually sent on wire
	throwawayNum   uint64   // oldest state we still hold
	lastRecvOldNum uint64   // oldNum from most recently received diff
	lastRecvNewNum uint64   // newNum from most recently received diff

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
		ocb:          ocb,
		rto:          initialRTO,
		lastSend:     time.Now(),
		lastRecv:     time.Now(),
		receivedNums: []uint64{0}, // start with state 0
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

// ForceNextSend forces the next tick to send, even with no pending diff.
func (t *Transport) ForceNextSend() {
	t.mu.Lock()
	t.lastSend = time.Time{} // zero time, always expired
	t.mu.Unlock()
}

// SetPending sets the diff payload to send on the next tick.
func (t *Transport) SetPending(diff []byte) {
	t.mu.Lock()
	if len(diff) > 0 {
		t.diffSent = false
	}
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
	haveNewDiff := haveDiff && !t.diffSent
	needAck := t.ackNum > t.sentAckNum
	sinceLastSend := now.Sub(t.lastSend)
	expired := sinceLastSend >= t.rto
	urgentAck := t.pendingDataAck

	shouldSend := haveNewDiff || needAck || expired || urgentAck
	if !shouldSend {
		return nil
	}

	// Build TransportInstruction.
	if haveNewDiff {
		t.sentNum++
		t.diffSent = true
		if !t.hasPendingBase {
			t.diffOldNum = t.ackedByRemote
			t.hasPendingBase = true
		}
	}
	t.pendingDataAck = false

	oldNum := t.ackedByRemote
	if haveDiff {
		oldNum = t.diffOldNum // use locked oldNum for diff retransmissions
	}

	ti := TransportInstruction{
		ProtocolVersion: 2,
		OldNum:          oldNum,
		NewNum:          t.sentNum,
		AckNum:          t.ackNum,
		ThrowawayNum:    0, // client doesn't tell server to throw away states
		Diff:            t.pendingDiff,
		LatchCaps:       t.localCaps,
	}
	t.sentAckNum = t.ackNum
	// Do NOT nil pendingDiff — keep for retransmission until server acks.

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

	// Process SSP fields (matching upstream mosh recv logic).
	t.mu.Lock()
	defer t.mu.Unlock()

	// Process ack from remote.
	if ti.AckNum > t.ackedByRemote {
		t.ackedByRemote = ti.AckNum
		if t.ackedByRemote >= t.sentNum && t.pendingDiff != nil {
			t.pendingDiff = nil
			t.diffSent = false
			t.hasPendingBase = false
		}
	}

	// Check if we already have new_num (dedup).
	for _, n := range t.receivedNums {
		if n == ti.NewNum {
			return nil
		}
	}

	// Check if we have old_num (required to apply diff).
	hasOld := false
	for _, n := range t.receivedNums {
		if n == ti.OldNum {
			hasOld = true
			break
		}
	}
	if !hasOld {
		return nil
	}

	// Process throwaway.
	if ti.ThrowawayNum > t.throwawayNum {
		t.throwawayNum = ti.ThrowawayNum
		filtered := t.receivedNums[:0]
		for _, n := range t.receivedNums {
			if n >= t.throwawayNum {
				filtered = append(filtered, n)
			}
		}
		t.receivedNums = filtered
	}

	// Track oldNum/newNum for state management.
	t.lastRecvOldNum = ti.OldNum
	t.lastRecvNewNum = ti.NewNum

	// Add new state.
	t.receivedNums = append(t.receivedNums, ti.NewNum)

	// Bound the list.
	if len(t.receivedNums) > 128 {
		t.receivedNums = t.receivedNums[1:]
	}

	// Update ack num to latest received state.
	t.ackNum = ti.NewNum

	// Trigger immediate ack when we receive data.
	if len(ti.Diff) > 0 {
		t.pendingDataAck = true
	}

	return ti.Diff
}

// AckedByRemote returns the highest state number the remote has acked.
func (t *Transport) AckedByRemote() uint64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.ackedByRemote
}

// SentNum returns the current sent state number.
func (t *Transport) SentNum() uint64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.sentNum
}

// LastRecvOldNum returns the oldNum from the most recently received diff.
func (t *Transport) LastRecvOldNum() uint64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.lastRecvOldNum
}

// LastRecvNewNum returns the newNum from the most recently received diff.
func (t *Transport) LastRecvNewNum() uint64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.lastRecvNewNum
}

// ThrowawayNum returns the server's throwaway number — states below this
// are no longer referenced by the server and can be safely pruned.
func (t *Transport) ThrowawayNum() uint64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.throwawayNum
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
