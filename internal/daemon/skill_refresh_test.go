package daemon

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/paths"
)

// A broken user-level skill is a warning, never a reason to refuse daemon startup.
func TestDaemonStartupSkillWriteFailureIsNonFatal(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("isolated detached-daemon service test uses a Unix login-shell shim")
	}
	root, err := os.MkdirTemp("", "dskill")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	p := paths.WithRoot(root)
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	// Force a deterministic failure even when the test runs as root.
	if err := os.WriteFile(filepath.Join(home, ".claude"), []byte("not a directory"), 0o644); err != nil {
		t.Fatal(err)
	}
	shellShim := filepath.Join(t.TempDir(), "test-shell")
	if err := os.WriteFile(shellShim, []byte("#!/bin/sh\nexec env -0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SHELL", shellShim)
	t.Setenv("NM_TEST_START_DAEMON", "1")
	t.Setenv("NM_DAEMON_HELPER_PROCESS", "daemon")
	t.Setenv("NM_TEST_DAEMON_START_TIMEOUT", "10s")
	t.Setenv("NM_TEST_DAEMON_START_POLL_INTERVAL", "10ms")

	if err := startDetachedDaemon(p); err != nil {
		t.Fatalf("skill failure stopped daemon startup: %v", err)
	}
	pid, err := ReadPID(p)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { shutdownIsolatedDaemon(t, p, pid) })
	log, err := os.ReadFile(p.DaemonLog())
	if err != nil || !strings.Contains(string(log), "refresh user-level agent skill failed") {
		t.Fatalf("missing skill refresh warning in daemon log: %v\n%s", err, log)
	}
}
