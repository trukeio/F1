package repodir

import (
	"fmt"
	"strconv"
	"strings"
)

// escape percent-encodes a field of the line-based repository files (refs.log,
// locks): every byte outside printable ASCII (0x21–0x7E) and every '%'
// becomes %XX. The empty string is written as "-", and a literal "-" as
// "%2D", so that fields are never empty and never contain spaces.
func escape(s string) string {
	if s == "" {
		return "-"
	}
	if s == "-" {
		return "%2D"
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c <= 0x20 || c >= 0x7f || c == '%' {
			fmt.Fprintf(&b, "%%%02X", c)
		} else {
			b.WriteByte(c)
		}
	}
	return b.String()
}

// unescape reverses escape. It rejects malformed escapes and unescaped bytes
// that escape would have encoded.
func unescape(s string) (string, error) {
	if s == "-" {
		return "", nil
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '%':
			if i+2 >= len(s) {
				return "", fmt.Errorf("truncated escape in %q", s)
			}
			v, err := strconv.ParseUint(s[i+1:i+3], 16, 8)
			if err != nil {
				return "", fmt.Errorf("bad escape in %q", s)
			}
			b.WriteByte(byte(v))
			i += 2
		case c <= 0x20 || c >= 0x7f:
			return "", fmt.Errorf("unescaped byte 0x%02x in %q", c, s)
		default:
			b.WriteByte(c)
		}
	}
	return b.String(), nil
}
