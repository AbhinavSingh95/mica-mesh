package terminal

import (
	"io"
	"strings"
	"testing"

	uv "github.com/charmbracelet/ultraviolet"
)

func TestOversizedPasteRejectedBeforeParserAllocation(t *testing.T) {
	for _, size := range []int{promptLimit, promptLimit + 1, 1024 * 1024} {
		rejected := 0
		text := "\x1b[200~" + strings.Repeat("x", size) + "\x1b[201~"
		g := &inputGuard{source: strings.NewReader(text + "z"), rejected: func() { rejected++ }}
		got, err := io.ReadAll(g)
		if err != nil {
			t.Fatal(err)
		}
		want := text + "z"
		count := 0
		if size > promptLimit {
			want = "z"
			count = 1
		}
		if string(got) != want || rejected != count {
			t.Errorf("size=%d output bytes=%d rejections=%d", size, len(got), rejected)
		}
	}
}

type byteReader struct{ io.Reader }

func (r byteReader) Read(p []byte) (int, error) { return r.Reader.Read(p[:1]) }
func TestSplitInputSequences(t *testing.T) {
	for _, input := range []string{"\x1b[200~a界\x1b[20z\x1b[201~", "\x1b[A", "界a", "\x1b"} {
		g := &inputGuard{source: byteReader{strings.NewReader(input)}, rejected: func() { t.Error("valid input rejected") }}
		got, err := io.ReadAll(g)
		if err != nil || string(got) != input {
			t.Errorf("%q => %q %v", input, got, err)
		}
	}
}
func TestEscapeStateBound(t *testing.T) {
	for _, pair := range [][2]string{{"\x1b]", "\a"}, {"\x1bP", "\x1b\\"}, {"\x1b[", "m"}, {"\x1bO", "A"}} {
		rejected := 0
		g := &inputGuard{source: byteReader{strings.NewReader(pair[0] + strings.Repeat("1", 100000) + pair[1] + "ok")}, rejected: func() { rejected++ }}
		got, err := io.ReadAll(g)
		if err != nil || string(got) != "ok" || rejected != 1 {
			t.Errorf("%q length=%d rejected=%d err=%v", pair[0], len(got), rejected, err)
		}
	}
}
func TestSplitOSCSequencesCannotControlTerminal(t *testing.T) {
	for _, sequence := range []string{"\x1b]52;c;secret\a", "\x1b]title\x1b\\", "\u009d52;c;secret\u009c", "\x1bPsecret\x1b\\", "\x1b[31m"} {
		for i := 0; i <= len(sequence); i++ {
			var s sanitizer
			got := s.text("before"+sequence[:i]) + s.text(sequence[i:]+"after\n\t界")
			if got != "beforeafter\n\t界" {
				t.Errorf("split=%d got %q", i, got)
			}
		}
	}
	var s sanitizer
	if got := s.text("\x1b]"+strings.Repeat("x", 100000)) + s.text("hidden\aok"); got != "ok" {
		t.Errorf("overlong control escaped: %d bytes", len(got))
	}
}
func TestStringControlsDrainThroughTheirOwnTerminator(t *testing.T) {
	for _, start := range []string{"\x1bP", "\x1b_", "\x1b^", "\x1bX", "\x90", "\x9f"} {
		input := start + strings.Repeat("1", escapeLimit+1) + "\aHIDDEN\x1b\\ok"
		rejected := 0
		g := &inputGuard{source: byteReader{strings.NewReader(input)}, rejected: func() { rejected++ }}
		got, err := io.ReadAll(g)
		if err != nil || string(got) != "ok" || rejected != 1 {
			t.Errorf("guard %q output=%q err=%v rejected=%d", start, got, err, rejected)
		}
		var s sanitizer
		if got := s.text(start + "\aHIDDEN\x1b\\ok"); got != "ok" {
			t.Errorf("sanitizer %q output=%q", start, got)
		}
	}
	for _, input := range []string{"\x9d52;c;secret\aok", "\x9b31mok"} {
		var s sanitizer
		if got := s.text(input); got != "ok" {
			t.Errorf("raw C1 escaped: %q", got)
		}
	}
}
func TestMalformedUTF8CannotBypassPasteGuard(t *testing.T) {
	for _, prefix := range []string{"\xe2", "\xf0\x80", "\xe2\x9d"} {
		rejected := 0
		g := &inputGuard{source: strings.NewReader(prefix + pasteStart + strings.Repeat("x", promptLimit+1) + pasteEnd + "z"), rejected: func() { rejected++ }}
		got, err := io.ReadAll(g)
		if err != nil || string(got) != "z" || rejected != 1 {
			t.Errorf("prefix=%q bytes=%d rejected=%d err=%v", prefix, len(got), rejected, err)
		}
	}
}

