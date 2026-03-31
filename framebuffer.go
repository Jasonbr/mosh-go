package mosh

import (
	"image/color"
	"strconv"
	"unicode/utf8"

	"github.com/charmbracelet/x/ansi"
	"github.com/unixshells/vt-go"
)

// Cell attribute bitmasks (matching charmbracelet/ultraviolet values).
const (
	attrBold          = 1 << 0
	attrFaint         = 1 << 1
	attrItalic        = 1 << 2
	attrBlink         = 1 << 3
	attrReverse       = 1 << 4
	attrStrikethrough = 1 << 5
)

// Framebuffer represents a terminal screen state for mosh SSP.
// The server captures this from the VT emulator; the diff between
// two Framebuffers produces the ANSI escape sequences sent as HostBytes.
type Framebuffer struct {
	W, H   int
	Cells  []Cell
	CurX   int
	CurY   int
	CurVis bool
}

// Cell is a single character cell with SGR attributes.
type Cell struct {
	Rune  rune
	Width int // display width (0 for continuation cells)
	Attr  Attr
}

// Attr holds SGR attributes for a cell.
type Attr struct {
	FG      Color
	BG      Color
	Bold    bool
	Dim     bool
	Italic  bool
	Under   bool
	Blink   bool
	Reverse bool
	Strike  bool
}

// Color represents a terminal color.
type Color struct {
	Type  ColorType
	Value uint32 // index (0-255) or RGB packed as 0x00RRGGBB
}

// ColorType identifies the color encoding.
type ColorType byte

const (
	ColorDefault ColorType = iota
	ColorIndex            // 0-7 normal, 8-15 bright, 16-255 extended
	ColorRGB
)

// NewFramebuffer allocates a blank framebuffer.
func NewFramebuffer(w, h int) *Framebuffer {
	fb := &Framebuffer{
		W:      w,
		H:      h,
		Cells:  make([]Cell, w*h),
		CurVis: true,
	}
	for i := range fb.Cells {
		fb.Cells[i].Rune = ' '
		fb.Cells[i].Width = 1
	}
	return fb
}

// CellAt returns a pointer to the cell at (x, y).
func (fb *Framebuffer) CellAt(x, y int) *Cell {
	if x < 0 || x >= fb.W || y < 0 || y >= fb.H {
		return nil
	}
	return &fb.Cells[y*fb.W+x]
}

// Diff produces the minimal ANSI escape sequence to transform old into fb.
// This is what goes into HostBytes.hoststring in the mosh protocol.
func (fb *Framebuffer) Diff(old *Framebuffer) []byte {
	var buf []byte

	// If dimensions changed, send a full redraw.
	if old == nil || old.W != fb.W || old.H != fb.H {
		return fb.fullRedraw()
	}

	// Hide cursor during update.
	buf = append(buf, "\033[?25l"...)

	var curAttr Attr
	curX, curY := -1, -1

	for y := 0; y < fb.H; y++ {
		rowOff := y * fb.W
		oldRowOff := y * old.W

		// Find first and last changed column in this row.
		first, last := -1, -1
		for x := 0; x < fb.W; x++ {
			if fb.Cells[rowOff+x] != old.Cells[oldRowOff+x] {
				if first < 0 {
					first = x
				}
				last = x
			}
		}
		if first < 0 {
			continue // row unchanged
		}

		// Move cursor if needed.
		if curX != first || curY != y {
			buf = appendCUP(buf, y, first)
			curX, curY = first, y
		}

		// Write changed cells.
		for x := first; x <= last; x++ {
			c := &fb.Cells[rowOff+x]
			if c.Width == 0 {
				continue // continuation cell of a wide char
			}
			buf = appendAttrDiff(buf, &curAttr, &c.Attr)
			if c.Rune == 0 || c.Rune == ' ' {
				buf = append(buf, ' ')
			} else {
				buf = appendRune(buf, c.Rune)
			}
			curX += c.Width
		}
	}

	// Reset attributes.
	if curAttr != (Attr{}) {
		buf = append(buf, "\033[m"...)
	}

	// Reposition cursor and show it.
	buf = appendCUP(buf, fb.CurY, fb.CurX)
	if fb.CurVis {
		buf = append(buf, "\033[?25h"...)
	}

	return buf
}

