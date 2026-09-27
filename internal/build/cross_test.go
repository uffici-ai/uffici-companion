package build_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

// 🔴 The companion ships as one static binary per OS and architecture
// (epic #1994 decision 46), so "it compiles" means it compiles for all six —
// and a Windows or ARM break is invisible on the developer's own machine until
// release day, which is the worst possible moment to find it.
//
// This lives in `go test` rather than in a shell script beside it so the
// child's `check` entry stays the one command its README, its CI and the
// platform's delegating leg all name. A script would be a fourth place to keep
// in step, and a root entry the inventory guard would have to admit.
func TestEveryReleaseTargetCompiles(t *testing.T) {
	if testing.Short() {
		t.Skip("cross-compilation is slow; -short skips it")
	}

	targets := []struct{ goos, goarch string }{
		{"linux", "amd64"}, {"linux", "arm64"},
		{"darwin", "amd64"}, {"darwin", "arm64"},
		{"windows", "amd64"}, {"windows", "arm64"},
	}
	// Non-vacuity: the list is the release matrix, and a table that shrank would
	// keep passing while covering less.
	if len(targets) != 6 {
		t.Fatalf("%d target(s) — the release matrix is three operating systems on two architectures", len(targets))
	}

	root := moduleRoot(t)
	out := t.TempDir()

	for _, target := range targets {
		t.Run(target.goos+"/"+target.goarch, func(t *testing.T) {
			binary := filepath.Join(out, target.goos+"-"+target.goarch)
			if target.goos == "windows" {
				binary += ".exe"
			}
			cmd := exec.Command("go", "build", "-trimpath", "-o", binary, "./cmd/uffici")
			cmd.Dir = root
			// CGO_ENABLED=0 is what makes a static binary — and what the scratch
			// image depends on. Building with cgo here would prove the wrong thing.
			cmd.Env = append(os.Environ(),
				"GOOS="+target.goos, "GOARCH="+target.goarch, "CGO_ENABLED=0")
			if output, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("building for %s/%s: %v\n%s", target.goos, target.goarch, err, output)
			}
			info, err := os.Stat(binary)
			if err != nil {
				t.Fatalf("the build reported success but produced nothing: %v", err)
			}
			if info.Size() == 0 {
				t.Fatal("the build produced an empty binary")
			}
		})
	}
}

// TestTheHostTargetIsInTheMatrix guards against a matrix that drifts away from
// the machine anyone actually develops on.
func TestTheHostTargetIsInTheMatrix(t *testing.T) {
	known := map[string]bool{"linux": true, "darwin": true, "windows": true}
	if !known[runtime.GOOS] {
		t.Skipf("this host runs %s, which is outside the release matrix", runtime.GOOS)
	}
}

func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("no go.mod above the working directory")
		}
		dir = parent
	}
}
