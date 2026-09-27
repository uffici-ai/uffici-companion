package build

import "testing"

// 🔴 Non-vacuous by construction: it asserts the fallback a developer build
// actually takes, and fails if Version() ever stops honouring the link-time
// override (the release's only way to name itself).
func TestVersionFallsBackToDevWhenUnstamped(t *testing.T) {
	t.Cleanup(func() { version = "" })

	version = ""
	if got := Version(); got != DevVersion {
		t.Fatalf("unstamped build reported %q, want %q", got, DevVersion)
	}

	version = "1.2.3"
	if got := Version(); got != "1.2.3" {
		t.Fatalf("stamped build reported %q, want the link-time value 1.2.3", got)
	}
}

func TestNameIsTheRepositoryName(t *testing.T) {
	// The mirror pushes this module onto a repository of the same name
	// (epic #1994 decision 34), and the installers name the binary from it.
	if Name != "uffici-companion" {
		t.Fatalf("Name is %q — it must match the child directory and its future repository", Name)
	}
}

// A customer verifying a signed release ties the running binary to it through
// these, so an unstamped build must say "unknown" rather than an empty string —
// an empty field reads as "the field is missing", not "this is a dev build".
func TestUnstampedBuildsReportUnknownRatherThanNothing(t *testing.T) {
	t.Cleanup(func() { commit, buildDate = "", "" })

	commit, buildDate = "", ""
	if got := Commit(); got != "unknown" {
		t.Fatalf("Commit() on an unstamped build is %q, want \"unknown\"", got)
	}
	if got := Date(); got != "unknown" {
		t.Fatalf("Date() on an unstamped build is %q, want \"unknown\"", got)
	}

	commit, buildDate = "abc1234", "2026-09-18T00:00:00Z"
	if got := Commit(); got != "abc1234" {
		t.Fatalf("Commit() is %q, want the link-time value", got)
	}
	if got := Date(); got != "2026-09-18T00:00:00Z" {
		t.Fatalf("Date() is %q, want the link-time value", got)
	}
}
