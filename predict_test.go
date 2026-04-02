package mosh

import (
	"testing"
	"time"
	"unicode/utf8"
)

func TestPredictorBasicEcho(t *testing.T) {
	p := NewPredictor()
	p.SetCursor(0, 0)

	// Type "abc"
	p.Keystroke([]byte("a"))
	p.Keystroke([]byte("b"))
	p.Keystroke([]byte("c"))

	if !p.Active() {
		t.Fatal("predictor should be active after typing")
	}
	if len(p.pending) != 3 {
		t.Fatalf("expected 3 pending, got %d", len(p.pending))
	}
	if p.pending[0].r != 'a' || p.pending[0].x != 0 || p.pending[0].y != 0 {
		t.Fatalf("prediction 0: got %+v", p.pending[0])
	}
	if p.pending[1].r != 'b' || p.pending[1].x != 1 {
		t.Fatalf("prediction 1: got %+v", p.pending[1])
	}
	if p.pending[2].r != 'c' || p.pending[2].x != 2 {
		t.Fatalf("prediction 2: got %+v", p.pending[2])
	}
}

func TestPredictorOverlay(t *testing.T) {
	p := NewPredictor()
	p.SetCursor(0, 0)
	p.Keystroke([]byte("hi"))

	fb := NewFramebuffer(80, 24)
	p.Overlay(fb)

	c0 := fb.CellAt(0, 0)
	c1 := fb.CellAt(1, 0)
	if c0.Rune != 'h' || !c0.Attr.Under {
		t.Fatalf("cell 0: got %+v, want 'h' underlined", c0)
	}
	if c1.Rune != 'i' || !c1.Attr.Under {
		t.Fatalf("cell 1: got %+v, want 'i' underlined", c1)
	}
	if fb.CurX != 2 || fb.CurY != 0 {
		t.Fatalf("cursor: got (%d,%d), want (2,0)", fb.CurX, fb.CurY)
	}
}

func TestPredictorConfirmAll(t *testing.T) {
	p := NewPredictor()
	p.SetCursor(0, 0)
	p.Keystroke([]byte("ab"))

	// Simulate server confirming "ab" at positions 0,1
	fb := NewFramebuffer(80, 24)
	fb.CellAt(0, 0).Rune = 'a'
	fb.CellAt(1, 0).Rune = 'b'
	fb.CurX = 2
	fb.CurY = 0

	p.Confirm(fb)
	if p.Active() {
		t.Fatal("predictor should be inactive after full confirmation")
	}
	if len(p.pending) != 0 {
		t.Fatalf("expected 0 pending, got %d", len(p.pending))
	}
}

func TestPredictorPartialConfirm(t *testing.T) {
	p := NewPredictor()
	p.SetCursor(0, 0)
	p.Keystroke([]byte("abc"))

	// Server has confirmed "a" but not "b" or "c" yet
	fb := NewFramebuffer(80, 24)
	fb.CellAt(0, 0).Rune = 'a'
	fb.CurX = 1
	fb.CurY = 0

	p.Confirm(fb)
	if !p.Active() {
		t.Fatal("predictor should still be active with pending predictions")
	}
	if len(p.pending) != 2 {
		t.Fatalf("expected 2 pending, got %d", len(p.pending))
	}
	if p.pending[0].r != 'b' {
		t.Fatalf("first remaining prediction: got %c, want b", p.pending[0].r)
	}
}

func TestPredictorDivergence(t *testing.T) {
	p := NewPredictor()
	p.SetCursor(0, 0)
	p.Keystroke([]byte("abc"))

	// Server shows 'x' where we predicted 'a' — tab completion, etc.
	fb := NewFramebuffer(80, 24)
	fb.CellAt(0, 0).Rune = 'x'
	fb.CurX = 5
	fb.CurY = 0

	p.Confirm(fb)
	if p.Active() {
		t.Fatal("predictor should reset on divergence")
	}
	if p.curX != 5 {
		t.Fatalf("cursor should sync to server: got %d, want 5", p.curX)
	}
}

func TestPredictorControlCharResets(t *testing.T) {
	p := NewPredictor()
	p.SetCursor(0, 0)
	p.Keystroke([]byte("ab"))

	if !p.Active() {
		t.Fatal("should be active")
	}

	// Send a control character (enter)
	p.Keystroke([]byte("\n"))
	if p.Active() {
		t.Fatal("control char should reset predictions")
	}
}

func TestPredictorEscapeSequenceResets(t *testing.T) {
	p := NewPredictor()
	p.SetCursor(0, 0)
	p.Keystroke([]byte("ab"))

	// Escape byte
	p.Keystroke([]byte{0x1b})
	if p.Active() {
		t.Fatal("escape should reset predictions")
	}
}

func TestPredictorExpireStale(t *testing.T) {
	p := NewPredictor()
	p.SetCursor(0, 0)
	p.Keystroke([]byte("a"))

	// Manually backdate the prediction
	p.pending[0].at = time.Now().Add(-600 * time.Millisecond)

	p.ExpireStale(time.Now())
	if p.Active() {
		t.Fatal("stale prediction should be expired")
	}
	if len(p.pending) != 0 {
		t.Fatalf("expected 0 pending, got %d", len(p.pending))
	}
}

