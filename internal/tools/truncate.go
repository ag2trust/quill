package tools

import (
	"fmt"
	"sync"
	"unicode/utf8"
)

// OutputCap is the maximum size of tool output returned to the model.
const OutputCap = 30000

// Capped is a bounded io.Writer keeping the first limit/2 bytes and the last
// limit/2 bytes written. Safe for concurrent use.
type Capped struct {
	mu    sync.Mutex
	half  int
	head  []byte
	ring  []byte // grows to half, then wraps
	pos   int    // next write index once ring is full
	total int64
}

// NewCapped returns a Capped holding at most limit bytes.
func NewCapped(limit int) *Capped {
	return &Capped{half: limit / 2}
}

func (c *Capped) Write(b []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := len(b)
	c.total += int64(n)
	if room := c.half - len(c.head); room > 0 {
		k := min(room, len(b))
		c.head = append(c.head, b[:k]...)
		b = b[k:]
	}
	if c.half == 0 {
		return n, nil
	}
	if len(b) > c.half {
		b = b[len(b)-c.half:]
		c.ring, c.pos = append(c.ring[:0], b...), 0
		return n, nil
	}
	for len(b) > 0 {
		if len(c.ring) < c.half {
			k := min(c.half-len(c.ring), len(b))
			c.ring = append(c.ring, b[:k]...)
			b = b[k:]
			continue
		}
		k := copy(c.ring[c.pos:], b)
		b = b[k:]
		c.pos = (c.pos + k) % c.half
	}
	return n, nil
}

// String returns everything written, or head + omission marker + tail.
func (c *Capped) String() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	tail := make([]byte, 0, len(c.ring))
	tail = append(tail, c.ring[c.pos:]...)
	tail = append(tail, c.ring[:c.pos]...)
	if c.total <= int64(len(c.head)+len(tail)) {
		return string(c.head) + string(tail)
	}
	head := c.head
	// Drop a trailing partial rune from head.
	for i := 1; i <= utf8.UTFMax-1 && i <= len(head); i++ {
		if r := head[len(head)-i:]; utf8.RuneStart(r[0]) {
			if !utf8.FullRune(r) {
				head = head[:len(head)-i]
			}
			break
		}
	}
	// Drop leading continuation bytes from tail.
	for len(tail) > 0 && !utf8.RuneStart(tail[0]) {
		tail = tail[1:]
	}
	omitted := c.total - int64(len(head)) - int64(len(tail))
	return fmt.Sprintf("%s\n[... %d bytes omitted ...]\n%s", head, omitted, tail)
}

// Truncate returns s as NewCapped(limit) would render it after writing s.
func Truncate(s string, limit int) string {
	c := NewCapped(limit)
	c.Write([]byte(s))
	return c.String()
}
