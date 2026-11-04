package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestDefaultsRunWithMock(t *testing.T) {
	for _, k := range []string{"RECURA_CONFIG", "ANTHROPIC_API_KEY", "OPENAI_API_KEY", "GEMINI_API_KEY", "RECURA_PROVIDERS"} {
		t.Setenv(k, "")
		os.Unsetenv(k)
	}
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Providers) != 2 || c.Providers[0].Type != "mock" || c.Providers[1].Type != "mock" {
		t.Fatalf("with no keys Recura should fall back to the mock provider, got %+v", c.Providers)
	}
}

func TestFileAndEnv(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "c.json")
	os.WriteFile(path, []byte(`{"threshold":0.9,"ttl":"1h","providers":[{"name":"a","type":"anthropic","model":"m","api_key_env":"MY_KEY"}]}`), 0o600)
	t.Setenv("RECURA_CONFIG", path)
	t.Setenv("MY_KEY", "sk-test")
	t.Setenv("RECURA_THRESHOLD", "0.85")
	t.Setenv("RECURA_API_KEYS", "k1:acme,k2:globex")
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.Threshold != 0.85 || c.TTL.Duration != time.Hour || c.Providers[0].APIKey != "sk-test" || c.APIKeys["k2"] != "globex" {
		t.Fatalf("unexpected config %+v", c)
	}
}

func TestValidate(t *testing.T) {
	c := Default()
	c.Providers = []Provider{{Name: "x", Type: "nope"}}
	if c.Validate() == nil {
		t.Fatal("unknown provider type should fail validation")
	}
}
