package gateway

import (
	"bufio"
	"bytes"
	"os"
	"regexp"
	"runtime"
	"strings"
	"unicode"
	"unicode/utf16"

	"github.com/uffici-ai/uffici-companion/internal/protocol"
)

// hostFactMaxChars is the protocol's bound on the OS release (#2236), and
// hostKindMaxChars its bound on the OS and the architecture.
const (
	hostFactMaxChars = 64
	hostKindMaxChars = 32
)

// macOSVersion reads ProductVersion from SystemVersion.plist.
var macOSVersion = regexp.MustCompile(`<key>ProductVersion</key>\s*<string>([^<]+)</string>`)

// HostFactsOf describes the machine the companion runs on (#2236): its OS and
// architecture as Go names them, and the release a person would recognise —
// `PRETTY_NAME` from /etc/os-release on Linux, `macOS <version>` on darwin —
// when it can be read.
//
// 🔴 The machine's KIND and nothing else: never the hostname, a user or a
// path. The facts reach the platform on every connection ticket and are shown
// to the organisation's admins, so anything that identified a person or a
// particular machine would be personal data carried for no purpose.
func HostFactsOf(goos, goarch string, readFile func(string) ([]byte, error)) *protocol.HostFacts {
	facts := &protocol.HostFacts{OS: clipTo(goos, hostKindMaxChars), Arch: clipTo(goarch, hostKindMaxChars)}
	switch goos {
	case "linux":
		if raw, err := readFile("/etc/os-release"); err == nil {
			facts.OSRelease = clipFact(osReleaseName(raw))
		}
	case "darwin":
		if raw, err := readFile("/System/Library/CoreServices/SystemVersion.plist"); err == nil {
			if match := macOSVersion.FindSubmatch(raw); match != nil {
				facts.OSRelease = clipFact("macOS " + strings.TrimSpace(string(match[1])))
			}
		}
	}
	return facts
}

// localHostFacts describes this machine.
func localHostFacts() *protocol.HostFacts {
	return HostFactsOf(runtime.GOOS, runtime.GOARCH, os.ReadFile)
}

// osReleaseName is PRETTY_NAME, else NAME, from an os-release file.
func osReleaseName(raw []byte) string {
	values := map[string]string{}
	scanner := bufio.NewScanner(bytes.NewReader(raw))
	for scanner.Scan() {
		key, value, found := strings.Cut(strings.TrimSpace(scanner.Text()), "=")
		if !found {
			continue
		}
		values[key] = strings.Trim(strings.TrimSpace(value), `"'`)
	}
	if name := values["PRETTY_NAME"]; name != "" {
		return name
	}
	return values["NAME"]
}

// clipFact bounds a fact to the protocol's longest (hostFactMaxChars).
func clipFact(value string) string { return clipTo(value, hostFactMaxChars) }

// clipTo bounds a fact to limit UTF-16 units — what the gateway's schema counts —
// with control and format characters removed (a bidi override or a zero-width
// space on the Connect tab reverses a line or makes two facts look alike).
//
// 🔴 Not runes: an emoji is one rune and two units, so a release name clipped to
// 64 runes can still be over the gateway's 64 and refused — and the ticket that
// carries it is refused whole, taking the companion offline over a label.
func clipTo(value string, limit int) string {
	value = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return -1
		}
		return r
	}, strings.TrimSpace(value))
	units := 0
	for i, r := range value {
		units += utf16.RuneLen(r)
		if units > limit {
			return strings.TrimSpace(value[:i])
		}
	}
	return value
}
