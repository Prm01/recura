// Package config loads Sieve's settings from an optional JSON file and
// environment variables. Environment variables win.
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Duration accepts "30s", "168h" or a number of seconds in JSON.
type Duration struct{ time.Duration }

func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		v, err := time.ParseDuration(s)
		if err != nil {
			return err
		}
		d.Duration = v
		return nil
	}
	var n float64
	if err := json.Unmarshal(b, &n); err != nil {
		return fmt.Errorf("duration must be a string like \"30s\" or seconds: %s", b)
	}
	d.Duration = time.Duration(n * float64(time.Second))
	return nil
}

func (d Duration) MarshalJSON() ([]byte, error) { return json.Marshal(d.String()) }

// Provider configures one model backend.
type Provider struct {
	Name      string  `json:"name"`
	Type      string  `json:"type"` // openai, anthropic, gemini, mock
	BaseURL   string  `json:"base_url,omitempty"`
	APIKey    string  `json:"api_key,omitempty"`
	APIKeyEnv string  `json:"api_key_env,omitempty"` // read the key from this env var
	Model     string  `json:"model,omitempty"`
	LatencyMS int     `json:"latency_ms,omitempty"` // mock only
	JitterMS  int     `json:"jitter_ms,omitempty"`  // mock only
	FailRate  float64 `json:"fail_rate,omitempty"`  // mock only
}

// Config is the full gateway configuration.
type Config struct {
	Addr         string   `json:"addr"`
	RedisURL     string   `json:"redis_url"`
	Threshold    float64  `json:"threshold"`
	TTL          Duration `json:"ttl"`
	MaxEntries   int      `json:"max_entries"`
	SyncInterval Duration `json:"sync_interval"`
	MaxPromptLen int      `json:"max_prompt_chars"`

	RateLimit struct {
		Capacity     float64 `json:"capacity"`
		RefillPerSec float64 `json:"refill_per_sec"`
	} `json:"rate_limit"`

	Router struct {
		Timeout          Duration `json:"timeout"`
		FailureThreshold int      `json:"failure_threshold"`
		Cooldown         Duration `json:"cooldown"`
	} `json:"router"`

	Embedder struct {
		Type    string `json:"type"` // hash or openai
		BaseURL string `json:"base_url"`
		APIKey  string `json:"api_key"`
		Model   string `json:"model"`
		Dim     int    `json:"dim"`
	} `json:"embedder"`

	Providers       []Provider        `json:"providers"`
	APIKeys         map[string]string `json:"api_keys"` // client key -> tenant; empty = open
	AdminKey        string            `json:"admin_key"`
	Synonyms        map[string]string `json:"synonyms"`
	Stopwords       []string          `json:"stopwords"`
	PrivacyPatterns []string          `json:"privacy_patterns"`
	CORSOrigins     []string          `json:"cors_origins"`
}

// Default returns a configuration that runs with no setup: memory cache,
// hash embedder and a mock model.
func Default() Config {
	var c Config
	c.Addr = ":8080"
	c.Threshold = 0.85
	c.TTL = Duration{7 * 24 * time.Hour}
	c.MaxEntries = 50000
	c.SyncInterval = Duration{2 * time.Second}
	c.MaxPromptLen = 8000
	c.RateLimit.Capacity = 20
	c.RateLimit.RefillPerSec = 1
	c.Router.Timeout = Duration{60 * time.Second}
	c.Router.FailureThreshold = 3
	c.Router.Cooldown = Duration{30 * time.Second}
	c.Embedder.Type = "hash"
	c.Embedder.Dim = 512
	return c
}