// fullRedraw produces ANSI to draw the entire screen from scratch.
func (fb *Framebuffer) fullRedraw() []byte {
	var buf []byte
	buf = append(buf, "\033[?25l"...)  // hide cursor
	buf = append(buf, "\033[H"...)     // home
	buf = append(buf, "\033[2J"...)    // clear screen
	buf = append(buf, "\033[m"...)     // reset attrs

	var curAttr Attr
	for y := 0; y < fb.H; y++ {
		if y > 0 {
			buf = append(buf, "\r\n"...)
		}
		rowOff := y * fb.W
		// Track trailing spaces to avoid sending them.
		lastNonSpace := -1
		for x := fb.W - 1; x >= 0; x-- {
			c := &fb.Cells[rowOff+x]
			if c.Rune != ' ' || c.Rune == 0 || c.Attr != (Attr{}) {
				lastNonSpace = x
				break
			}
		}

		for x := 0; x <= lastNonSpace; x++ {
			c := &fb.Cells[rowOff+x]
			if c.Width == 0 {
				continue
			}
			buf = appendAttrDiff(buf, &curAttr, &c.Attr)
			if c.Rune == 0 || c.Rune == ' ' {
				buf = append(buf, ' ')
			} else {
				buf = appendRune(buf, c.Rune)
			}
		}
	}

	if curAttr != (Attr{}) {
		buf = append(buf, "\033[m"...)
	}
	buf = appendCUP(buf, fb.CurY, fb.CurX)
	if fb.CurVis {
		buf = append(buf, "\033[?25h"...)
	}
	return buf
}

// appendCUP appends a cursor position escape (1-indexed).
func appendCUP(buf []byte, row, col int) []byte {
	buf = append(buf, "\033["...)
	buf = strconv.AppendInt(buf, int64(row+1), 10)
	buf = append(buf, ';')
	buf = strconv.AppendInt(buf, int64(col+1), 10)
	buf = append(buf, 'H')
	return buf
}

// appendAttrDiff appends SGR sequences to transition from cur to next.
func appendAttrDiff(buf []byte, cur, next *Attr) []byte {
	if *cur == *next {
		return buf
	}

	// If attributes are being removed, reset first.
	needsReset := (cur.Bold && !next.Bold) ||
		(cur.Dim && !next.Dim) ||
		(cur.Italic && !next.Italic) ||
		(cur.Under && !next.Under) ||
		(cur.Blink && !next.Blink) ||
		(cur.Reverse && !next.Reverse) ||
		(cur.Strike && !next.Strike) ||
		(cur.FG.Type != ColorDefault && next.FG.Type == ColorDefault) ||
		(cur.BG.Type != ColorDefault && next.BG.Type == ColorDefault)

	if needsReset {
		buf = append(buf, "\033[0"...)
		*cur = Attr{}
	} else {
		buf = append(buf, "\033["...)
	}

	sep := byte(';')
	first := !needsReset

	addParam := func(code string) {
		if !first {
			buf = append(buf, sep)
		}
		first = false
		buf = append(buf, code...)
	}

	if next.Bold && !cur.Bold {
		addParam("1")
	}
	if next.Dim && !cur.Dim {
		addParam("2")
	}
	if next.Italic && !cur.Italic {
		addParam("3")
	}
	if next.Under && !cur.Under {
		addParam("4")
	}
	if next.Blink && !cur.Blink {
		addParam("5")
	}
	if next.Reverse && !cur.Reverse {
		addParam("7")
	}
	if next.Strike && !cur.Strike {
		addParam("9")
	}

	if next.FG != cur.FG {
		buf = appendColor(buf, next.FG, true, &first, sep)
	}
	if next.BG != cur.BG {
		buf = appendColor(buf, next.BG, false, &first, sep)
	}

	buf = append(buf, 'm')
	*cur = *next
	return buf
}

// appendColor appends SGR parameters for a color.
func appendColor(buf []byte, c Color, fg bool, first *bool, sep byte) []byte {
	addSep := func() {
		if !*first {
			buf = append(buf, sep)
		}
		*first = false
	}

	switch c.Type {
	case ColorDefault:
		addSep()
		if fg {
			buf = append(buf, "39"...)
		} else {
			buf = append(buf, "49"...)
		}
	case ColorIndex:
		idx := c.Value
		if idx < 8 {
			addSep()
			if fg {
				buf = strconv.AppendInt(buf, int64(30+idx), 10)
			} else {
				buf = strconv.AppendInt(buf, int64(40+idx), 10)
			}
		} else if idx < 16 {
			addSep()
			if fg {
				buf = strconv.AppendInt(buf, int64(90+idx-8), 10)
			} else {
				buf = strconv.AppendInt(buf, int64(100+idx-8), 10)
			}
		} else {
			addSep()
			if fg {
				buf = append(buf, "38;5;"...)
			} else {
				buf = append(buf, "48;5;"...)
			}
			buf = strconv.AppendInt(buf, int64(idx), 10)
		}
	case ColorRGB:
		addSep()
		r := (c.Value >> 16) & 0xff
		g := (c.Value >> 8) & 0xff
		b := c.Value & 0xff
		if fg {
			buf = append(buf, "38;2;"...)
		} else {
			buf = append(buf, "48;2;"...)
		}
		buf = strconv.AppendInt(buf, int64(r), 10)
		buf = append(buf, ';')
		buf = strconv.AppendInt(buf, int64(g), 10)
		buf = append(buf, ';')
		buf = strconv.AppendInt(buf, int64(b), 10)
	}
	return buf
}

