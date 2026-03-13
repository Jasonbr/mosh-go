package mosh

import (
	"strings"
	"testing"
)

func TestFramebufferDiffIdentical(t *testing.T) {
	fb := NewFramebuffer(80, 24)
	old := NewFramebuffer(80, 24)
	diff := fb.Diff(old)
	// Should just be cursor positioning + show cursor, no cell updates.
	if strings.Contains(string(diff), "\033[2J") {
		t.Fatal("identical framebuffers should not trigger full clear")
	}
}

func TestFramebufferDiffSingleCell(t *testing.T) {
	old := NewFramebuffer(80, 24)
	fb := NewFramebuffer(80, 24)
	fb.CellAt(5, 3).Rune = 'A'
	fb.CellAt(5, 3).Width = 1

	diff := fb.Diff(old)
	s := string(diff)
	// Should contain CUP to row 4, col 6 (1-indexed).
	if !strings.Contains(s, "\033[4;6H") {
		t.Fatalf("expected CUP \\033[4;6H, got %q", s)
	}
	if !strings.Contains(s, "A") {
		t.Fatal("diff should contain 'A'")
	}
}

func TestFramebufferFullRedraw(t *testing.T) {
	fb := NewFramebuffer(80, 24)
	fb.CellAt(0, 0).Rune = 'X'
	fb.CellAt(0, 0).Width = 1

	diff := fb.Diff(nil)
	s := string(diff)
	if !strings.Contains(s, "\033[2J") {
		t.Fatal("nil old should trigger full redraw with clear")
	}
	if !strings.Contains(s, "X") {
		t.Fatal("full redraw should contain 'X'")
	}
}

func TestFramebufferDiffSizeChange(t *testing.T) {
	old := NewFramebuffer(80, 24)
	fb := NewFramebuffer(132, 43)
	diff := fb.Diff(old)
	s := string(diff)
	if !strings.Contains(s, "\033[2J") {
		t.Fatal("size change should trigger full redraw")
	}
}

func TestFramebufferDiffWithAttrs(t *testing.T) {
	old := NewFramebuffer(80, 24)
	fb := NewFramebuffer(80, 24)

	c := fb.CellAt(0, 0)
	c.Rune = 'B'
	c.Width = 1
	c.Attr = Attr{Bold: true, FG: Color{Type: ColorIndex, Value: 1}}

	diff := fb.Diff(old)
	s := string(diff)
	// Should contain bold (1) and red fg (31).
	if !strings.Contains(s, "1") {
		t.Fatal("diff should contain bold SGR")
	}
	if !strings.Contains(s, "31") {
		t.Fatal("diff should contain red FG SGR")
	}
	if !strings.Contains(s, "B") {
		t.Fatal("diff should contain 'B'")
	}
	// Should reset at end.
	if !strings.Contains(s, "\033[m") {
		t.Fatal("diff should reset attributes at end")
	}
}

func TestFramebufferDiffRGBColor(t *testing.T) {
	old := NewFramebuffer(80, 24)
	fb := NewFramebuffer(80, 24)

	c := fb.CellAt(0, 0)
	c.Rune = 'R'
	c.Width = 1
	c.Attr.FG = Color{Type: ColorRGB, Value: 0xFF8000} // orange

	diff := fb.Diff(old)
	s := string(diff)
	if !strings.Contains(s, "38;2;255;128;0") {
		t.Fatalf("expected RGB SGR, got %q", s)
	}
}

func TestFramebufferDiffWideChar(t *testing.T) {
	old := NewFramebuffer(80, 24)
	fb := NewFramebuffer(80, 24)

	// Wide character at (0,0) with continuation at (1,0).
	fb.CellAt(0, 0).Rune = '中'
	fb.CellAt(0, 0).Width = 2
	fb.CellAt(1, 0).Rune = 0
	fb.CellAt(1, 0).Width = 0

	diff := fb.Diff(old)
	s := string(diff)
	if !strings.Contains(s, "中") {
		t.Fatal("diff should contain wide character")
	}
}

func TestFramebufferDiffCursorPosition(t *testing.T) {
	old := NewFramebuffer(80, 24)
	fb := NewFramebuffer(80, 24)
	fb.CurX = 10
	fb.CurY = 5

	diff := fb.Diff(old)
	s := string(diff)
	// Final cursor position should be at row 6, col 11 (1-indexed).
	if !strings.Contains(s, "\033[6;11H") {
		t.Fatalf("expected cursor at [6;11H, got %q", s)
	}
}

func TestFramebufferDiffCursorHidden(t *testing.T) {
	old := NewFramebuffer(80, 24)
	fb := NewFramebuffer(80, 24)
	fb.CurVis = false

	diff := fb.Diff(old)
	s := string(diff)
	if strings.Contains(s, "\033[?25h") {
		t.Fatal("cursor should not be shown when CurVis=false")
	}
}

func TestAppendCUP(t *testing.T) {
	buf := appendCUP(nil, 0, 0)
	if string(buf) != "\033[1;1H" {
		t.Fatalf("got %q", string(buf))
	}
	buf = appendCUP(nil, 23, 79)
	if string(buf) != "\033[24;80H" {
		t.Fatalf("got %q", string(buf))
	}
}