func TestPasteStartEncodingsMatchDecoder(t *testing.T) {
	starts := []string{"200", "0200", "200;1", "200:1", "200;", "200:", "2?00", "2<00", "2=00", "2>00", "2147483848", "4294967496", "18446744073709551816", "2147483647200", "200" + strings.Repeat(";", 32)}
	for _, intro := range []string{"\x1b[", "\x9b"} {
		for _, params := range starts {
			start := intro + params + "~"
			var decoder uv.EventDecoder
			n, ev := decoder.Decode([]byte(start))
			if _, ok := ev.(uv.PasteStartEvent); !ok || n != len(start) {
				t.Fatalf("upstream did not accept %q: %T %d", start, ev, n)
			}
			for _, size := range []int{promptLimit, promptLimit + 1} {
				rejected := 0
				payload := strings.Repeat("q", size)
				g := &inputGuard{source: byteReader{strings.NewReader(start + payload + pasteEnd + "z")}, rejected: func() { rejected++ }}
				got, err := io.ReadAll(g)
				want, count := pasteStart+payload+pasteEnd+"z", 0
				if size > promptLimit {
					want, count = "z", 1
				}
				if err != nil || string(got) != want || rejected != count {
					t.Errorf("start=%q size=%d bytes=%d rejected=%d err=%v", start, size, len(got), rejected, err)
				}
			}
		}
	}
}
func TestOverlongAndNestedPasteStartDrainsWholeBody(t *testing.T) {
	for _, start := range []string{"\x1b[" + strings.Repeat("0", 100000) + "200~", "\x1b[12\x1b[200~", "\x1b[12\x9b200~", "\x1bO12\x1b[200~", "\x1b\x1b[200~"} {
		rejected := 0
		g := &inputGuard{source: byteReader{strings.NewReader(start + strings.Repeat("q", promptLimit+1) + pasteEnd + "z")}, rejected: func() { rejected++ }}
		got, err := io.ReadAll(g)
		if err != nil || string(got) != "z" || rejected < 1 {
			t.Errorf("start bytes=%d output bytes=%d rejected=%d err=%v", len(start), len(got), rejected, err)
		}
	}
}

func TestIncompleteRejectedPasteRemainsDraining(t *testing.T) {
	rejected := 0
	g := &inputGuard{source: strings.NewReader("\x9b0200;1~" + strings.Repeat("q", 1024*1024)), rejected: func() { rejected++ }}
	got, err := io.ReadAll(g)
	if err != nil || len(got) != 0 || rejected != 1 {
		t.Fatalf("incomplete paste bytes=%d rejected=%d err=%v", len(got), rejected, err)
	}
	g.source = strings.NewReader("hidden" + pasteEnd + pasteStart + "valid" + pasteEnd + "z")
	got, err = io.ReadAll(g)
	if err != nil || string(got) != pasteStart+"valid"+pasteEnd+"z" || rejected != 1 {
		t.Fatalf("drain output=%q rejected=%d err=%v", got, rejected, err)
	}
}
