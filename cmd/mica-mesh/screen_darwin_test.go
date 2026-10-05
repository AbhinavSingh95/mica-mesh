package main

import (
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	"charm.land/lipgloss/v2"
)

// testScreen reconstructs the renderer's cursor edits for state assertions.
// It is bounded to the fixture's largest terminal. Raw bytes are also retained
// separately so escape-injection assertions do not depend on this decoder.
type testScreen struct {
	cells       [40][120]string
	x, y        int
	top, bottom int // Scroll region; a zero bottom means the full fixture height.
	last        string
	pending     string
}

func TestScreenReplaysRendererScrollRegion(t *testing.T) {
	var s testScreen
	s.write("\x1b[1;1HHeader\x1b[5;1HAgents  0 ready\x1b[6;1HFirst\x1b[7;1HSecond\x1b[12;1HFooter")
	// The renderer inserts an Agent panel by scrolling only rows 6 through 11.
	s.write("\x1b[6;11r\x1b[6;1H\x1b[3T\x1b[1;40r\x1b[5;9H1")
	for row, want := range map[int]string{0: "Header", 4: "Agents  1 ready", 8: "First", 9: "Second", 11: "Footer"} {
		if got := strings.TrimSpace(strings.Split(s.text(), "\n")[row]); got != want {
			t.Errorf("row %d: got %q, want %q", row, got, want)
		}
	}
}

func (s *testScreen) write(data string) {
	s.pending += data
	for len(s.pending) > 0 {
		if s.pending[0] == '\x1b' {
			if len(s.pending) < 2 {
				return
			}
			if s.pending[1] == ']' {
				end := 2
				for end < len(s.pending) && s.pending[end] != '\a' && s.pending[end] != '\x1b' {
					end++
				}
				if end == len(s.pending) || s.pending[end] == '\x1b' && end+1 == len(s.pending) {
					return
				}
				if s.pending[end] == '\x1b' {
					end++
				}
				s.pending = s.pending[end+1:]
				continue
			}
			if s.pending[1] == '[' {
				end := 2
				for end < len(s.pending) && (s.pending[end] < 0x40 || s.pending[end] > 0x7e) {
					end++
				}
				if end == len(s.pending) {
					return
				}
				s.csi(s.pending[2:end], s.pending[end])
				s.pending = s.pending[end+1:]
				continue
			}
			if s.pending[1] == 'M' {
				s.y = max(0, s.y-1)
			}
			s.pending = s.pending[2:]
			continue
		}
		if !utf8.FullRuneInString(s.pending) {
			return
		}
		r, n := utf8.DecodeRuneInString(s.pending)
		s.pending = s.pending[n:]
		switch r {
		case '\r':
			s.x = 0
		case '\n':
			s.y = min(s.y+1, 39)
		case '\b':
			s.x = max(0, s.x-1)
		case '\t':
			s.x = min(119, (s.x/8+1)*8)
		default:
			if r < 32 {
				continue
			}
			width := lipgloss.Width(string(r))
			s.last = string(r)
			if s.x < 120 {
				s.cells[s.y][s.x] = string(r)
				for i := 1; i < width && s.x+i < 120; i++ {
					s.cells[s.y][s.x+i] = "\x00"
				}
			}
			s.x = min(s.x+width, 120)
		}
	}
}

func (s *testScreen) csi(params string, command byte) {
	bottom := s.bottom
	if bottom == 0 {
		bottom = len(s.cells)
	}
	parts := strings.Split(params, ";")
	value := func(i, fallback int) int {
		if i >= len(parts) {
			return fallback
		}
		n, err := strconv.Atoi(parts[i])
		if err != nil || n == 0 {
			return fallback
		}
		return n
	}
	switch command {
	case 'r':
		top, end := value(0, 1)-1, value(1, len(s.cells))
		if top >= 0 && top < end-1 && end <= len(s.cells) {
			s.top, s.bottom = top, end
			s.x, s.y = 0, 0
		}
	case 'H', 'f':
		s.y, s.x = min(max(value(0, 1)-1, 0), 39), min(max(value(1, 1)-1, 0), 119)
	case 'A':
		s.y = max(0, s.y-value(0, 1))
	case 'B':
		s.y = min(39, s.y+value(0, 1))
	case 'E':
		s.y, s.x = min(39, s.y+value(0, 1)), 0
	case 'F':
		s.y, s.x = max(0, s.y-value(0, 1)), 0
	case 'C':
		s.x = min(119, s.x+value(0, 1))
	case 'D':
		s.x = max(0, s.x-value(0, 1))
	case 'G', '`':
		s.x = min(max(value(0, 1)-1, 0), 119)
	case 'd':
		s.y = min(max(value(0, 1)-1, 0), 39)
	case 'J':
		mode := value(0, 0)
		for y := 0; y < 40; y++ {
			for x := 0; x < 120; x++ {
				if mode == 2 || mode == 3 || mode == 0 && (y > s.y || y == s.y && x >= s.x) || mode == 1 && (y < s.y || y == s.y && x <= s.x) {
					s.cells[y][x] = ""
				}
			}
		}
	case 'K':
		mode := value(0, 0)
		for x := 0; x < 120; x++ {
			if mode == 2 || mode == 0 && x >= s.x || mode == 1 && x <= s.x {
				s.cells[s.y][x] = ""
			}
		}
	case 'P':
		n := min(value(0, 1), 120-s.x)
		copy(s.cells[s.y][s.x:], s.cells[s.y][s.x+n:])
		clear(s.cells[s.y][120-n:])
	case '@':
		n := min(value(0, 1), 120-s.x)
		copy(s.cells[s.y][s.x+n:], s.cells[s.y][s.x:120-n])
		clear(s.cells[s.y][s.x : s.x+n])
	case 'b':
		for range min(value(0, 1), 120-s.x) {
			s.cells[s.y][s.x] = s.last
			s.x++
		}
	case 'I':
		s.x = min(119, (s.x/8+value(0, 1))*8)
	case 'Z':
		s.x = max(0, ((s.x-1)/8-value(0, 1)+1)*8)
	case 'X':
		for x := s.x; x < min(120, s.x+value(0, 1)); x++ {
			s.cells[s.y][x] = ""
		}
	case 'L':
		if s.y < s.top || s.y >= bottom {
			return
		}
		n := min(value(0, 1), bottom-s.y)
		copy(s.cells[s.y+n:bottom], s.cells[s.y:bottom-n])
		clear(s.cells[s.y : s.y+n])
	case 'M':
		if s.y < s.top || s.y >= bottom {
			return
		}
		n := min(value(0, 1), bottom-s.y)
		copy(s.cells[s.y:bottom], s.cells[s.y+n:bottom])
		clear(s.cells[bottom-n : bottom])
	case 'S':
		n := min(value(0, 1), bottom-s.top)
		copy(s.cells[s.top:bottom], s.cells[s.top+n:bottom])
		clear(s.cells[bottom-n : bottom])
	case 'T':
		n := min(value(0, 1), bottom-s.top)
		copy(s.cells[s.top+n:bottom], s.cells[s.top:bottom-n])
		clear(s.cells[s.top : s.top+n])
	}
}

func (s *testScreen) text() string {
	var b strings.Builder
	for _, row := range s.cells {
		for _, cell := range row {
			if cell == "" {
				b.WriteByte(' ')
			} else if cell != "\x00" {
				b.WriteString(cell)
			}
		}
		b.WriteByte('\n')
	}
	return b.String()
}
