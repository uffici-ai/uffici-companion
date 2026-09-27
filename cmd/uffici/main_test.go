package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The CLI's contract is its exit codes and its refusals. Both are what a CI job
// reads, so both are tested here rather than left to a manual run.

func TestVersionPrintsAndExitsZero(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := run([]string{"--version"}, &out, &errOut); code != 0 {
		t.Fatalf("--version exited %d, want 0", code)
	}
	if !strings.Contains(out.String(), "uffici-companion") {
		t.Fatalf("--version printed %q", out.String())
	}
}

// 🔴 Exit 2, not 1. "This build never ran your script" and "your script failed"
// are different answers, and a CI job that collapsed them would report a
// misconfiguration as a product defect.
func TestAMisconfigurationExitsTwo(t *testing.T) {
	home := t.TempDir()
	for name, args := range map[string][]string{
		"no fixture":       {"run", "--home", home},
		"a missing script": {"run", "--home", home, "--fixture", filepath.Join(home, "absent.json")},
		"an unknown flag":  {"--nonsense"},
	} {
		t.Run(name, func(t *testing.T) {
			var out, errOut bytes.Buffer
			if code := run(args, &out, &errOut); code != 2 {
				t.Fatalf("exited %d, want 2\nstderr: %s", code, errOut.String())
			}
		})
	}
}

// A script that cannot assert anything must be refused at load, not played.
func TestAnInvalidScriptExitsTwo(t *testing.T) {
	home := t.TempDir()
	script := filepath.Join(home, "empty.json")
	if err := os.WriteFile(script, []byte(`{"v":1,"conversations":{},"steps":[]}`), 0o600); err != nil {
		t.Fatalf("writing: %v", err)
	}
	var out, errOut bytes.Buffer
	if code := run([]string{"run", "--home", home, "--fixture", script, "--once"}, &out, &errOut); code != 2 {
		t.Fatalf("exited %d, want 2\nstderr: %s", code, errOut.String())
	}
	if !strings.Contains(errOut.String(), "conversations") {
		t.Fatalf("the refusal does not say what was wrong: %s", errOut.String())
	}
}

// The `run` verb is optional; both spellings must reach the same place.
func TestTheRunVerbIsOptional(t *testing.T) {
	var withVerb, withoutVerb bytes.Buffer
	var ignored bytes.Buffer
	withCode := run([]string{"run", "--version"}, &withVerb, &ignored)
	withoutCode := run([]string{"--version"}, &withoutVerb, &ignored)
	if withCode != withoutCode || withVerb.String() != withoutVerb.String() {
		t.Fatalf("`run --version` and `--version` disagree: %d/%q vs %d/%q",
			withCode, withVerb.String(), withoutCode, withoutVerb.String())
	}
}

// `uffici doctor` with nowhere to probe says so and exits 2, never 0.
func TestDoctorWithoutAGatewayURLExitsTwo(t *testing.T) {
	t.Setenv("UFFICI_GATEWAY_URL", "")
	var out, errOut bytes.Buffer
	if code := run([]string{"doctor", "--home", t.TempDir()}, &out, &errOut); code != 2 {
		t.Fatalf("exited %d, want 2\nstderr: %s", code, errOut.String())
	}
	if !strings.Contains(errOut.String(), "UFFICI_GATEWAY_URL") {
		t.Fatalf("the refusal does not name what is missing: %s", errOut.String())
	}
	if code := run([]string{"doctor", "--nonsense"}, &out, &errOut); code != 2 {
		t.Fatalf("an unknown doctor flag exited %d", code)
	}
}

// `uffici pair` with nowhere to pair against says so and exits 2 (#1966).
func TestPairWithoutAGatewayURLExitsTwo(t *testing.T) {
	t.Setenv("UFFICI_GATEWAY_URL", "")
	var out, errOut bytes.Buffer
	if code := run([]string{"pair", "--home", t.TempDir(), "--harness", "hermes"}, &out, &errOut); code != 2 {
		t.Fatalf("exited %d, want 2\nstderr: %s", code, errOut.String())
	}
	if !strings.Contains(errOut.String(), "UFFICI_GATEWAY_URL") {
		t.Fatalf("the refusal does not name what is missing: %s", errOut.String())
	}
	if code := run([]string{"pair", "--nonsense"}, &out, &errOut); code != 2 {
		t.Fatalf("an unknown pair flag exited %d", code)
	}
}
