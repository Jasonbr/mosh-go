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

// moshConnect(url, key, cols, rows) → Promise<MoshSession>
func moshConnect(this js.Value, args []js.Value) interface{} {
	if len(args) < 4 {
		return reject("moshConnect requires (url, key, cols, rows)")
	}

	url := args[0].String()
	key := args[1].String()
	cols := args[2].Int()
	rows := args[3].Int()

	handler := js.FuncOf(func(this js.Value, pargs []js.Value) interface{} {
		resolve := pargs[0]
		rejectFn := pargs[1]

		go func() {
			session, err := newSession(url, key, cols, rows)
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
	client  *mosh.Client
	conn    *wtConn
	tracker *stateTracker
	mu      sync.Mutex
	closed  bool
}

func newSession(url, key string, cols, rows int) (*session, error) {
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

	client, err := mosh.DialConnRaw(conn, ocb)
	if err != nil {
		conn.Close()
		return nil, err
	}

	s := &session{
		client:  client,
		conn:    conn,
		tracker: newStateTracker(cols, rows),
	}

	// Background goroutine: read from mosh client, feed to state tracker.
	go s.recvLoop()

	return s, nil
}

func (s *session) jsObject() js.Value {
	obj := js.Global().Get("Object").New()

	obj.Set("send", js.FuncOf(func(this js.Value, args []js.Value) interface{} {
		if len(args) < 1 {
			return nil
		}
		data := []byte(args[0].String())
		s.client.Send(data)
		s.client.Tick()

		// Feed input to predictor — returns immediate overlay output.
		if out := s.tracker.keystroke(data); len(out) > 0 {
			return string(out)
		}
		return js.Null()
	}))

	obj.Set("resize", js.FuncOf(func(this js.Value, args []js.Value) interface{} {
		if len(args) < 2 {
			return nil
		}
		cols := args[0].Int()
		rows := args[1].Int()
		s.client.Resize(uint16(cols), uint16(rows))
		s.tracker.resize(cols, rows)
		s.client.Tick()
		return nil
	}))

	// tick() — flush pending keystrokes + keepalive/retransmit.
	obj.Set("tick", js.FuncOf(func(this js.Value, args []js.Value) interface{} {
		s.client.Tick()
		return nil
	}))

	// poll() — returns pre-diffed ANSI output from the state tracker.
	obj.Set("poll", js.FuncOf(func(this js.Value, args []js.Value) interface{} {
		out := s.tracker.poll()
		if out == nil {
			return js.Null()
		}
		return string(out)
	}))

	// reconnect() — swap WebTransport connection using session token.
	obj.Set("reconnect", js.FuncOf(func(this js.Value, args []js.Value) interface{} {
		handler := js.FuncOf(func(this js.Value, pargs []js.Value) interface{} {
			resolve := pargs[0]
			rejectFn := pargs[1]
			go func() {
				if err := s.conn.Reconnect(); err != nil {
					rejectFn.Invoke(err.Error())
					return
				}
				resolve.Invoke(true)
			}()
			return nil
		})
		return js.Global().Get("Promise").New(handler)
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

// recvLoop reads raw transport diffs and feeds them to the state
// tracker for framebuffer-based processing.
func (s *session) recvLoop() {
	for {
		diff := s.client.RecvRaw(1 * time.Second)
		if diff == nil {
			s.mu.Lock()
			closed := s.closed
			s.mu.Unlock()
			if closed {
				return
			}
			continue
		}
		t := s.client.Transport()
		oldNum := t.LastRecvOldNum()
		newNum := t.LastRecvNewNum()
		throwawayNum := t.ThrowawayNum()
		s.tracker.applyDiff(diff, oldNum, newNum, throwawayNum)
	}
}

func reject(msg string) js.Value {
	return js.Global().Get("Promise").Call("reject", msg)
}
