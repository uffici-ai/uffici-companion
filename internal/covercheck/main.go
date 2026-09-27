// Command covercheck fails when a coverage profile falls below a minimum.
//
// 🔴 It exists because `go test -cover` PRINTS a percentage and exits 0 whatever
// it is. A coverage requirement that only a human reads is not a requirement —
// it is a number that drifts down one commit at a time, and nobody can say when
// it crossed the line. This makes crossing it a failed check.
package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run holds the logic so the exit codes are testable without a process — the
// same shape as `cmd/uffici`. A gate whose own failure path is untested is a
// gate nobody has seen fail.
func run(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("covercheck", flag.ContinueOnError)
	flags.SetOutput(stderr)
	minimum := flags.Float64("min", 90, "the minimum percentage of covered statements")
	if err := flags.Parse(args); err != nil {
		return 2
	}

	profile := flags.Arg(0)
	if profile == "" {
		_, _ = fmt.Fprintln(stderr, "covercheck: usage: covercheck -min <percent> <profile>")
		return 2
	}

	total, err := totalCoverage(profile)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "covercheck: %v\n", err)
		return 2
	}
	if total < *minimum {
		_, _ = fmt.Fprintf(stderr,
			"covercheck: %.1f%% of statements are covered, and this module requires %.1f%%.\n"+
				"Run `go tool cover -func=%s | sort -k3 -n | head` to see where.\n",
			total, *minimum, profile)
		return 1
	}
	_, _ = fmt.Fprintf(stdout, "covercheck: %.1f%% covered (minimum %.1f%%)\n", total, *minimum)
	return 0
}

// totalCoverage reads the `total:` line `go tool cover -func` prints last.
func totalCoverage(profile string) (float64, error) {
	output, err := exec.Command("go", "tool", "cover", "-func="+profile).Output()
	if err != nil {
		return 0, fmt.Errorf("reading %s: %w", profile, err)
	}
	lines := strings.Split(strings.TrimSpace(string(output)), "\n")
	if len(lines) == 0 {
		return 0, fmt.Errorf("%s produced no coverage lines", profile)
	}
	last := lines[len(lines)-1]
	if !strings.HasPrefix(last, "total:") {
		return 0, fmt.Errorf("the last line of the profile is not a total: %q", last)
	}
	fields := strings.Fields(last)
	percent := strings.TrimSuffix(fields[len(fields)-1], "%")
	value, err := strconv.ParseFloat(percent, 64)
	if err != nil {
		return 0, fmt.Errorf("parsing the total %q: %w", percent, err)
	}
	return value, nil
}
