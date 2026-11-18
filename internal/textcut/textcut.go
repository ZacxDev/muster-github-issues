// Package textcut cuts text to a byte budget without splitting a character.
package textcut

import "unicode/utf8"

// Prefix is s cut to at most n bytes. When the cut would split a UTF-8
// character it moves back to that character's first byte, looking back at
// most utf8.UTFMax-1 bytes. Bytes that are not UTF-8 at all (a body that is
// not text) are cut like any other byte: they never make it drop more.
func Prefix(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if len(s) <= n {
		return s
	}
	// s[n] is the first byte left out. If it continues a character that
	// starts in s[:n], cut before that character instead.
	for i := n; i > n-utf8.UTFMax && i > 0; i-- {
		if utf8.RuneStart(s[i]) {
			return s[:i]
		}
	}
	return s[:n] // no character start within reach: not UTF-8 here
}

// Cap is s cut to at most n bytes with marker appended when it was cut.
func Cap(s string, n int, marker string) string {
	if len(s) <= n {
		return s
	}
	return Prefix(s, n) + marker
}