// Load reads the file named by SIEVE_CONFIG (if set), then applies
// environment overrides, then fills providers from API-key variables.
func Load() (Config, error) {
	c := Default()
	if path := os.Getenv("SIEVE_CONFIG"); path != "" {
		raw, err := os.ReadFile(path)
		if err != nil {
			return c, fmt.Errorf("config: %w", err)
		}
		if err := json.Unmarshal(raw, &c); err != nil {
			return c, fmt.Errorf("config %s: %w", path, err)
		}
	}
	str := func(k string, dst *string) {
		if v, ok := os.LookupEnv(k); ok {
			*dst = v
		}
	}
	var err error
	num := func(k string, dst *float64) {
		if v, ok := os.LookupEnv(k); ok && err == nil {
			*dst, err = strconv.ParseFloat(v, 64)
			if err != nil {
				err = fmt.Errorf("%s: %w", k, err)
			}
		}
	}
	integer := func(k string, dst *int) {
		if v, ok := os.LookupEnv(k); ok && err == nil {
			*dst, err = strconv.Atoi(v)
			if err != nil {
				err = fmt.Errorf("%s: %w", k, err)
			}
		}
	}
	dur := func(k string, dst *Duration) {
		if v, ok := os.LookupEnv(k); ok && err == nil {
			dst.Duration, err = time.ParseDuration(v)
			if err != nil {
				err = fmt.Errorf("%s: %w", k, err)
			}
		}
	}
	str("SIEVE_ADDR", &c.Addr)
	if p := os.Getenv("PORT"); p != "" && os.Getenv("SIEVE_ADDR") == "" {
		c.Addr = ":" + p // Render, Railway, Fly and Cloud Run set PORT
	}
	str("SIEVE_REDIS_URL", &c.RedisURL)
	num("SIEVE_THRESHOLD", &c.Threshold)
	dur("SIEVE_TTL", &c.TTL)
	integer("SIEVE_MAX_ENTRIES", &c.MaxEntries)
	num("SIEVE_RATE_CAPACITY", &c.RateLimit.Capacity)
	num("SIEVE_RATE_REFILL_PER_SEC", &c.RateLimit.RefillPerSec)
	dur("SIEVE_PROVIDER_TIMEOUT", &c.Router.Timeout)
	str("SIEVE_ADMIN_KEY", &c.AdminKey)
	str("SIEVE_EMBEDDER", &c.Embedder.Type)
	str("SIEVE_EMBED_BASE_URL", &c.Embedder.BaseURL)
	str("SIEVE_EMBED_API_KEY", &c.Embedder.APIKey)
	str("SIEVE_EMBED_MODEL", &c.Embedder.Model)
	if v := os.Getenv("SIEVE_API_KEYS"); v != "" {
		// "key1:tenantA,key2:tenantB"
		c.APIKeys = map[string]string{}
		for _, pair := range strings.Split(v, ",") {
			k, t, ok := strings.Cut(strings.TrimSpace(pair), ":")
			if !ok || k == "" || t == "" {
				return c, fmt.Errorf("SIEVE_API_KEYS: want key:tenant pairs, got %q", pair)
			}
			c.APIKeys[k] = t
		}
	}
	if err != nil {
		return c, err
	}
	if len(c.Providers) == 0 {
		c.Providers = providersFromEnv()
	}
	for i := range c.Providers {
		p := &c.Providers[i]
		if p.APIKey == "" && p.APIKeyEnv != "" {
			p.APIKey = os.Getenv(p.APIKeyEnv)
		}
		if p.Name == "" {
			p.Name = p.Type
		}
	}
	return c, c.Validate()
}

func providersFromEnv() []Provider {
	order := strings.Split(envOr("SIEVE_PROVIDERS", "anthropic,openai,gemini"), ",")
	var out []Provider
	for _, name := range order {
		switch strings.TrimSpace(name) {
		case "anthropic":
			if k := os.Getenv("ANTHROPIC_API_KEY"); k != "" {
				out = append(out, Provider{Name: "anthropic", Type: "anthropic", APIKey: k, Model: envOr("SIEVE_ANTHROPIC_MODEL", "claude-haiku-5-5")})
			}
		case "openai":
			if k := os.Getenv("OPENAI_API_KEY"); k != "" {
				out = append(out, Provider{Name: "openai", Type: "openai", APIKey: k, BaseURL: envOr("OPENAI_BASE_URL", "https://api.openai.com/v1"), Model: envOr("SIEVE_OPENAI_MODEL", "gpt-4o-mini")})
			}
		case "gemini":
			if k := os.Getenv("GEMINI_API_KEY"); k != "" {
				out = append(out, Provider{Name: "gemini", Type: "gemini", APIKey: k, Model: envOr("SIEVE_GEMINI_MODEL", "gemini-2.0-flash")})
			}
		case "ollama":
			out = append(out, Provider{Name: "ollama", Type: "openai", BaseURL: envOr("OLLAMA_BASE_URL", "http://localhost:11434/v1"), Model: envOr("SIEVE_OLLAMA_MODEL", "llama3.2")})
		}
	}
	if len(out) == 0 {
		// Two mock tiers so fallback can be tried without any API key.
		out = append(out,
			Provider{Name: "mock-primary", Type: "mock", LatencyMS: 800, JitterMS: 600},
			Provider{Name: "mock-backup", Type: "mock", LatencyMS: 1500, JitterMS: 500})
	}
	return out
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// Validate checks values that would otherwise fail confusingly later.
func (c Config) Validate() error {
	if c.Threshold <= 0 || c.Threshold > 1 {
		return fmt.Errorf("threshold must be in (0, 1], got %v", c.Threshold)
	}
	if c.RateLimit.Capacity < 1 {
		return fmt.Errorf("rate_limit.capacity must be at least 1")
	}
	if len(c.Providers) == 0 {
		return fmt.Errorf("no providers configured")
	}
	for _, p := range c.Providers {
		switch p.Type {
		case "openai", "anthropic", "gemini", "mock":
		default:
			return fmt.Errorf("provider %q: unknown type %q", p.Name, p.Type)
		}
		if p.Type != "mock" && p.Model == "" {
			return fmt.Errorf("provider %q: model is required", p.Name)
		}
		if p.Type == "openai" && p.BaseURL == "" {
			return fmt.Errorf("provider %q: base_url is required for openai-compatible providers", p.Name)
		}
	}
	switch c.Embedder.Type {
	case "hash":
	case "openai":
		if c.Embedder.BaseURL == "" || c.Embedder.Model == "" {
			return fmt.Errorf("embedder openai needs base_url and model")
		}
	default:
		return fmt.Errorf("unknown embedder %q", c.Embedder.Type)
	}
	return nil
}
