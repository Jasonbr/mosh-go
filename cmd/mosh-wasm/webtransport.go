//go:build js && wasm

package main

import (
	"errors"
	"strings"
	"sync"
	"syscall/js"
	"time"
)

// wtConn wraps a browser WebTransport connection as a mosh.Conn.
// Supports transparent reconnection: when the underlying WebTransport
// dies, reads return timeouts while the worker re-dials. Once a new
// connection is swapped in via Reconnect(), reads resume.
type wtConn struct {
	mu        sync.Mutex
	transport js.Value
	writer    js.Value // persistent writable stream writer
	incoming  chan []byte
	done      chan struct{}
	once      sync.Once
	deadline  time.Time

	url   string
	token string // session token from relay, used for reconnect
}

func dialWebTransport(url string) (*wtConn, error) {
	wt := js.Global().Get("WebTransport").New(url)

	readyCh := make(chan error, 1)
	onReady := js.FuncOf(func(this js.Value, args []js.Value) interface{} {
		readyCh <- nil
		return nil
	})
	onFail := js.FuncOf(func(this js.Value, args []js.Value) interface{} {
		msg := "WebTransport connection failed"
		if len(args) > 0 && !args[0].IsUndefined() && !args[0].IsNull() {
			if m := args[0].Get("message"); !m.IsUndefined() {
				msg = m.String()
			}
		}
		readyCh <- errors.New(msg)
		return nil
	})
	wt.Get("ready").Call("then", onReady, onFail)

	select {
	case err := <-readyCh:
		if err != nil {
			onReady.Release()
			onFail.Release()
			return nil, err
		}
	case <-time.After(10 * time.Second):
		onReady.Release()
		onFail.Release()
		wt.Call("close")
		return nil, errors.New("WebTransport connect timeout")
	}
	onReady.Release()
	onFail.Release()

	writer := wt.Get("datagrams").Get("writable").Call("getWriter")

	c := &wtConn{
		transport: wt,
		writer:    writer,
		incoming:  make(chan []byte, 256),
		done:      make(chan struct{}),
		url:       url,
	}

	// Start reading datagrams via a persistent JS callback.
	c.startReader()

	return c, nil
}

// Reconnect dials a new WebTransport connection using the session token
// and swaps it into this conn. The mosh client keeps its SSP state and
// resumes transparently.
func (c *wtConn) Reconnect() error {
	c.mu.Lock()
	token := c.token
	url := c.url
	c.mu.Unlock()

	if token == "" {
		return errors.New("no session token")
	}

	// Add token to URL for reconnect.
	sep := "&"
	if !strings.Contains(url, "?") {
		sep = "?"
	}
	reconnURL := url + sep + "token=" + token

	wt := js.Global().Get("WebTransport").New(reconnURL)

	readyCh := make(chan error, 1)
	onReady := js.FuncOf(func(this js.Value, args []js.Value) interface{} {
		readyCh <- nil
		return nil
	})
	onFail := js.FuncOf(func(this js.Value, args []js.Value) interface{} {
		msg := "reconnect failed"
		if len(args) > 0 && !args[0].IsUndefined() && !args[0].IsNull() {
			if m := args[0].Get("message"); !m.IsUndefined() {
				msg = m.String()
			}
		}
		readyCh <- errors.New(msg)
		return nil
	})
	wt.Get("ready").Call("then", onReady, onFail)

	select {
	case err := <-readyCh:
		if err != nil {
			onReady.Release()
			onFail.Release()
			return err
		}
	case <-time.After(10 * time.Second):
		onReady.Release()
		onFail.Release()
		wt.Call("close")
		return errors.New("reconnect timeout")
	}
	onReady.Release()
	onFail.Release()

	writer := wt.Get("datagrams").Get("writable").Call("getWriter")

	// Swap in the new connection.
	c.mu.Lock()
	oldTransport := c.transport
	c.transport = wt
	c.writer = writer
	c.mu.Unlock()

	// Close old transport (best-effort).
	if !oldTransport.IsUndefined() && !oldTransport.IsNull() {
		defer func() { recover() }()
		oldTransport.Call("close")
	}

	// Start reading from the new connection.
	c.startReader()

	return nil
}

// Token returns the session token received from the relay.
func (c *wtConn) Token() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.token
}

// startReader sets up a self-chaining read loop using persistent callbacks.
func (c *wtConn) startReader() {
	c.mu.Lock()
	reader := c.transport.Get("datagrams").Get("readable").Call("getReader")
	c.mu.Unlock()

	var onData, onErr js.Func
	var readNext js.Func

	readNext = js.FuncOf(func(this js.Value, args []js.Value) interface{} {
		reader.Call("read").Call("then", onData, onErr)
		return nil
	})

	onData = js.FuncOf(func(this js.Value, args []js.Value) interface{} {
		result := args[0]
		if result.Get("done").Bool() {
			return nil
		}
		value := result.Get("value")
		buf := make([]byte, value.Get("byteLength").Int())
		js.CopyBytesToGo(buf, js.Global().Get("Uint8Array").New(value))

		// Check for TOKEN: prefix (session token from relay).
		if len(buf) > 6 && string(buf[:6]) == "TOKEN:" {
			c.mu.Lock()
			c.token = string(buf[6:])
			c.mu.Unlock()
		} else {
			select {
			case c.incoming <- buf:
			default:
			}
		}

		// Defer next read to next event loop tick so Go goroutines can process.
		js.Global().Call("setTimeout", readNext, 0)
		return nil
	})

	onErr = js.FuncOf(func(this js.Value, args []js.Value) interface{} {
		// Stream error or closed — stop reading.
		// The reconnect logic in the worker will handle this.
		return nil
	})

	// Start first read.
	readNext.Invoke()
}

func (c *wtConn) Read(b []byte) (int, error) {
	c.mu.Lock()
	deadline := c.deadline
	c.mu.Unlock()

	var timer <-chan time.Time
	if !deadline.IsZero() {
		d := time.Until(deadline)
		if d <= 0 {
			return 0, errors.New("i/o timeout")
		}
		timer = time.After(d)
	}

	select {
	case <-c.done:
		return 0, errors.New("connection closed")
	case data := <-c.incoming:
		n := copy(b, data)
		return n, nil
	case <-timer:
		return 0, errors.New("i/o timeout")
	}
}

func (c *wtConn) Write(b []byte) (int, error) {
	c.mu.Lock()
	writer := c.writer
	c.mu.Unlock()

	if writer.IsUndefined() || writer.IsNull() {
		return 0, errors.New("not connected")
	}

	arr := js.Global().Get("Uint8Array").New(len(b))
	js.CopyBytesToJS(arr, b)
	writer.Call("write", arr)
	return len(b), nil
}

func (c *wtConn) SetReadDeadline(t time.Time) error {
	c.mu.Lock()
	c.deadline = t
	c.mu.Unlock()
	return nil
}

func (c *wtConn) Close() error {
	c.once.Do(func() {
		close(c.done)
		c.mu.Lock()
		t := c.transport
		c.mu.Unlock()
		if !t.IsUndefined() && !t.IsNull() {
			t.Call("close")
		}
	})
	return nil
}
