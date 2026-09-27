// Package docs holds this module's documentation guard.
//
// 🔴 It exists because the platform's own doc-link guard cannot reach here.
// that repository's own doc-link guard scans its instruction set and the root
// `CLAUDE.md` only, so every relative link in this child's Markdown — its
// README and its CLAUDE.md — is checked by nothing on the platform side, and
// by nothing at all once the child is mirrored out. The check therefore lives
// in the child's own toolchain, inside its own `check` entry, which is the
// rule for every gate a child needs.
package docs

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// Markdown inline links: the target of `[text](target)`.
var markdownLink = regexp.MustCompile(`\[[^\]]*\]\(([^)]+)\)`)

// moduleRoot walks up from the test's directory to the directory holding go.mod.
func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("cannot read the working directory: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("walked to the filesystem root without finding go.mod — the guard cannot locate the module")
		}
		dir = parent
	}
}

// markdownFiles lists every .md file in the module, skipping nothing: the child
// is small and everything in it is authored.
func markdownFiles(t *testing.T, root string) []string {
	t.Helper()
	var found []string
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if entry.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(entry.Name(), ".md") {
			found = append(found, path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", root, err)
	}
	return found
}

func TestEveryRelativeMarkdownLinkResolves(t *testing.T) {
	root := moduleRoot(t)
	files := markdownFiles(t, root)

	// 🔴 Non-vacuity, twice over: an empty file set or a link set that never
	// matched would make the assertion below pass forever over nothing, which
	// is the failure shape this repository calls defect-ledger class 1.
	if len(files) < 2 {
		t.Fatalf("found %d Markdown file(s) under %s — README.md and CLAUDE.md are both expected, so the walk is broken", len(files), root)
	}

	checked := 0
	for _, file := range files {
		source, err := os.ReadFile(file)
		if err != nil {
			t.Fatalf("reading %s: %v", file, err)
		}
		for _, match := range markdownLink.FindAllStringSubmatch(string(source), -1) {
			target := strings.TrimSpace(match[1])
			// External and in-page links are somebody else's problem.
			if target == "" || strings.Contains(target, "://") || strings.HasPrefix(target, "#") ||
				strings.HasPrefix(target, "mailto:") {
				continue
			}
			target = strings.SplitN(target, "#", 2)[0]
			if target == "" {
				continue
			}
			checked++
			resolved := filepath.Join(filepath.Dir(file), target)
			if _, err := os.Stat(resolved); err != nil {
				rel, _ := filepath.Rel(root, file)
				t.Errorf("%s links %q, which does not exist (looked at %s)", rel, match[1], resolved)
			}
		}
	}

	if checked == 0 {
		t.Fatal("no relative Markdown links were checked — the link regex matched nothing, so this guard asserts nothing")
	}
}
