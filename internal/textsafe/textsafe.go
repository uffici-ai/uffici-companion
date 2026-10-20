// Package textsafe holds the companion's one rule for text it did not write
// and is about to show: control and format characters removed.
package textsafe

import (
	"strings"
	"unicode"
)

// Visible drops every control character and every format character (Cf) from
// value. A bidi override or a zero-width space reverses a line or makes two
// names look alike; a control character can steer the terminal it is printed
// on. The one copy of that rule: the host facts and the doctor both use it.
func Visible(value string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return -1
		}
		return r
	}, value)
}
