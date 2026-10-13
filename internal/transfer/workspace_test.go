package transfer

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 🔴 The fence table (#2673 requirement 2): every path a plugin or the platform
// names resolves — through the filesystem's own links — to inside the
// workspace, or is refused with a reason.
func TestTheWorkspaceFenceTable(t *testing.T) {
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret"), []byte("s"), 0o600); err != nil {
		t.Fatal(err)
	}
	ws, err := OpenWorkspace(t.TempDir(), false, "")
	if err != nil {
		t.Fatal(err)
	}
	root := ws.Root()
	mustMkdir(t, filepath.Join(root, "docs", "deep"))
	mustWrite(t, filepath.Join(root, "docs", "deep", "a.txt"))
	mustMkdir(t, filepath.Join(root, "public"))
	// A link inside the workspace leading OUT of it, one leading to a file out
	// of it, one staying inside, and a hidden name for an ordinary directory.
	mustLink(t, outside, filepath.Join(root, "escape"))
	mustLink(t, filepath.Join(outside, "secret"), filepath.Join(root, "leak.txt"))
	mustLink(t, filepath.Join(root, "docs"), filepath.Join(root, "docs-link"))
	mustLink(t, filepath.Join(root, "public"), filepath.Join(root, ".hidden"))

	refused := map[string]FenceReason{
		"":                               FenceEmpty,
		"../x":                           FenceParent,
		"docs/../../x":                   FenceParent,
		"docs/../a.txt":                  FenceParent,
		filepath.Join(outside, "secret"): FenceOutside,
		"/etc/passwd":                    FenceOutside,
		"escape/secret":                  FenceOutside,
		"escape":                         FenceOutside,
		"leak.txt":                       FenceOutside,
		"~/.ssh/id_ed25519":              FenceDotfile,
		".env":                           FenceDotfile,
		"docs/.git/config":               FenceDotfile,
		".hidden/x":                      FenceDotfile,
		".uffici/partial/x":              FenceDotfile,
		"a\x00b":                         FenceInvalid,
	}
	for name, want := range refused {
		_, err := ws.Resolve(name)
		var fence *FenceError
		if !errors.As(err, &fence) || fence.Reason != want {
			t.Errorf("Resolve(%q) = %v, want refused as %s", name, err, want)
		}
		if err != nil && strings.Contains(err.Error(), name) && name != "" {
			t.Errorf("the refusal of %q names the path", name)
		}
	}

	accepted := map[string]string{
		"docs/deep/a.txt":           filepath.Join(root, "docs", "deep", "a.txt"),
		"docs/deep/new/file.pdf":    filepath.Join(root, "docs", "deep", "new", "file.pdf"),
		"docs-link/deep/a.txt":      filepath.Join(root, "docs", "deep", "a.txt"),
		filepath.Join(root, "docs"): filepath.Join(root, "docs"),
		"./docs/deep/a.txt":         filepath.Join(root, "docs", "deep", "a.txt"),
	}
	for name, want := range accepted {
		got, err := ws.Resolve(name)
		if err != nil || got != want {
			t.Errorf("Resolve(%q) = %q, %v; want %q", name, got, err, want)
		}
	}
	if rel := ws.Rel(filepath.Join(root, "docs", "deep", "a.txt")); rel != "docs/deep/a.txt" {
		t.Errorf("Rel = %q", rel)
	}
}

func TestDotfilesAreAdmittedOnlyWhenTheOwnerAllowsThem(t *testing.T) {
	ws, err := OpenWorkspace(t.TempDir(), true, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ws.Resolve(".env"); err != nil {
		t.Fatalf("an allowed dotfile was refused: %v", err)
	}
	// The companion's own corner stays refused whatever the setting.
	if _, err := ws.Resolve(".uffici/partial/x"); err == nil {
		t.Fatal("the companion's partial directory was reachable")
	}
	if _, err := ws.Resolve("../x"); err == nil {
		t.Fatal("a parent escape was admitted with dotfiles allowed")
	}
}

func TestOpenWorkspaceRefusesARelativeRootAndCreatesAMissingOne(t *testing.T) {
	if _, err := OpenWorkspace("relative/root", false, ""); err == nil {
		t.Fatal("a relative root was accepted")
	}
	root := filepath.Join(t.TempDir(), "a", "b")
	ws, err := OpenWorkspace(root, false, "")
	if err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(filepath.Join(ws.Root(), ".uffici", "partial")); err != nil || !info.IsDir() {
		t.Fatal("the partial directory was not created")
	}
	blocked := filepath.Join(t.TempDir(), "file")
	mustWrite(t, blocked)
	if _, err := OpenWorkspace(filepath.Join(blocked, "under"), false, ""); err == nil {
		t.Fatal("a workspace under a file was accepted")
	}
}

func TestWritable(t *testing.T) {
	dir := t.TempDir()
	if err := Writable(dir); err != nil {
		t.Fatalf("a writable directory: %v", err)
	}
	if err := Writable(filepath.Join(dir, "missing")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a missing directory: %v", err)
	}
	file := filepath.Join(dir, "f")
	mustWrite(t, file)
	if err := Writable(file); err == nil {
		t.Fatal("a file passed as a directory")
	}
	locked := filepath.Join(dir, "locked")
	mustMkdir(t, locked)
	if err := os.Chmod(locked, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o700) })
	if os.Geteuid() != 0 {
		if err := Writable(locked); err == nil {
			t.Fatal("a read-only directory passed")
		}
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 2 {
		t.Fatalf("the probe left a file behind: %d entries", len(entries))
	}
}

func mustMkdir(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o750); err != nil {
		t.Fatal(err)
	}
}

func mustWrite(t *testing.T, path string) {
	t.Helper()
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func mustLink(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
}
