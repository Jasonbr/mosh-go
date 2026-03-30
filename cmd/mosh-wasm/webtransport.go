//go:build js && wasm

package main

import (
	"errors"
	"sync"
	"syscall/js"
	"time"
)

// wtConn wraps a browser WebTransport connection as a mosh.Conn.
// Uses WebTransport datagrams for unreliable, unordered delivery —
// matching UDP semantics for the mosh protocol.
type wtConn struct {
	transport js.Value // WebTransport instance
	reader    js.Value // datagrams.readable reader
	writer    js.Value // datagrams.writable writer

	incoming chan []byte
	done     chan struct{}
	once     sync.Once

	mu       sync.Mutex
	deadline time.Time
}

func dialWebTransport(url string) (*wtConn, error) {
	wt := js.Global().Get("WebTransport").New(url)

	// Wait for .ready promise
	readyCh := make(chan error, 1)
	wt.Get("ready").Call("then",
		js.FuncOf(func(this js.Value, args []js.Value) interface{} {
			readyCh <- nil
			return nil
		}),
		js.FuncOf(func(this js.Value, args []js.Value) interface{} {
			msg := "WebTransport connection failed"
			if len(args) > 0 {
				msg = args[0].Get("message").String()
			}
			readyCh <- errors.New(msg)
			return nil
		}),
	)

	select {
	case err := <-readyCh:
		if err != nil {
			return nil, err
		}
	case <-time.After(10 * time.Second):
		wt.Call("close")
		return nil, errors.New("WebTransport connect timeout")
	}

	datagrams := wt.Get("datagrams")
	reader := datagrams.Get("readable").Call("getReader")
	writer := datagrams.Get("writable").Call("getWriter")

	c := &wtConn{
		transport: wt,
		reader:    reader,
		writer:    writer,
		incoming:  make(chan []byte, 256),
		done:      make(chan struct{}),
	}

	// Start reading datagrams in background
	go c.readLoop()

	return c, nil
}

func (c *wtConn) readLoop() {
	for {
		ch := make(chan js.Value, 1)
		c.reader.Call("read").Call("then",
			js.FuncOf(func(this js.Value, args []js.Value) interface{} {
				ch <- args[0]
				return nil
			}),
			js.FuncOf(func(this js.Value, args []js.Value) interface{} {
				close(ch)
				return nil
			}),
		)

		select {
		case <-c.done:
			return
		case result, ok := <-ch:
			if !ok {
				return
			}
			if result.Get("done").Bool() {
				return
			}
			value := result.Get("value")
			buf := make([]byte, value.Get("byteLength").Int())
			js.CopyBytesToGo(buf, js.Global().Get("Uint8Array").New(value))
			select {
			case c.incoming <- buf:
			default:
				// drop if channel full
			}
		}
	}
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
	arr := js.Global().Get("Uint8Array").New(len(b))
	js.CopyBytesToJS(arr, b)
	c.writer.Call("write", arr)
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
		c.reader.Call("cancel")
		c.writer.Call("close")
		c.transport.Call("close")
	})
	return nil
}
