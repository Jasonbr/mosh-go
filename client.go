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
	conn      *net.UDPConn
	transport *Transport
	ocb       *OCB

	mu      sync.Mutex
	output  []byte // accumulated terminal output
	outputC chan struct{}

	done chan struct{}
	wg   sync.WaitGroup
}

// Dial connects to a mosh server. The key is the base64-encoded mosh key
// (with or without padding).
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

	c := &Client{
		conn:      conn,
		transport: NewTransport(ocb, false),
		ocb:       ocb,
		outputC:   make(chan struct{}, 1),
		done:      make(chan struct{}),
	}

	c.wg.Add(2)
	go c.recvLoop()
	go c.sendLoop()

	// Send initial empty datagram to associate with the server.
	c.tick()

	return c, nil
}

// Send sends keystrokes to the server.
func (c *Client) Send(keys []byte) {
	diff := MarshalUserMessage([]UserInstruction{{Keys: keys}})
	c.transport.SetPending(diff)
	c.tick()
}

// Resize sends a resize to the server.
func (c *Client) Resize(cols, rows uint16) {
	diff := MarshalUserMessage([]UserInstruction{{
		Width:  int32(cols),
		Height: int32(rows),
	}})
	c.transport.SetPending(diff)
	c.tick()
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
	for _, dg := range c.transport.Tick() {
		c.conn.Write(dg)
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
