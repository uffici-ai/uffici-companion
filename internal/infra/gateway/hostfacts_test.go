package gateway

import (
	"errors"
	"strings"
	"testing"
	"unicode/utf16"
)

func reader(files map[string]string) func(string) ([]byte, error) {
	return func(path string) ([]byte, error) {
		if content, ok := files[path]; ok {
			return []byte(content), nil
		}
		return nil, errors.New("absent")
	}
}

// #2236: the machine's KIND — its OS, architecture and the release a person
// would recognise — and nothing that identifies it.
func TestHostFactsNameTheReleaseAPersonWouldRecognise(t *testing.T) {
	linux := HostFactsOf("linux", "amd64", reader(map[string]string{
		"/etc/os-release": "NAME=\"Debian GNU/Linux\"\nPRETTY_NAME=\"Debian GNU/Linux 13 (trixie)\"\nVERSION_ID=\"13\"\n",
	}))
	if linux.OS != "linux" || linux.Arch != "amd64" || linux.OSRelease != "Debian GNU/Linux 13 (trixie)" {
		t.Fatalf("linux: %+v", linux)
	}
	named := HostFactsOf("linux", "arm64", reader(map[string]string{"/etc/os-release": "NAME=Alpine Linux\n"}))
	if named.OSRelease != "Alpine Linux" {
		t.Fatalf("with no PRETTY_NAME the NAME is the release: %+v", named)
	}
	mac := HostFactsOf("darwin", "arm64", reader(map[string]string{
		"/System/Library/CoreServices/SystemVersion.plist": "<dict>\n\t<key>ProductVersion</key>\n\t<string>26.1</string>\n</dict>",
	}))
	if mac.OSRelease != "macOS 26.1" {
		t.Fatalf("darwin: %+v", mac)
	}
}

func TestAnUnreadableReleaseIsLeftOutNeverGuessed(t *testing.T) {
	facts := HostFactsOf("linux", "amd64", reader(nil))
	if facts.OSRelease != "" || facts.OS != "linux" {
		t.Fatalf("%+v", facts)
	}
	if other := HostFactsOf("freebsd", "amd64", reader(nil)); other.OSRelease != "" {
		t.Fatalf("an OS this reads nothing for reported %+v", other)
	}
}

func TestAReleaseLongerThanTheProtocolAllowsIsClipped(t *testing.T) {
	facts := HostFactsOf("linux", "amd64", reader(map[string]string{
		"/etc/os-release": "PRETTY_NAME=\"" + strings.Repeat("é", 80) + "\"\n",
	}))
	if got := len([]rune(facts.OSRelease)); got != hostFactMaxChars {
		t.Fatalf("clipped to %d characters, want %d", got, hostFactMaxChars)
	}
}

// 🔴 The gateway counts UTF-16 units: 40 emoji are 40 runes and 80 units, and a
// fact over its bound refuses the whole ticket.
func TestAFactIsClippedAsTheGatewayCountsIt(t *testing.T) {
	facts := HostFactsOf("linux", "amd64", reader(map[string]string{
		"/etc/os-release": "PRETTY_NAME=\"" + strings.Repeat("😀", 40) + "\x07\"\n",
	}))
	units := len(utf16.Encode([]rune(facts.OSRelease)))
	if units > hostFactMaxChars || units == 0 {
		t.Fatalf("clipped to %d UTF-16 units, want at most %d", units, hostFactMaxChars)
	}
	if strings.ContainsRune(facts.OSRelease, 0x07) {
		t.Fatal("a control character reached the wire")
	}
}

func TestABidiOverrideNeverReachesTheWire(t *testing.T) {
	facts := HostFactsOf("linux", "amd64", reader(map[string]string{
		"/etc/os-release": "PRETTY_NAME=\"Debian\u202e\u200b 13\"\n",
	}))
	if facts.OSRelease != "Debian 13" {
		t.Fatalf("the release reached the wire as %q", facts.OSRelease)
	}
	if long := HostFactsOf(strings.Repeat("x", 40), "amd64", reader(nil)); len(long.OS) != hostKindMaxChars {
		t.Fatalf("the OS is %d characters, want the protocol's %d", len(long.OS), hostKindMaxChars)
	}
}

func TestTheLocalMachineIsDescribed(t *testing.T) {
	if facts := localHostFacts(); facts.OS == "" || facts.Arch == "" {
		t.Fatalf("%+v", facts)
	}
}
