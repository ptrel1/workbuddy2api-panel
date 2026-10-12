package logfmt

import "testing"

func TestLabel(t *testing.T) {
	cases := []struct{ uid, nick, want string }{
		{"1234567890abcdef", "周五口", "12345678(周五口)"},
		{"1234567890abcdef", "", "12345678"},
		{"short", "pt", "short(pt)"},
		{"", "x", "-(x)"},
		{"", "", "-"},
	}
	for _, c := range cases {
		if got := Label(c.uid, c.nick); got != c.want {
			t.Errorf("Label(%q,%q)=%q want %q", c.uid, c.nick, got, c.want)
		}
	}
}
