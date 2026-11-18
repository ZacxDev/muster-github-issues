package textcut_test

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/ZacxDev/muster-github-issues/internal/textcut"
)

func TestPrefix(t *testing.T) {
	for _, tc := range []struct {
		name, s string
		n       int
		want    string
	}{
		{"short", "abc", 5, "abc"},
		{"exact", "abcde", 5, "abcde"},
		{"ascii", "abcdef", 4, "abcd"},
		{"zero", "abc", 0, ""},
		{"splits é", "aé", 2, "a"},                          // é is 2 bytes
		{"splits 😀", "ab😀c", 4, "ab"},                       // 😀 is 4 bytes
		{"after 😀", "ab😀c", 6, "ab😀"},                       // boundary right after it
		{"invalid bytes", "\xff\xfe\xfdabc", 2, "\xff\xfe"}, // not UTF-8: cut where asked
		{"invalid then text", "\xff" + strings.Repeat("a", 10), 5, "\xffaaaa"},
	} {
		if got := textcut.Prefix(tc.s, tc.n); got != tc.want {
			t.Errorf("%s: Prefix(%q, %d) = %q, want %q", tc.name, tc.s, tc.n, got, tc.want)
		}
	}
}

// The bug this package replaced: one invalid byte early in the text made the
// old cut strip the whole message, a byte at a time, to "".
func TestOneInvalidByteDoesNotEmptyTheCut(t *testing.T) {
	s := "\xff" + strings.Repeat("x", 4000)
	got := textcut.Prefix(s, 2048)
	if len(got) != 2048 {
		t.Fatalf("Prefix of a body starting with \\xff kept %d bytes, want 2048", len(got))
	}
	// Valid text is never split mid-character.
	v := strings.Repeat("é", 2000)
	if got := textcut.Prefix(v, 2047); !utf8.ValidString(got) || len(got) != 2046 {
		t.Fatalf("Prefix of é×2000 to 2047 = %d bytes (valid %t), want 2046", len(got), utf8.ValidString(got))
	}
	if got := textcut.Cap(s, 10, "…"); got != "\xffxxxxxxxxx…" {
		t.Fatalf("Cap = %q", got)
	}
}
