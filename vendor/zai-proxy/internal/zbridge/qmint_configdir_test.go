package zbridge

import (
	"os"
	"path/filepath"
	"testing"
)

// TestQblessFileFindsUserConfigDir pins the 2026-09-10 outage fix: when
// QBLESS_FILE points at a path that no longer exists (stale bundle dir) and
// no qbless.json sits in cwd or next to tokens.sqlite, the loader must still
// find the session in ~/.config/zai-proxy/ — where start.sh blesses to —
// instead of concluding "no session" while a healthy Q sits unused.
func TestQblessFileFindsUserConfigDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("QBLESS_FILE", filepath.Join(home, "gone-bundle", "qbless.json")) // stale path

	// Nothing in cwd (test binary's cwd): chdir into an empty temp dir to be sure.
	wd := t.TempDir()
	oldWd, _ := os.Getwd()
	if err := os.Chdir(wd); err != nil {
		t.Skip("cannot chdir")
	}
	t.Cleanup(func() { _ = os.Chdir(oldWd) })

	// No file anywhere → not found.
	if got := qblessFile(); got != "" {
		t.Fatalf("qblessFile() = %q, want empty when nothing exists", got)
	}

	// Healthy session in the user config dir → found.
	cfgDir := filepath.Join(home, ".config", "zai-proxy")
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(cfgDir, "qbless.json")
	if err := os.WriteFile(want, []byte(`{"q":"x","sk":"y"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := qblessFile(); got != want {
		t.Fatalf("qblessFile() = %q, want %q (user config dir fallback)", got, want)
	}
}
