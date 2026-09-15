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
	"strings"
)

// RealGit resolves the system git binary by absolute path only, never
// consulting PATH: a wrapper (guard shim, audit wrapper, etc.) or the fake
// CLI itself can win PATH, and either one being mistaken for "real git" is
// what let fakecli and a wrapper forward into each other without bound
// (github.com/ironerumi/no-mistakes#5). Restricting resolution to a fixed
// list of well-known install locations means PATH is never consulted, so
// neither a wrapper nor the fake CLI can ever be selected.
func RealGit() (string, error) {
	locations := []string{"/usr/bin/git", "/opt/homebrew/bin/git", "/usr/local/bin/git"}
	for _, p := range locations {
		if fi, err := os.Stat(p); err == nil && fi.Mode().IsRegular() && fi.Mode()&0111 != 0 {
			return p, nil
		}
	}
	return "", fmt.Errorf("testgit: real git not found in standard locations (%s)", strings.Join(locations, ", "))
}
