// Package config reads the gateway's settings from environment variables
// (optionally loaded from a .env file).
package config

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// ProviderConfig describes one AI provider the gateway can call.
type ProviderConfig struct {
	Name           string
	APIKey         string
	BaseURL        string
	Model          string
	RPM, RPD, TPM  int // free-tier limits: requests/minute, requests/day, tokens/minute (0 = unlimited)
	MaxInputTokens int // if a conversation is longer than this, old messages are dropped (0 = never)
	Mock           bool
	MockLatency    time.Duration
}

// Config holds every setting.
type Config struct {
	Port        string
	DatabaseURL string // empty = keep keys and logs in memory (lost on restart)
	AdminToken  string

	Providers     []ProviderConfig // in priority order
	MockProviders bool

	MaxConcurrent    int
	QueueMaxWaiting  int
	QueueWaitTimeout time.Duration
	AttemptTimeout   time.Duration
	MaxRetries       int
	BreakerThreshold int
	BreakerOpenFor   time.Duration

	CacheTTL        time.Duration
	CacheMaxEntries int

	DefaultKeyRPM    int
	DefaultKeyDaily  int
	LogRetentionDays int

	// Notional prices (USD per million tokens) used only to show "money saved".
	// They are NOT what you pay (free tiers cost nothing); they estimate what the same
	// traffic would cost on a paid plan.
	PriceInPerM  float64
	PriceOutPerM float64

	// Public self-serve keys (so anyone can try the site without the admin token).
	PublicSignup       bool
	PublicKeyRPM       int
	PublicKeyDaily     int
	SignupPerIPPerDay  int
	SignupGlobalPerDay int
	RequireOwnKeys     bool // visitors' keys may not use the site's own provider keys; they must bring theirs
	ByokRPM            int  // requests/minute for a key when the visitor brings their own provider keys
	TrustedProxyHops   int  // how many reverse proxies sit in front (Render = 1); used to find the real client IP

	AllowedOrigins string
	WebDist        string
}

// LoadDotEnv reads KEY=VALUE lines from a file into the process environment.
// Variables that are already set are left alone. A missing file is fine.
func LoadDotEnv(path string) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		v = strings.TrimSpace(v)
		if len(v) >= 2 && (v[0] == '"' && v[len(v)-1] == '"' || v[0] == '\'' && v[len(v)-1] == '\'') {
			v = v[1 : len(v)-1]
		} else if i := strings.Index(v, " #"); i >= 0 { // trailing comment
			v = strings.TrimSpace(v[:i])
		}
		if _, exists := os.LookupEnv(k); !exists {
			_ = os.Setenv(k, v)
		}
	}
}

func str(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

func integer(key string, def int) int {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}

func number(key string, def float64) float64 {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def
	}
	n, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return def
	}
	return n
}

func boolean(key string, def bool) bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv(key)))
	switch v {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	}
	return def
}

func duration(key string, def time.Duration) time.Duration {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return def
	}
	return d
}

