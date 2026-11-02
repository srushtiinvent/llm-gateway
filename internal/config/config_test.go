package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func clearEnv(t *testing.T) {
	for _, k := range []string{"GEMINI_API_KEY", "GROQ_API_KEY", "OPENROUTER_API_KEY", "MOCK_PROVIDERS", "PROVIDER_ORDER",
		"GROQ_RPM", "MAX_CONCURRENT", "CACHE_TTL", "PORT", "DATABASE_URL", "ADMIN_TOKEN"} {
		t.Setenv(k, "")
	}
}

func TestLoadDotEnv(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, ".env")
	os.WriteFile(p, []byte(`
# a comment
FOO_TEST_A=plain
FOO_TEST_B="quoted value"
FOO_TEST_C='single'
export FOO_TEST_D=exported
FOO_TEST_E=value # trailing comment
FOO_TEST_KEEP=from-file
   FOO_TEST_F = spaced
`), 0o600)
	t.Setenv("FOO_TEST_KEEP", "from-env")
	LoadDotEnv(p)
	for k, want := range map[string]string{"FOO_TEST_A": "plain", "FOO_TEST_B": "quoted value", "FOO_TEST_C": "single",
		"FOO_TEST_D": "exported", "FOO_TEST_E": "value", "FOO_TEST_KEEP": "from-env", "FOO_TEST_F": "spaced"} {
		if got := os.Getenv(k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
	LoadDotEnv(filepath.Join(dir, "missing")) // a missing file is fine
}

func TestNoProvidersGivesHelpfulError(t *testing.T) {
	clearEnv(t)
	_, err := Load()
	if err == nil || !strings.Contains(err.Error(), "MOCK_PROVIDERS=true") {
		t.Fatalf("error should explain how to get started, got %v", err)
	}
}

func TestMockMode(t *testing.T) {
	clearEnv(t)
	t.Setenv("MOCK_PROVIDERS", "true")
	c, err := Load()
	if err != nil || len(c.Providers) != 3 || !c.Providers[0].Mock {
		t.Fatalf("%v %+v", err, c.Providers)
	}
}

func TestProvidersWithKeysInOrder(t *testing.T) {
	clearEnv(t)
	t.Setenv("GROQ_API_KEY", "g")
	t.Setenv("GEMINI_API_KEY", "m")
	t.Setenv("GROQ_RPM", "7")
	t.Setenv("MAX_CONCURRENT", "3")
	t.Setenv("CACHE_TTL", "90s")
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Providers) != 2 || c.Providers[0].Name != "gemini" || c.Providers[1].Name != "groq" {
		t.Fatalf("default order is gemini, groq, openrouter (skipping providers without a key): %+v", c.Providers)
	}
	if c.Providers[1].RPM != 7 || c.MaxConcurrent != 3 || c.CacheTTL != 90*time.Second {
		t.Fatalf("overrides not applied: %+v %d %v", c.Providers[1], c.MaxConcurrent, c.CacheTTL)
	}
	t.Setenv("PROVIDER_ORDER", "groq,gemini")
	c, _ = Load()
	if c.Providers[0].Name != "groq" {
		t.Fatal("PROVIDER_ORDER should change priority")
	}
	t.Setenv("PROVIDER_ORDER", "groq,chatgpt")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "chatgpt") {
		t.Fatalf("unknown provider name should be reported: %v", err)
	}
}
