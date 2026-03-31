//go:build js && wasm

package main

import (
	"encoding/base64"
	"fmt"
	"sync"
	"syscall/js"
	"time"

	mosh "github.com/unixshells/mosh-go"
)

func main() {
	js.Global().Set("moshConnect", js.FuncOf(moshConnect))
	select {} // keep alive
}

// moshConnect(url, key) → Promise<MoshSession>
// url: WebTransport URL (e.g., "https://relay.example.com/mosh/user/device")
// key: base64-encoded mosh key
func moshConnect(this js.Value, args []js.Value) interface{} {
	if len(args) < 2 {
		return reject("moshConnect requires (url, key)")
	}

	url := args[0].String()
	key := args[1].String()

	handler := js.FuncOf(func(this js.Value, pargs []js.Value) interface{} {
		resolve := pargs[0]
		rejectFn := pargs[1]

		go func() {
			session, err := newSession(url, key)
			if err != nil {
				rejectFn.Invoke(err.Error())
				return
			}
			resolve.Invoke(session.jsObject())
		}()

		return nil
	})

	return js.Global().Get("Promise").New(handler)
}

type session struct {
	client *mosh.Client
	conn   *wtConn
	mu     sync.Mutex
	closed bool
}

func newSession(url, key string) (*session, error) {
	for len(key)%4 != 0 {
		key += "="
	}
	rawKey, err := base64.StdEncoding.DecodeString(key)
	if err != nil {
		return nil, fmt.Errorf("bad key: %w", err)
	}
	ocb, err := mosh.NewOCB(rawKey)
	if err != nil {
		return nil, err
	}

	conn, err := dialWebTransport(url)
	if err != nil {
		return nil, err
	}

	// Use DialConnManual — no internal sendLoop. JS drives Tick() via setInterval.
	client, err := mosh.DialConnManual(conn, ocb)
	if err != nil {
		conn.Close()
		return nil, err
	}

	return &session{client: client, conn: conn}, nil
}

func (s *session) jsObject() js.Value {
	obj := js.Global().Get("Object").New()

	// Keystrokes are queued via Send(). The JS interval flushes them.
	// Do NOT tick from send — the WebTransport ack round-trip is ~50ms,
	// and ticking per-keystroke creates duplicate sequence numbers.
	obj.Set("send", js.FuncOf(func(this js.Value, args []js.Value) interface{} {
		if len(args) < 1 {
			return nil
		}
		s.client.Send([]byte(args[0].String()))
		return nil
	}))

	obj.Set("resize", js.FuncOf(func(this js.Value, args []js.Value) interface{} {
		if len(args) < 2 {
			return nil
		}
		s.client.Resize(uint16(args[0].Int()), uint16(args[1].Int()))
		return nil
	}))

	// tick() — send keepalive. Called by JS setInterval at a slow rate.
	obj.Set("tick", js.FuncOf(func(this js.Value, args []js.Value) interface{} {
		s.client.Tick()
		return nil
	}))

	// recv(callback) — calls callback(string) whenever output is available
	obj.Set("recv", js.FuncOf(func(this js.Value, args []js.Value) interface{} {
		if len(args) < 1 {
			return nil
		}
		cb := args[0]
		go func() {
			for {
				out := s.client.Recv(60 * time.Second)
				if out == nil {
					s.mu.Lock()
					closed := s.closed
					s.mu.Unlock()
					if closed {
						return
					}
					continue
				}
				cb.Invoke(string(out))
			}
		}()
		return nil
	}))

	obj.Set("close", js.FuncOf(func(this js.Value, args []js.Value) interface{} {
		s.mu.Lock()
		s.closed = true
		s.mu.Unlock()
		s.client.Close()
		return nil
	}))

	return obj
}

func reject(msg string) js.Value {
	return js.Global().Get("Promise").Call("reject", msg)
}