// appendRune appends a rune as UTF-8.
func appendRune(buf []byte, r rune) []byte {
	var tmp [4]byte
	n := encodeRune(tmp[:], r)
	return append(buf, tmp[:n]...)
}

// encodeRune is a minimal UTF-8 encoder avoiding the unicode/utf8 import.
func encodeRune(buf []byte, r rune) int {
	switch {
	case r < 0x80:
		buf[0] = byte(r)
		return 1
	case r < 0x800:
		buf[0] = byte(0xc0 | (r >> 6))
		buf[1] = byte(0x80 | (r & 0x3f))
		return 2
	case r < 0x10000:
		buf[0] = byte(0xe0 | (r >> 12))
		buf[1] = byte(0x80 | ((r >> 6) & 0x3f))
		buf[2] = byte(0x80 | (r & 0x3f))
		return 3
	default:
		buf[0] = byte(0xf0 | (r >> 18))
		buf[1] = byte(0x80 | ((r >> 12) & 0x3f))
		buf[2] = byte(0x80 | ((r >> 6) & 0x3f))
		buf[3] = byte(0x80 | (r & 0x3f))
		return 4
	}
}

// SnapshotEmulator captures the VT emulator state into a Framebuffer.
func SnapshotEmulator(emu *vt.Emulator, cursorVisible bool) *Framebuffer {
	w := emu.Width()
	h := emu.Height()
	fb := NewFramebuffer(w, h)

	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			c := emu.CellAt(x, y)
			if c == nil {
				continue
			}
			cell := &fb.Cells[y*w+x]

			// Content → first rune.
			if c.Content == "" || c.Content == " " {
				cell.Rune = ' '
			} else {
				r, _ := utf8.DecodeRuneInString(c.Content)
				if r == utf8.RuneError {
					cell.Rune = ' '
				} else {
					cell.Rune = r
				}
			}

			cell.Width = c.Width
			if cell.Width == 0 && cell.Rune == ' ' {
				// Continuation cell.
				cell.Width = 0
			}

			// Attributes.
			cell.Attr.Bold = c.Style.Attrs&attrBold != 0
			cell.Attr.Dim = c.Style.Attrs&attrFaint != 0
			cell.Attr.Italic = c.Style.Attrs&attrItalic != 0
			cell.Attr.Blink = c.Style.Attrs&attrBlink != 0
			cell.Attr.Reverse = c.Style.Attrs&attrReverse != 0
			cell.Attr.Strike = c.Style.Attrs&attrStrikethrough != 0
			cell.Attr.Under = c.Style.Underline != ansi.UnderlineNone

			cell.Attr.FG = convertColor(c.Style.Fg)
			cell.Attr.BG = convertColor(c.Style.Bg)
		}
	}

	pos := emu.CursorPosition()
	fb.CurX = pos.X
	fb.CurY = pos.Y
	fb.CurVis = cursorVisible

	return fb
}

// convertColor converts a color.Color from the VT emulator to our Color type.
func convertColor(c color.Color) Color {
	if c == nil {
		return Color{Type: ColorDefault}
	}
	switch v := c.(type) {
	case ansi.BasicColor:
		return Color{Type: ColorIndex, Value: uint32(v)}
	case ansi.IndexedColor:
		return Color{Type: ColorIndex, Value: uint32(v)}
	case ansi.TrueColor:
		return Color{Type: ColorRGB, Value: uint32(v)}
	case color.RGBA:
		return Color{Type: ColorRGB, Value: uint32(v.R)<<16 | uint32(v.G)<<8 | uint32(v.B)}
	default:
		r, g, b, _ := c.RGBA()
		return Color{Type: ColorRGB, Value: uint32(r>>8)<<16 | uint32(g>>8)<<8 | uint32(b>>8)}
	}
}
