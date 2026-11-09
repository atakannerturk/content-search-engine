package config

import (
	"strings"
	"testing"
	"time"
)

func mapLookup(m map[string]string) Lookup {
	return func(key string) (string, bool) {
		v, ok := m[key]
		return v, ok
	}
}

func TestLoadDefaults(t *testing.T) {
	t.Parallel()

	cfg, err := Load(mapLookup(nil))
	if err != nil {
		t.Fatalf("Load with no variables: %v", err)
	}
	if cfg != Default() {
		t.Fatalf("Load without variables must equal Default()\n got: %+v\nwant: %+v", cfg, Default())
	}
	if cfg.Providers.Provider1URL != DefaultProvider1URL || cfg.Providers.Provider2URL != DefaultProvider2URL {
		t.Fatalf("default provider URLs not applied: %+v", cfg.Providers)
	}
}

func TestLoadOverrides(t *testing.T) {
	t.Parallel()

	cfg, err := Load(mapLookup(map[string]string{
		"HTTP_ADDR":               ":9090",
		"PROVIDER_RATE_LIMIT_RPS": "0.5",
		"PROVIDER_BURST":          "3",
		"PROVIDER_TIMEOUT":        "3s",
		"PROVIDER_MAX_BODY_BYTES": "2048",
		"SYNC_ON_START":           "false",
		"SYNC_INTERVAL":           "0",
		"CACHE_BACKEND":           "redis",
		"REDIS_ADDR":              "redis:6379",
		"TRUST_PROXY":             "true",
		"LOG_FORMAT":              "json",
		"SYNC_TOKEN":              "  secret  ",
	}))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.HTTPAddr != ":9090" {
		t.Errorf("HTTPAddr = %q", cfg.HTTPAddr)
	}
	if cfg.Providers.RateLimitRPS != 0.5 || cfg.Providers.Burst != 3 || cfg.Providers.Timeout != 3*time.Second || cfg.Providers.MaxBodyBytes != 2048 {
		t.Errorf("provider settings not applied: %+v", cfg.Providers)
	}
	if cfg.Sync.OnStart || cfg.Sync.Interval != 0 {
		t.Errorf("sync settings not applied: %+v", cfg.Sync)
	}
	if cfg.Cache.Backend != CacheRedis || cfg.Cache.RedisAddr != "redis:6379" {
		t.Errorf("cache settings not applied: %+v", cfg.Cache)
	}
	if !cfg.API.TrustProxy || cfg.Log.Format != "json" {
		t.Errorf("api/log settings not applied: %+v %+v", cfg.API, cfg.Log)
	}
	if cfg.Sync.Token != "secret" {
		t.Errorf("Token should be trimmed, got %q", cfg.Sync.Token)
	}
}

func TestLoadEmptyValueKeepsDefault(t *testing.T) {
	t.Parallel()

	cfg, err := Load(mapLookup(map[string]string{"SYNC_INTERVAL": "", "HTTP_ADDR": "   "}))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Sync.Interval != Default().Sync.Interval || cfg.HTTPAddr != Default().HTTPAddr {
		t.Fatalf("empty values must keep defaults: %+v", cfg)
	}
}

func TestLoadReportsEveryParseError(t *testing.T) {
	t.Parallel()

	_, err := Load(mapLookup(map[string]string{
		"PROVIDER_BURST": "many",
		"SYNC_ON_START":  "yes please",
		"CACHE_TTL":      "soon",
	}))
	if err == nil {
		t.Fatal("expected an error")
	}
	for _, key := range []string{"PROVIDER_BURST", "SYNC_ON_START", "CACHE_TTL"} {
		if !strings.Contains(err.Error(), key) {
			t.Errorf("error should mention %s: %v", key, err)
		}
	}
}

func TestValidate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		mutate  func(*Config)
		wantErr string
	}{
		{"zero rps", func(c *Config) { c.Providers.RateLimitRPS = 0 }, "PROVIDER_RATE_LIMIT_RPS"},
		{"bad provider url", func(c *Config) { c.Providers.Provider2URL = "ftp://x" }, "PROVIDER2_URL"},
		{"relative provider url", func(c *Config) { c.Providers.Provider1URL = "/local/path" }, "PROVIDER1_URL"},
		{"unknown cache backend", func(c *Config) { c.Cache.Backend = "memcached" }, "CACHE_BACKEND"},
		{"redis without addr", func(c *Config) { c.Cache.Backend = CacheRedis; c.Cache.RedisAddr = "" }, "REDIS_ADDR"},
		{"ratio above one", func(c *Config) { c.Sync.DeactivateMaxRatio = 1.5 }, "DEACTIVATE_MAX_RATIO"},
		{"bad log level", func(c *Config) { c.Log.Level = "verbose" }, "LOG_LEVEL"},
		{"max pages zero", func(c *Config) { c.Providers.MaxPages = 0 }, "PROVIDER_MAX_PAGES"},
		{"api burst zero", func(c *Config) { c.API.RateLimitBurst = 0 }, "API_RATE_LIMIT_BURST"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg := Default()
			tc.mutate(&cfg)
			err := cfg.Validate()
			if err == nil {
				t.Fatalf("expected error mentioning %s", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error %q should mention %s", err, tc.wantErr)
			}
		})
	}

	t.Run("defaults are valid", func(t *testing.T) {
		t.Parallel()
		if err := Default().Validate(); err != nil {
			t.Fatalf("Default() must validate: %v", err)
		}
	})
	t.Run("zero intervals disable features", func(t *testing.T) {
		t.Parallel()
		cfg := Default()
		cfg.Sync.Interval = 0
		cfg.Sync.RescoreInterval = 0
		cfg.Cache.TTL = 0
		if err := cfg.Validate(); err != nil {
			t.Fatalf("zero intervals must be valid: %v", err)
		}
	})
}
