package zbridge

import (
	"os"
	"path/filepath"
	"testing"
)

// The 2026-09-10 production incident: a bare `./zai-proxy --agent-mode`
// (no ZAI_TOKEN env, started directly instead of via run-freebuff-proxy.sh)
// fell into guest init, and Z.AI gates the flagship models (glm-5.3, glm-5.2,
// glm-5-turbo) behind logged-in user levels — every flagship request 403'd
// with "Model not available for current user level". loadConfig must fall
// back to the shared token file (~/.config/zai-proxy/token) so a saved token
// is honored no matter how the binary was started.

// setTokenFileForTest points tokenFilePath at a temp file via HOME override
// and writes the given content. Restores HOME afterwards. NOTE: t.TempDir()
// returns a DIFFERENT dir per call — resolve it once and reuse, or HOME and
// the written file land in two different trees.
func setTokenFileForTest(t *testing.T, content string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	if content != "" {
		dir := filepath.Join(home, ".config", "zai-proxy")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "token"), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func TestReadTokenFile(t *testing.T) {
	t.Run("present with whitespace", func(t *testing.T) {
		setTokenFileForTest(t, "  tok123 \n")
		if got := readTokenFile(); got != "tok123" {
			t.Fatalf("readTokenFile = %q, want %q", got, "tok123")
		}
	})
	t.Run("absent", func(t *testing.T) {
		setTokenFileForTest(t, "")
		if got := readTokenFile(); got != "" {
			t.Fatalf("readTokenFile = %q, want empty", got)
		}
	})
}

func TestLoadConfigFallsBackToTokenFile(t *testing.T) {
	setTokenFileForTest(t, "file-token-abc\n")
	t.Setenv("ZAI_TOKEN", "")
	c := loadConfig()
	if c.ZaiToken != "file-token-abc" {
		t.Fatalf("ZaiToken = %q, want %q (token file must be the fallback)", c.ZaiToken, "file-token-abc")
	}
}

func TestLoadConfigEnvBeatsTokenFile(t *testing.T) {
	setTokenFileForTest(t, "file-token-abc\n")
	t.Setenv("ZAI_TOKEN", "env-token-xyz")
	c := loadConfig()
	if c.ZaiToken != "env-token-xyz" {
		t.Fatalf("ZaiToken = %q, want %q (explicit env must win)", c.ZaiToken, "env-token-xyz")
	}
}

func TestLoadConfigNoTokenStaysEmpty(t *testing.T) {
	setTokenFileForTest(t, "")
	t.Setenv("ZAI_TOKEN", "")
	c := loadConfig()
	if c.ZaiToken != "" {
		t.Fatalf("ZaiToken = %q, want empty (guest init must remain reachable)", c.ZaiToken)
	}
}
