package tools

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestCappedSmall(t *testing.T) {
	c := NewCapped(OutputCap)
	c.Write([]byte("0123456789"))
	if c.String() != "0123456789" {
		t.Fatalf("got %q", c.String())
	}
}

func TestCappedLarge(t *testing.T) {
	c := NewCapped(OutputCap)
	chunk := []byte(strings.Repeat("y\n", 2048))
	for i := 0; i < 10*1024*1024/len(chunk); i++ {
		c.Write(chunk)
	}
	s := c.String()
	if len(s) >= 30100 || !strings.Contains(s, "bytes omitted") || !strings.HasPrefix(s, "y\n") {
		t.Fatalf("bad output len=%d", len(s))
	}
}

func TestTruncateDeterministic(t *testing.T) {
	in := strings.Repeat("é", 50000)
	a, b := Truncate(in, 1001), Truncate(in, 1001)
	if a != b || !utf8.ValidString(a) || !strings.Contains(a, "bytes omitted") {
		t.Fatalf("bad: %q", a[:50])
	}
	// chunking independence
	c := NewCapped(1001)
	for i := 0; i < len(in); i += 7 {
		c.Write([]byte(in[i:min(i+7, len(in))]))
	}
	if c.String() != a {
		t.Fatal("chunking changed output")
	}
	if Truncate("short", 100) != "short" {
		t.Fatal("short changed")
	}
}

func TestCappedBound(t *testing.T) {
	c := NewCapped(100)
	for i := 0; i < 1000; i++ {
		c.Write([]byte("abcdefg"))
	}
	if len(c.head)+len(c.ring) > 100 {
		t.Fatal("holds more than limit")
	}
}

func TestTruncateRuneBoundaries(t *testing.T) {
	for _, r := range []string{"é", "€", "𝄞"} {
		in := strings.Repeat(r, 5000)
		for limit := 1000; limit < 1010; limit++ {
			out := Truncate(in, limit)
			if !utf8.ValidString(out) || !strings.Contains(out, "bytes omitted") {
				t.Fatalf("rune %q limit %d: invalid or untruncated", r, limit)
			}
		}
	}
}

func TestTruncateBinaryTailKept(t *testing.T) {
	in := strings.Repeat("a", 500) + strings.Repeat("\x80", 100)
	out := Truncate(in, 100)
	if !strings.HasSuffix(out, strings.Repeat("\x80", 47)) {
		t.Fatalf("tail wrongly consumed: %q", out)
	}
}
