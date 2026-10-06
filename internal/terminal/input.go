package terminal

import (
	"errors"
	"io"
	"os"
	"unicode/utf8"
)

const promptLimit = 16 * 1024
const escapeLimit = 4 * 1024
const pasteStart = "\x1b[200~"
const pasteEnd = "\x1b[201~"

// inputGuard holds a complete paste or escape before exposing it to the library.
// Storage never grows with input. Rejected sequences drain through their terminator.
// The reader has one owner. Its source can set a short deadline for a lone Escape.
type inputGuard struct {
	source         io.Reader
	rejected       func()
	ambiguity      func(bool) error
	state          byte // e: Escape, c: CSI, o: SS3, s: string control, p: paste
	held           [promptLimit + 12]byte
	n              int
	output         [promptLimit + 12]byte
	start, end     int
	prefix         int
	csi            csiPaste
	stringEscape   bool
	bellTerminator bool
	discard        bool
	raw            [4096]byte
	pos, count     int
	sourceErr      error
	fatalErr       error
	utf8Left       int
	utf8Bytes      [utf8.UTFMax]byte
	utf8N          int
}

func (g *inputGuard) Read(p []byte) (int, error) {
	if g.fatalErr != nil {
		return 0, g.fatalErr
	}
	if len(p) == 0 {
		return 0, nil
	}
	for g.start == g.end {
		if g.pos == g.count {
			if g.sourceErr != nil {
				err := g.sourceErr
				g.sourceErr = nil
				if g.state == 'e' {
					g.release()
					if g.end > 0 {
						break
					}
				}
				return 0, err
			}
			if g.ambiguity != nil {
				if err := g.ambiguity(g.state == 'e'); err != nil {
					return 0, err
				}
			}
			g.count, g.sourceErr = g.source.Read(g.raw[:])
			g.pos = 0
			if g.count == 0 {
				if errors.Is(g.sourceErr, os.ErrDeadlineExceeded) && g.state == 'e' {
					g.sourceErr = nil
					g.release()
					break
				}
				if g.sourceErr == nil {
					continue
				}
				continue
			}
		}
		b := g.raw[g.pos]
		g.pos++
		g.consume(b)
		if g.fatalErr != nil {
			return 0, g.fatalErr
		}
	}
	n := copy(p, g.output[g.start:g.end])
	g.start += n
	if g.start == g.end {
		g.start = 0
		g.end = 0
	}
	return n, nil
}
func (g *inputGuard) reject() {
	if !g.discard {
		g.discard = true
		g.n = 0
		g.rejected()
	}
}
func (g *inputGuard) hold(b byte, limit int) {
	if g.discard {
		return
	}
	if g.n == limit {
		g.reject()
		return
	}
	g.held[g.n] = b
	g.n++
}
func (g *inputGuard) release() {
	if !g.discard {
		g.end = copy(g.output[:], g.held[:g.n])
	}
	g.n = 0
	g.state = 0
	g.discard = false
	g.stringEscape = false
}
func (g *inputGuard) consume(b byte) {
	if g.state == 'p' {
		if b == pasteEnd[g.prefix] {
			g.prefix++
			if g.prefix == len(pasteEnd) {
				if !g.discard {
					copy(g.output[:], pasteStart)
					copy(g.output[len(pasteStart):], g.held[:g.n])
					copy(g.output[len(pasteStart)+g.n:], pasteEnd)
					g.end = len(pasteStart) + g.n + len(pasteEnd)
				}
				g.state = 0
				g.n = 0
				g.prefix = 0
				g.discard = false
			}
			return
		}
		for i := 0; i < g.prefix; i++ {
			g.hold(pasteEnd[i], promptLimit)
		}
		g.prefix = 0
		if b == pasteEnd[0] {
			g.prefix = 1
		} else {
			g.hold(b, promptLimit)
		}
		return
	}
	if g.state == 0 {
		if g.utf8Left > 0 {
			if b >= 0x80 && b <= 0xbf {
				g.utf8Bytes[g.utf8N] = b
				g.utf8N++
				g.utf8Left--
				if g.utf8Left == 0 && utf8.Valid(g.utf8Bytes[:g.utf8N]) {
					g.end = copy(g.output[:], g.utf8Bytes[:g.utf8N])
				}
				return
			}
			// An invalid UTF-8 prefix cannot hide a following control marker.
			g.utf8Left = 0
		}
		switch {
		case b >= 0xc2 && b <= 0xdf:
			g.utf8Left = 1
		case b >= 0xe0 && b <= 0xef:
			g.utf8Left = 2
		case b >= 0xf0 && b <= 0xf4:
			g.utf8Left = 3
		}
		if g.utf8Left > 0 {
			g.utf8Bytes[0] = b
			g.utf8N = 1
			return
		}

		switch b {
		case 0x1b:
			g.state = 'e'
		case 0x9b:
			g.state = 'c'
			g.csi = csiPaste{}
		case 0x8f:
			g.state = 'o'
		case 0x90, 0x9d, 0x98, 0x9e, 0x9f:
			g.state = 's'
			g.bellTerminator = b == 0x9d
		default:
			g.output[0] = b
			g.end = 1
			return
		}
		g.hold(b, escapeLimit)
		return
	}
	// A malformed escape must not carry an embedded paste start past this guard.
	if (g.state == 'c' || g.state == 'o') && (b < 0x20 || b > 0x7e) || g.state == 'e' && (b == 0x1b || b == 0x9b) {
		g.reject()
		g.release()
		g.consume(b)
		return
	}
	g.hold(b, escapeLimit)
	switch g.state {
	case 'e':
		switch b {
		case '[':
			g.state = 'c'
			g.csi = csiPaste{}
		case 'O':
			g.state = 'o'
		case ']', 'P', 'X', '^', '_':
			g.state = 's'
			g.bellTerminator = b == ']'
		default:
			g.release()
		}
	case 'c':
		g.csi.consume(b)
		if b >= 0x40 && b <= 0x7e {
			// Win32 serialized input is unsupported on macOS. Stop before later bytes
			// can become commands; interpreting reconstructed paste would emulate Win32.
			// Paste payloads bypass this branch and remain literal.
			if b == '_' {
				g.fatalErr = errors.New("unsupported terminal input encoding; use another macOS terminal or rerun with --plain")
				return
			}
			if b == '~' && g.csi.pasteStart() {
				g.state = 'p'
				g.n = 0
			} else {
				g.release()
			}
		}
	case 'o':
		if b < '0' || b > '9' {
			g.release()
		}
	case 's':
		if b == 7 && g.bellTerminator || b == 0x9c || (g.stringEscape && b == '\\') {
			g.release()
		} else {
			g.stringEscape = b == 0x1b
		}
	}
}

// csiPaste mirrors the pinned Ultraviolet decoder's first-parameter semantics.
// Its Param mask/sentinel is 31 bits, its arithmetic uses native int overflow,
// and it scans at most 32 parameters. Keep classifying after the escape storage
// limit: an overlong paste marker still requires draining the whole paste body.
// Both supported Mac architectures use 64-bit int. Tests compare with the decoder.
type csiPaste struct {
	value   int
	params  int
	started bool
	invalid bool
}

func (c *csiPaste) consume(b byte) {
	if b >= 0x40 && b <= 0x7e {
		return
	}
	if !c.started && b >= '<' && b <= '?' {
		c.invalid = true
	}
	c.started = true
	if b < 0x30 || b > 0x3f || c.params >= 32 {
		c.invalid = true
		return
	}
	if c.params == 0 && b >= '0' && b <= '9' {
		if c.value == 0x7fffffff {
			c.value = 0
		}
		c.value = c.value*10 + int(b-'0')
	}
	if b == ';' || b == ':' {
		c.params++
	}
}
func (c *csiPaste) pasteStart() bool { return !c.invalid && c.value&0x7fffffff == 200 }