// Load builds the configuration from environment variables.
func Load() (*Config, error) {
	c := &Config{
		Port:        str("PORT", "8080"),
		DatabaseURL: str("DATABASE_URL", ""),
		AdminToken:  str("ADMIN_TOKEN", ""),

		MockProviders: boolean("MOCK_PROVIDERS", false),

		MaxConcurrent:    integer("MAX_CONCURRENT", 8),
		QueueMaxWaiting:  integer("QUEUE_MAX_WAITING", 50),
		QueueWaitTimeout: duration("QUEUE_WAIT_TIMEOUT", 20*time.Second),
		AttemptTimeout:   duration("ATTEMPT_TIMEOUT", 25*time.Second),
		MaxRetries:       integer("MAX_RETRIES", 1),
		BreakerThreshold: integer("BREAKER_THRESHOLD", 3),
		BreakerOpenFor:   duration("BREAKER_OPEN_FOR", 30*time.Second),

		CacheTTL:        duration("CACHE_TTL", time.Hour),
		CacheMaxEntries: integer("CACHE_MAX_ENTRIES", 1000),

		DefaultKeyRPM:    integer("DEFAULT_KEY_RPM", 20),
		DefaultKeyDaily:  integer("DEFAULT_KEY_DAILY", 500),
		LogRetentionDays: integer("LOG_RETENTION_DAYS", 14),

		PriceInPerM:  number("PRICE_IN_PER_M", 0.15),
		PriceOutPerM: number("PRICE_OUT_PER_M", 0.60),

		PublicSignup:       boolean("PUBLIC_SIGNUP", true),
		PublicKeyRPM:       integer("PUBLIC_KEY_RPM", 6),
		PublicKeyDaily:     integer("PUBLIC_KEY_DAILY", 20),
		SignupPerIPPerDay:  integer("SIGNUP_PER_IP_PER_DAY", 3),
		SignupGlobalPerDay: integer("SIGNUP_GLOBAL_PER_DAY", 200),
		RequireOwnKeys:     boolean("REQUIRE_OWN_KEYS", false),
		ByokRPM:            integer("BYOK_RPM", 30),
		TrustedProxyHops:   integer("TRUSTED_PROXY_HOPS", 1),

		AllowedOrigins: str("ALLOWED_ORIGINS", "*"),
		WebDist:        str("WEB_DIST", ""),
	}

	if c.MockProviders {
		// Three pretend providers with tiny limits so every feature can be seen without any API key.
		c.Providers = []ProviderConfig{
			{Name: "mock-gemini", Model: "mock-model-a", Mock: true, MockLatency: 400 * time.Millisecond, RPM: 5, RPD: 100, TPM: 0},
			{Name: "mock-groq", Model: "mock-model-b", Mock: true, MockLatency: 150 * time.Millisecond, RPM: 10, RPD: 200, TPM: 0},
			{Name: "mock-openrouter", Model: "mock-model-c", Mock: true, MockLatency: 700 * time.Millisecond, RPM: 20, RPD: 50, TPM: 0},
		}
		// For load tests: MOCK_RPM / MOCK_RPD (0 = unlimited) and MOCK_LATENCY_MS apply to all three fake providers.
		for i := range c.Providers {
			if _, ok := os.LookupEnv("MOCK_RPM"); ok {
				c.Providers[i].RPM = integer("MOCK_RPM", c.Providers[i].RPM)
			}
			if _, ok := os.LookupEnv("MOCK_RPD"); ok {
				c.Providers[i].RPD = integer("MOCK_RPD", c.Providers[i].RPD)
			}
			if v := integer("MOCK_LATENCY_MS", -1); v >= 0 {
				c.Providers[i].MockLatency = time.Duration(v) * time.Millisecond
			}
		}
		return c, nil
	}

	// NOTE: these limits and model names are starting points. Free-tier rules change
	// often, so check each provider's current limits and edit your .env accordingly.
	known := map[string]ProviderConfig{
		"gemini": {
			Name: "gemini", APIKey: str("GEMINI_API_KEY", ""),
			BaseURL: str("GEMINI_BASE_URL", "https://generativelanguage.googleapis.com/v1beta/openai"),
			Model:   str("GEMINI_MODEL", "gemini-3.1-flash-lite"),
			RPM:     integer("GEMINI_RPM", 10), RPD: integer("GEMINI_RPD", 250), TPM: integer("GEMINI_TPM", 250000),
			MaxInputTokens: integer("GEMINI_MAX_INPUT_TOKENS", 100000),
		},
		"groq": {
			Name: "groq", APIKey: str("GROQ_API_KEY", ""),
			BaseURL: str("GROQ_BASE_URL", "https://api.groq.com/openai/v1"),
			Model:   str("GROQ_MODEL", "openai/gpt-oss-20b"),
			RPM:     integer("GROQ_RPM", 30), RPD: integer("GROQ_RPD", 1000), TPM: integer("GROQ_TPM", 12000),
			MaxInputTokens: integer("GROQ_MAX_INPUT_TOKENS", 6000),
		},
		"openrouter": {
			Name: "openrouter", APIKey: str("OPENROUTER_API_KEY", ""),
			BaseURL: str("OPENROUTER_BASE_URL", "https://openrouter.ai/api/v1"),
			Model:   str("OPENROUTER_MODEL", "meta-llama/llama-3.3-70b-instruct:free"),
			RPM:     integer("OPENROUTER_RPM", 20), RPD: integer("OPENROUTER_RPD", 50), TPM: integer("OPENROUTER_TPM", 0),
			MaxInputTokens: integer("OPENROUTER_MAX_INPUT_TOKENS", 8000),
		},
	}

	order := strings.Split(str("PROVIDER_ORDER", "gemini,groq,openrouter"), ",")
	for _, name := range order {
		name = strings.ToLower(strings.TrimSpace(name))
		p, ok := known[name]
		if !ok {
			return nil, fmt.Errorf("PROVIDER_ORDER contains unknown provider %q (use gemini, groq, openrouter)", name)
		}
		if p.APIKey == "" {
			continue // no key = provider is simply not used
		}
		c.Providers = append(c.Providers, p)
	}
	if len(c.Providers) == 0 {
		return nil, fmt.Errorf("no providers configured: add at least one API key (GEMINI_API_KEY, GROQ_API_KEY or OPENROUTER_API_KEY) to .env, or set MOCK_PROVIDERS=true to try the gateway without any keys")
	}
	return c, nil
}