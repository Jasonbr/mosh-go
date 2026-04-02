package mosh

import (
	"time"
	"unicode"
	"unicode/utf8"
)

// Predictor implements mosh-style speculative local echo.
//
// When the user types a printable character, it is predicted to appear
// at the current cursor position and the cursor advances. When the
// server confirms the character (it appears in the server's framebuffer
// at the expected position), the prediction is retired. If the server
// diverges, all predictions are cleared.
//
// This is the core of what makes mosh feel local: keystrokes appear
// instantly even over high-latency connections.
type Predictor struct {
	pending []prediction // unconfirmed predictions, in order typed
	curX    int          // predicted cursor X (columns)
	curY    int          // predicted cursor Y (rows)
	epoch   int          // incremented on reset; stale predictions are ignored
	active  bool         // false until first prediction; cleared on divergence

	// Glitch: when the server confirms a prediction, we note it so the
	// overlay can stop rendering that cell as "speculative".
	confirmed int // count of predictions confirmed so far this epoch
}

type prediction struct {
	r     rune
	x, y  int // where we predicted it would land
	epoch int
	at    time.Time
}

const (
	predictionTimeout = 500 * time.Millisecond
)

// NewPredictor creates a new predictor.
func NewPredictor() *Predictor {
	return &Predictor{}
}

// KeystrokeFrom processes user input and returns any printable runes
// that should be predicted. Non-printable input (control chars, escape
// sequences) clears predictions since we can't know what the server
// will do.
func (p *Predictor) Keystroke(input []byte) {
	i := 0
	for i < len(input) {
		r, size := utf8.DecodeRune(input[i:])
		i += size
		if r == utf8.RuneError {
			p.Reset()
			return
		}

		if r < 0x20 || r == 0x7f {
			// Control character — we can't predict what happens.
			// Backspace, enter, tab, escape sequences, etc.
			p.Reset()
			return
		}

		if unicode.IsPrint(r) {
			p.pending = append(p.pending, prediction{
				r:     r,
				x:     p.curX,
				y:     p.curY,
				epoch: p.epoch,
				at:    time.Now(),
			})
			p.curX++
			p.active = true
		}
	}
}

// Reset clears all predictions (e.g., on control characters or server divergence).
func (p *Predictor) Reset() {
	p.pending = p.pending[:0]
	p.epoch++
	p.active = false
	p.confirmed = 0
}

// SetCursor updates the predicted cursor position from the server state.
// Call this when processing server output so predictions know where
// the cursor is.
func (p *Predictor) SetCursor(x, y int) {
	if !p.active {
		p.curX = x
		p.curY = y
	}
}

// Active returns true if there are pending predictions.
func (p *Predictor) Active() bool {
	return p.active && len(p.pending) > 0
}

// ExpireStale removes predictions older than the timeout.
func (p *Predictor) ExpireStale(now time.Time) {
	cutoff := now.Add(-predictionTimeout)
	changed := false
	for len(p.pending) > 0 && p.pending[0].at.Before(cutoff) {
		p.pending = p.pending[1:]
		changed = true
	}
	if changed && len(p.pending) == 0 {
		p.active = false
	}
}

// Confirm checks the server framebuffer against pending predictions.
// Confirmed predictions are removed. If the server shows something
// different where we predicted, all predictions are cleared.
func (p *Predictor) Confirm(fb *Framebuffer) {
	if !p.active || len(p.pending) == 0 {
		// No predictions, just track cursor.
		p.curX = fb.CurX
		p.curY = fb.CurY
		return
	}

	// Walk through pending predictions and see if the server matches.
	confirmed := 0
	for confirmed < len(p.pending) {
		pred := &p.pending[confirmed]

		if pred.epoch != p.epoch {
			// Stale prediction from before a reset.
			confirmed++
			continue
		}

		cell := fb.CellAt(pred.x, pred.y)
		if cell == nil {
			// Out of bounds — clear everything.
			p.Reset()
			p.curX = fb.CurX
			p.curY = fb.CurY
			return
		}

		if cell.Rune == pred.r {
			// Server confirmed this prediction.
			confirmed++
		} else if (cell.Rune == ' ' || cell.Rune == 0) && pred.r != ' ' {
			// Server hasn't caught up yet — stop checking.
			// (But if we predicted a space, a space cell is a match, handled above.)
			break
		} else {
			// Server diverged — something unexpected happened
			// (tab completion, shell editing, etc.)
			p.Reset()
			p.curX = fb.CurX
			p.curY = fb.CurY
			return
		}
	}

	if confirmed > 0 {
		p.pending = p.pending[confirmed:]
		p.confirmed += confirmed
	}

	if len(p.pending) == 0 {
		// All predictions confirmed. Sync cursor to server.
		p.active = false
		p.curX = fb.CurX
		p.curY = fb.CurY
	}
}

// Overlay applies pending predictions onto a framebuffer for display.
// The framebuffer is modified in-place — call this on a copy if needed.
// Predicted characters get an underline attribute to indicate they're
// speculative (matching C++ mosh behavior).
func (p *Predictor) Overlay(fb *Framebuffer) {
	if !p.active {
		return
	}

	for i := range p.pending {
		pred := &p.pending[i]
		if pred.epoch != p.epoch {
			continue
		}
		cell := fb.CellAt(pred.x, pred.y)
		if cell == nil {
			continue
		}
		cell.Rune = pred.r
		cell.Width = 1
		cell.Attr.Under = true // underline marks speculative characters
	}

	// Move cursor to predicted position.
	if len(p.pending) > 0 {
		fb.CurX = p.curX
		fb.CurY = p.curY
	}
}
