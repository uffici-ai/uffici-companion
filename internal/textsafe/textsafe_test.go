package textsafe

import "testing"

func TestVisibleDropsControlAndFormatCharacters(t *testing.T) {
	for in, want := range map[string]string{
		"github":           "github",
		"git\x1b[2Jhub\n":  "git[2Jhub",
		"abc\u202edef":     "abcdef",
		"zero\u200bwidth":  "zerowidth",
		"ünïcödé — fine 🙂": "ünïcödé — fine 🙂",
	} {
		if got := Visible(in); got != want {
			t.Errorf("Visible(%q) = %q, want %q", in, got, want)
		}
	}
}