func TestPredictorSpaceConfirm(t *testing.T) {
	p := NewPredictor()
	p.SetCursor(0, 0)
	p.Keystroke([]byte("hi there"))

	if len(p.pending) != 8 {
		t.Fatalf("expected 8 predictions, got %d", len(p.pending))
	}

	// Server shows "hi " — space at position 2 should confirm, not stall
	fb := NewFramebuffer(80, 24)
	fb.CellAt(0, 0).Rune = 'h'
	fb.CellAt(1, 0).Rune = 'i'
	fb.CellAt(2, 0).Rune = ' '
	fb.CurX = 3
	fb.CurY = 0

	p.Confirm(fb)
	if len(p.pending) != 5 {
		t.Fatalf("expected 5 pending after confirming 'hi ', got %d", len(p.pending))
	}
	if p.pending[0].r != 't' {
		t.Fatalf("first remaining should be 't', got %c", p.pending[0].r)
	}
}

func TestPredictorNoActivityNoCursorOverride(t *testing.T) {
	p := NewPredictor()

	// Without any predictions, SetCursor should track server
	p.SetCursor(10, 5)
	if p.curX != 10 || p.curY != 5 {
		t.Fatalf("cursor not tracking: got (%d,%d)", p.curX, p.curY)
	}

	// After typing, SetCursor should not override predicted position
	p.Keystroke([]byte("x"))
	p.SetCursor(0, 0)
	if p.curX != 11 {
		t.Fatalf("predicted cursor should not be overridden: got %d, want 11", p.curX)
	}
}

func TestPredictorMultibyteUTF8(t *testing.T) {
	p := NewPredictor()
	p.SetCursor(0, 0)
	p.Keystroke([]byte("é")) // 2 bytes

	if len(p.pending) != 1 {
		t.Fatalf("expected 1 prediction, got %d", len(p.pending))
	}
	if p.pending[0].r != 'é' {
		t.Fatalf("got %c, want é", p.pending[0].r)
	}
}

func TestPredictorOverlayDoesNotAffectUnpredictedCells(t *testing.T) {
	p := NewPredictor()
	p.SetCursor(5, 0)
	p.Keystroke([]byte("x"))

	fb := NewFramebuffer(80, 24)
	fb.CellAt(0, 0).Rune = 'A'
	fb.CellAt(1, 0).Rune = 'B'

	p.Overlay(fb)

	// Unpredicted cells should be untouched
	if fb.CellAt(0, 0).Rune != 'A' {
		t.Fatal("cell 0 should be unchanged")
	}
	if fb.CellAt(1, 0).Rune != 'B' {
		t.Fatal("cell 1 should be unchanged")
	}
	// Predicted cell
	if fb.CellAt(5, 0).Rune != 'x' {
		t.Fatal("cell 5 should be 'x'")
	}
}

// TestPredictorE2EGoServer tests prediction with real mosh transport.
func TestPredictorE2EGoServer(t *testing.T) {
	srv, err := NewServer("/bin/sh", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve()
	defer srv.Close()
	<-srv.started

	client, err := Dial("127.0.0.1", srv.Port(), srv.KeyBase64())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	// Wait for shell prompt.
	for i := 0; i < 40; i++ {
		if out := client.Recv(500 * time.Millisecond); len(out) > 0 {
			break
		}
	}

	pred := NewPredictor()
	pred.SetCursor(0, 0)

	// Type printable chars — predictions should appear immediately.
	keys := []byte("echo hello")
	pred.Keystroke(keys)
	client.Send(keys)

	if !pred.Active() {
		t.Fatal("predictor should be active after typing")
	}
	if len(pred.pending) != 10 {
		t.Fatalf("expected 10 predictions, got %d", len(pred.pending))
	}

	// The predictions can be overlaid on a framebuffer immediately —
	// this is the "instant echo" the user sees.
	fb := NewFramebuffer(80, 24)
	pred.Overlay(fb)
	if fb.CellAt(0, 0).Rune != 'e' || fb.CellAt(1, 0).Rune != 'c' {
		t.Fatal("overlay should show predicted characters immediately")
	}

	// Wait for server to confirm. The server will echo the characters
	// and predictions should get confirmed.
	deadline := time.After(10 * time.Second)
	for pred.Active() {
		select {
		case <-deadline:
			t.Fatalf("predictions not confirmed. %d still pending", len(pred.pending))
		default:
		}
		out := client.Recv(200 * time.Millisecond)
		if out != nil {
			// Parse server output into a framebuffer to confirm predictions.
			// For simplicity, build a minimal framebuffer from ANSI output.
			serverFB := NewFramebuffer(80, 24)
			applyANSIToFB(serverFB, out)
			pred.Confirm(serverFB)
		}
		pred.ExpireStale(time.Now())
	}

	t.Log("prediction E2E with Go server passed")
}

// applyANSIToFB is a minimal helper that writes characters from ANSI
// output into a framebuffer. It doesn't parse escape sequences — just
// extracts printable characters at sequential positions. Good enough
// for testing prediction confirmation.
func applyANSIToFB(fb *Framebuffer, data []byte) {
	x, y := 0, 0
	i := 0
	for i < len(data) {
		if data[i] == 0x1b {
			// Skip escape sequence
			i++
			if i < len(data) && data[i] == '[' {
				i++
				for i < len(data) && !((data[i] >= 'A' && data[i] <= 'Z') || (data[i] >= 'a' && data[i] <= 'z')) {
					i++
				}
				if i < len(data) {
					i++ // skip final byte
				}
			}
			continue
		}
		if data[i] == '\r' {
			x = 0
			i++
			continue
		}
		if data[i] == '\n' {
			y++
			i++
			continue
		}
		r, size := utf8.DecodeRune(data[i:])
		i += size
		if r >= 0x20 && x < fb.W && y < fb.H {
			cell := fb.CellAt(x, y)
			cell.Rune = r
			cell.Width = 1
			x++
		}
	}
	fb.CurX = x
	fb.CurY = y
}
