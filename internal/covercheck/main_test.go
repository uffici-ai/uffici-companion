package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 🔴 A broken gate reports green, which is worse than no gate: it converts an
// unchecked requirement into a checked-looking one. So the reader itself is
// tested, including the shapes that must NOT be read as a passing total.

// writeProfile writes a coverage profile `go tool cover` can read.
func writeProfile(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "cover.out")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("writing the profile: %v", err)
	}
	return path
}

func TestItReadsTheTotalFromARealProfile(t *testing.T) {
	// One file, two statement blocks, one covered: 50%.
	profile := writeProfile(t, strings.Join([]string{
		"mode: set",
		"github.com/uffici-ai/uffici-companion/internal/covercheck/main.go:20.13,22.2 1 1",
		"github.com/uffici-ai/uffici-companion/internal/covercheck/main.go:24.13,26.2 1 0",
		"",
	}, "\n"))

	total, err := totalCoverage(profile)
	if err != nil {
		t.Fatalf("totalCoverage: %v", err)
	}
	if total < 49 || total > 51 {
		t.Fatalf("read %.1f%%, want about 50%%", total)
	}
}

func TestAnUnreadableProfileIsAnErrorNotAZero(t *testing.T) {
	// 🔴 Zero would read as "nothing is covered" and fail the build for the
	// wrong reason; the caller must be told the profile could not be read.
	if _, err := totalCoverage(filepath.Join(t.TempDir(), "absent.out")); err == nil {
		t.Fatal("a missing profile was read as a coverage figure")
	}
	if _, err := totalCoverage(writeProfile(t, "this is not a coverage profile\n")); err == nil {
		t.Fatal("a malformed profile was read as a coverage figure")
	}
}

func TestTheGateExitCodes(t *testing.T) {
	passing := writeProfile(t, strings.Join([]string{
		"mode: set",
		"github.com/uffici-ai/uffici-companion/internal/covercheck/main.go:20.13,22.2 1 1",
		"",
	}, "\n"))

	var out, errOut strings.Builder
	if code := run([]string{"-min", "90", passing}, &out, &errOut); code != 0 {
		t.Fatalf("a profile above the line exited %d, want 0\n%s", code, errOut.String())
	}
	if !strings.Contains(out.String(), "covered") {
		t.Fatalf("a passing run said %q", out.String())
	}

	// A profile that is genuinely BELOW the line: one covered block, one not.
	partial := writeProfile(t, strings.Join([]string{
		"mode: set",
		"github.com/uffici-ai/uffici-companion/internal/covercheck/main.go:20.13,22.2 1 1",
		"github.com/uffici-ai/uffici-companion/internal/covercheck/main.go:24.13,26.2 1 0",
		"",
	}, "\n"))

	out.Reset()
	errOut.Reset()
	if code := run([]string{"-min", "90", partial}, &out, &errOut); code != 1 {
		t.Fatalf("a profile at 50%% exited %d against a 90%% minimum, want 1", code)
	}
	// 🔴 The failure must say where to look, or the next person just lowers the
	// number until it passes.
	if !strings.Contains(errOut.String(), "go tool cover") {
		t.Fatalf("the failure does not say how to find the gap: %q", errOut.String())
	}

	// A usage error is 2, distinct from a coverage miss: "you ran it wrong" and
	// "your coverage is low" are different answers.
	out.Reset()
	errOut.Reset()
	if code := run(nil, &out, &errOut); code != 2 {
		t.Fatalf("a missing profile argument exited %d, want 2", code)
	}
	if code := run([]string{"-min", "90", "/absent/profile.out"}, &out, &errOut); code != 2 {
		t.Fatalf("an unreadable profile exited %d, want 2", code)
	}
}
