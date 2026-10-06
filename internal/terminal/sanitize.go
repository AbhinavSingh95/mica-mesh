package terminal

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// sanitizer is one text stream's constant-size control-sequence state. It never
// retains control payloads, including after an unterminated oversized sequence.
type sanitizer struct {
	state          byte
	bellTerminator bool
	pending        [utf8.UTFMax]byte
	n              int
}

func (s *sanitizer) text(value string) string {
	var out strings.Builder
	for i := 0; i < len(value); i++ {
		s.pending[s.n] = value[i]
		s.n++
		for s.n > 0 {
			var r rune
			var w int
			if s.pending[0] >= 0x80 && s.pending[0] <= 0x9f {
				r = rune(s.pending[0])
				w = 1
			} else {
				if !utf8.FullRune(s.pending[:s.n]) {
					break
				}
				r, w = utf8.DecodeRune(s.pending[:s.n])
			}
			s.n = copy(s.pending[:], s.pending[w:s.n])
			s.accept(r, &out)
		}
	}
	return out.String()
}
func (s *sanitizer) accept(r rune, out *strings.Builder) {
	switch s.state {
	case 'e':
		switch r {
		case '[':
			s.state = 'c'
		case ']', 'P', 'X', '^', '_':
			s.state = 's'
			s.bellTerminator = r == ']'
		default:
			s.state = 0
		}
		return
	case 'c':
		if r >= 0x40 && r <= 0x7e {
			s.state = 0
		}
		return
	case 's', 't':
		if r == 7 && s.bellTerminator || r == 0x9c || s.state == 't' && r == '\\' {
			s.state = 0
		} else if r == 0x1b {
			s.state = 't'
		} else {
			s.state = 's'
		}
		return
	}
	switch r {
	case 0x1b:
		s.state = 'e'
	case 0x9b:
		s.state = 'c'
	case 0x90, 0x9d, 0x98, 0x9e, 0x9f:
		s.state = 's'
		s.bellTerminator = r == 0x9d
	case '\n', '\t':
		out.WriteRune(r)
	default:
		if !unicode.IsControl(r) && r != utf8.RuneError {
			out.WriteRune(r)
		}
	}
}
