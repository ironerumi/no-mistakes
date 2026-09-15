// Package testgit resolves the real system git binary for tests that spawn
// a fake CLI on PATH (see internal/pipeline/fakecli). It exists because
// exec.LookPath("git") returns the PATH winner, not necessarily real git: if
// a passthrough git wrapper (guard shim, audit wrapper, etc.) sits on the
// developer's PATH, that wrapper gets mistaken for "real git" and, once the
// fake CLI directory is prepended ahead of it, the two forward into each
// other without bound (github.com/ironerumi/no-mistakes#5).
package testgit

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// RealGit resolves the system git binary by absolute path, never the PATH
// winner. It tries a fixed list of well-known install locations first and
// only falls back to exec.LookPath("git") as a last resort; that fallback
// result is rejected if it resolves inside a fake-CLI temp directory
// (os.MkdirTemp("", "fakecli...")), since that can only mean a fake CLI
// shadowed itself on PATH ahead of the real tool.
func RealGit() (string, error) {
	for _, p := range []string{"/usr/bin/git", "/opt/homebrew/bin/git", "/usr/local/bin/git"} {
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
			return p, nil
		}
	}
	p, err := exec.LookPath("git")
	if err != nil {
		return "", err
	}
	if looksLikeFakeCLIPath(p) {
		return "", fmt.Errorf("testgit: PATH-resolved git %q resolves inside a fake CLI directory, refusing", p)
	}
	return p, nil
}

// looksLikeFakeCLIPath reports whether p resolves inside a fake CLI temp
// directory (os.MkdirTemp("", "fakecli...")), the only way a PATH-derived
// git fallback could actually be the fake CLI shadowing itself.
func looksLikeFakeCLIPath(p string) bool {
	return strings.Contains(p, "fakecli")
}
