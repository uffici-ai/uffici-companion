package transfer

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// 🔴 The companion is HARNESS-AGNOSTIC: the code that moves files knows the
// platform, the local wire and a workspace — never a harness. A harness's paths,
// media rules and settings are its plugin's, through the same two ops every
// plugin calls. This fails on any harness's name in the transfer code, so the
// rule holds mechanically and not by review.
//
// The names are every harness a plugin exists or is planned for, and the
// plugin repositories' prefix. The platform's repository holds the same list
// against its own register of plugin children, from outside this module.
var harnessNames = regexp.MustCompile(`(?i)hermes|openclaw|uffici-for-`)

// transferSources are the non-test Go sources the rule covers: this package,
// and the gateway's two files that carry a transfer's bytes and grants.
func transferSources(t *testing.T) []string {
	t.Helper()
	own, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	gateway := []string{
		filepath.Join("..", "infra", "gateway", "files.go"),
		filepath.Join("..", "infra", "gateway", "transfer.go"),
	}
	var out []string
	for _, file := range append(own, gateway...) {
		if !strings.HasSuffix(file, "_test.go") {
			out = append(out, file)
		}
	}
	return out
}

func TestTheTransferCodeNamesNoHarness(t *testing.T) {
	files := transferSources(t)
	// Non-vacuity: a glob that matched nothing would pass over nothing.
	if len(files) < 8 {
		t.Fatalf("scanned %d files — the transfer sources were not found", len(files))
	}
	for _, file := range files {
		raw, err := os.ReadFile(file) //nolint:gosec // this module's own sources
		if err != nil {
			t.Fatalf("reading %s: %v", file, err)
		}
		for i, line := range strings.Split(string(raw), "\n") {
			if match := harnessNames.FindString(line); match != "" {
				t.Errorf("%s:%d names the harness %q — a harness's conventions belong to its plugin", file, i+1, match)
			}
		}
	}
}

// The matcher itself catches what it is for — otherwise a broken pattern would
// pass every source.
func TestTheHarnessMatcherCatchesEveryName(t *testing.T) {
	for _, line := range []string{"if harness == \"hermes\"", "HERMES_MEDIA_ALLOW_DIRS", "openclaw media", "uffici-for-hermes"} {
		if !harnessNames.MatchString(line) {
			t.Errorf("the matcher missed %q", line)
		}
	}
	if harnessNames.MatchString("the workspace's conversations directory") {
		t.Error("the matcher flags ordinary words")
	}
}
