package testgit

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestRealGit_IgnoresWrapperOnPATH is a resolution-only assertion: it never
// runs the resolved binary, let alone through the fake CLI forwarding chain.
// Doing so live would recurse the test runner itself - the exact failure
// this package exists to prevent (issue #5).
func TestRealGit_IgnoresWrapperOnPATH(t *testing.T) {
	if _, err := os.Stat("/usr/bin/git"); err != nil {
		t.Skip("/usr/bin/git not present on this host")
	}

	wrapperDir := t.TempDir()
	wrapper := filepath.Join(wrapperDir, "git")
	if err := os.WriteFile(wrapper, []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", wrapperDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	// The wrapper, not real git, is now the PATH winner.
	if pathWinner, err := exec.LookPath("git"); err != nil || pathWinner != wrapper {
		t.Fatalf("test setup: PATH winner = %q, %v, want the wrapper %q", pathWinner, err, wrapper)
	}

	got, err := RealGit()
	if err != nil {
		t.Fatalf("RealGit() error = %v", err)
	}
	if got == wrapper {
		t.Fatalf("RealGit() returned the PATH-shadowing wrapper %q, want an absolute real-git path", got)
	}
	if got != "/usr/bin/git" {
		t.Fatalf("RealGit() = %q, want /usr/bin/git", got)
	}
}

func TestLooksLikeFakeCLIPath(t *testing.T) {
	fakeDir, err := os.MkdirTemp("", "fakecli")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(fakeDir) })

	if !looksLikeFakeCLIPath(filepath.Join(fakeDir, "git")) {
		t.Fatalf("looksLikeFakeCLIPath(%q) = false, want true", filepath.Join(fakeDir, "git"))
	}
	if looksLikeFakeCLIPath("/usr/bin/git") {
		t.Fatal("looksLikeFakeCLIPath(\"/usr/bin/git\") = true, want false")
	}
}

// TestNoStrayExecLookPathGit statically enforces centralization: every test
// file that needs "real git" (as opposed to a bare availability check) must
// resolve it through testgit.RealGit, not its own exec.LookPath("git") - see
// issue #5 for why a second, independently-resolved "real git" pointer can
// reintroduce the unbounded fakecli/wrapper recursion this package prevents.
// Scoped to _test.go files: production code (e.g. internal/cli/doctor.go's
// plain "is git installed" check) never resolves a path to spawn a fake CLI
// against, so it is out of scope for this issue.
func TestNoStrayExecLookPathGit(t *testing.T) {
	repoRoot := repoRootForTest(t)
	const needle = `exec.LookPath("git")`
	// This file's own doc comments and implementation legitimately mention
	// the pattern; everything else in internal/ must not.
	allow := map[string]bool{
		filepath.Join(repoRoot, "internal", "testgit", "testgit_test.go"): true,
	}
	var hits []string
	err := filepath.Walk(filepath.Join(repoRoot, "internal"), func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(path, "_test.go") {
			return err
		}
		if allow[path] {
			return nil
		}
		raw, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		if strings.Contains(string(raw), needle) {
			hits = append(hits, path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) > 0 {
		t.Fatalf("stray %s in %v; route through testgit.RealGit() instead", needle, hits)
	}
}

func repoRootForTest(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	// This file lives at <repoRoot>/internal/testgit/testgit_test.go.
	return filepath.Join(wd, "..", "..")
}
