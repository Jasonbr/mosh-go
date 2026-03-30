//go:build js && wasm

package main

import (
	"errors"
	"sync"
	"syscall/js"
	"time"
)

// wtConn wraps a browser WebTransport connection as a mosh.Conn.
type wtConn struct {
	transport js.Value
	writer    js.Value // persistent writable stream writer
	incoming  chan []byte
	done      chan struct{}
	once      sync.Once

	mu       sync.Mutex
	deadline time.Time
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
	}

	// Start reading datagrams via a persistent JS callback.
	c.startReader()

	return c, nil
}

// startReader sets up a self-chaining read loop using persistent callbacks.
func (c *wtConn) startReader() {
	reader := c.transport.Get("datagrams").Get("readable").Call("getReader")

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
		select {
		case c.incoming <- buf:
		default:
		}
		// Defer next read to next event loop tick so Go goroutines can process.
		js.Global().Call("setTimeout", readNext, 0)
		return nil
	})

	onErr = js.FuncOf(func(this js.Value, args []js.Value) interface{} {
		// Stream error or closed — stop reading.
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
		c.transport.Call("close")
	})
	return nil
}
