package mosh

import (
	"encoding/base64"
	"fmt"
	"net"
	"os"
	"sync"
	"time"
)

// Client is a mosh client. It connects to a mosh server over UDP,
// handles the SSP transport, and provides send/recv for terminal I/O.
type Client struct {
	conn      Conn
	transport *Transport
	ocb       *OCB

	mu      sync.Mutex
	output  []byte // accumulated terminal output
	outputC chan struct{}

	// Action tracking for cumulative diffs.
	actionsMu        sync.Mutex
	actions          []UserInstruction
	ackedActionCount int                // how many actions the server has
	lastAcked        uint64
	sentActionCounts map[uint64]int     // sentNum → action count at that state
	dirty            bool               // true = new actions since last tick

	done chan struct{}
	wg   sync.WaitGroup
}

// Dial connects to a mosh server over UDP. The key is the base64-encoded
// mosh key (with or without padding).
func Dial(host string, port int, key string) (*Client, error) {
	// Pad key for base64 if needed.
	for len(key)%4 != 0 {
		key += "="
	}
	rawKey, err := base64.StdEncoding.DecodeString(key)
	if err != nil {
		return nil, fmt.Errorf("mosh: bad key: %w", err)
	}

	ocb, err := NewOCB(rawKey)
	if err != nil {
		return nil, err
	}

	conn, err := net.DialUDP("udp4", nil, &net.UDPAddr{
		IP:   net.ParseIP(host),
		Port: port,
	})
	if err != nil {
		return nil, err
	}

	return DialConn(conn, ocb)
}

// DialConn creates a mosh client over an existing datagram connection.
// Use this with WebTransport or other non-UDP transports.
func DialConn(conn Conn, ocb *OCB) (*Client, error) {
	c := &Client{
		conn:             conn,
		transport:        NewTransport(ocb, false),
		ocb:              ocb,
		outputC:          make(chan struct{}, 1),
		done:             make(chan struct{}),
		sentActionCounts: make(map[uint64]int),
	}

	c.wg.Add(2)
	go c.recvLoop()
	go c.sendLoop()

	// Send initial keepalive to associate with the server.
	c.transport.ForceNextSend()
	c.tick()

	return c, nil
}

// Send sends keystrokes to the server.
func (c *Client) Send(keys []byte) {
	c.actionsMu.Lock()
	c.actions = append(c.actions, UserInstruction{Keys: append([]byte{}, keys...)})
	c.dirty = true
	c.actionsMu.Unlock()
}

// Resize sends a resize to the server.
func (c *Client) Resize(cols, rows uint16) {
	c.actionsMu.Lock()
	c.actions = append(c.actions, UserInstruction{Width: int32(cols), Height: int32(rows)})
	c.dirty = true
	c.actionsMu.Unlock()
}


// Recv reads accumulated terminal output, blocking until output is available
// or the timeout expires. Returns nil on timeout.
func (c *Client) Recv(timeout time.Duration) []byte {
	// Check if output is already available.
	c.mu.Lock()
	if len(c.output) > 0 {
		out := c.output
		c.output = nil
		c.mu.Unlock()
		return out
	}
	c.mu.Unlock()

	// Wait for output or timeout.
	select {
	case <-c.outputC:
	case <-time.After(timeout):
		return nil
	case <-c.done:
		return nil
	}

	c.mu.Lock()
	out := c.output
	c.output = nil
	c.mu.Unlock()
	return out
}

// Close shuts down the client.
func (c *Client) Close() {
	select {
	case <-c.done:
		return
	default:
		close(c.done)
	}
	c.conn.Close()
	c.wg.Wait()
}

// Transport returns the underlying SSP transport for advanced use
// (e.g., capability negotiation).
func (c *Client) Transport() *Transport {
	return c.transport
}

func (c *Client) tick() {
	// Batch: compute diff from accumulated actions.
	c.actionsMu.Lock()
	if c.dirty {
		c.dirty = false
		c.processAcksLocked()
		newActions := c.actions[c.ackedActionCount:]
		c.transport.SetPending(MarshalUserMessage(newActions))
		c.sentActionCounts[c.transport.SentNum()+1] = len(c.actions)
	}
	c.actionsMu.Unlock()

	for _, dg := range c.transport.Tick() {
		c.conn.Write(dg)
	}
}

func (c *Client) processAcksLocked() {
	acked := c.transport.AckedByRemote()
	if acked > c.lastAcked {
		c.lastAcked = acked
		// Only advance base when server caught up (no states in flight).
		if acked >= c.transport.SentNum() {
			if count, ok := c.sentActionCounts[acked]; ok && count > c.ackedActionCount {
				c.ackedActionCount = count
			}
			c.sentActionCounts = make(map[uint64]int)
		}
	}
}

func (c *Client) recvLoop() {
	defer c.wg.Done()
	buf := make([]byte, maxPayload+64)

	for {
		select {
		case <-c.done:
			return
		default:
		}

		c.conn.SetReadDeadline(time.Now().Add(1 * time.Second))
		n, err := c.conn.Read(buf)
		if err != nil {
			if os.IsTimeout(err) {
				continue
			}
			return
		}
		if n < minDatagram {
			continue
		}

		data := make([]byte, n)
		copy(data, buf[:n])

		diff := c.transport.Recv(data)
		if diff == nil {
			continue
		}

		instrs, err := UnmarshalHostMessage(diff)
		if err != nil || len(instrs) == 0 {
			continue
		}

		var output []byte
		for _, hi := range instrs {
			output = append(output, hi.Hoststring...)
		}
		if len(output) == 0 {
			continue
		}

		c.mu.Lock()
		c.output = append(c.output, output...)
		c.mu.Unlock()

		select {
		case c.outputC <- struct{}{}:
		default:
		}
	}
}

func (c *Client) sendLoop() {
	defer c.wg.Done()
	ticker := time.NewTicker(tickInterval)
	defer ticker.Stop()

	for {
		select {
		case <-c.done:
			return
		case <-ticker.C:
			c.tick()
		}
	}
}
