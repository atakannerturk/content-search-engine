// Package config loads the service configuration from environment variables.
//
// Every setting has a working default so the binary starts with nothing but a
// reachable PostgreSQL; the defaults mirror .env.example and docker-compose.yml.
package config

import (
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Mock provider endpoints supplied with the assignment.
const (
	DefaultProvider1URL = "https://raw.githubusercontent.com/WEG-Technology/mock/refs/heads/main/v2/provider1"
	DefaultProvider2URL = "https://raw.githubusercontent.com/WEG-Technology/mock/refs/heads/main/v2/provider2"
)

// Cache backends.
const (
	CacheMemory = "memory"
	CacheRedis  = "redis"
)

// Config is the fully resolved service configuration.
type Config struct {
	HTTPAddr    string
	DatabaseURL string
	Providers   Providers
	Sync        Sync
	Cache       Cache
	API         API
	Log         Log
}

// Providers configures outbound provider access.
type Providers struct {
	Provider1URL string
	Provider2URL string
	// RateLimitRPS and Burst bound outbound requests per provider (token bucket).
	RateLimitRPS float64
	Burst        int
	// Timeout applies to a single provider request.
	Timeout time.Duration
	// MaxPages caps the pages fetched from one provider in one run.
	MaxPages int
	// MaxRetries is the number of retries after the first attempt.
	MaxRetries int
	// MaxBodyBytes bounds a provider response body.
	MaxBodyBytes int64
}

// Sync configures the ingestion scheduler.
type Sync struct {
	OnStart bool
	// Interval between scheduled runs; zero disables the scheduler.
	Interval time.Duration
	// Timeout bounds one full run (all providers).
	Timeout time.Duration
	// Token, when non-empty, is required as a bearer token on POST /api/v1/sync.
	Token string
	// DeactivateMaxRatio aborts soft deletion when a run would deactivate a
	// larger share of the active rows of a provider than this (0..1).
	DeactivateMaxRatio float64
	// RescoreInterval drives the freshness rescoring job; zero disables it.
	RescoreInterval time.Duration
}

// Cache configures the search response cache.
type Cache struct {
	Backend string
	// TTL for cached search responses; zero disables caching.
	TTL        time.Duration
	MaxEntries int
	RedisAddr  string
}

// API configures inbound HTTP behaviour.
type API struct {
	RateLimitRPS   float64
	RateLimitBurst int
	// TrustProxy enables X-Forwarded-For as the client address for rate limiting.
	TrustProxy bool
}

// Log configures logging.
type Log struct {
	Level  string
	Format string
}

// Lookup resolves an environment variable; os.LookupEnv satisfies it.
type Lookup func(key string) (string, bool)

// Default returns the built-in configuration.
func Default() Config {
	return Config{
		HTTPAddr:    ":8080",
		DatabaseURL: "postgres://app:app@localhost:5432/content?sslmode=disable",
		Providers: Providers{
			Provider1URL: DefaultProvider1URL,
			Provider2URL: DefaultProvider2URL,
			RateLimitRPS: 2,
			Burst:        2,
			Timeout:      10 * time.Second,
			MaxPages:     10,
			MaxRetries:   3,
			MaxBodyBytes: 10 << 20,
		},
		Sync: Sync{
			OnStart:            true,
			Interval:           10 * time.Minute,
			Timeout:            2 * time.Minute,
			DeactivateMaxRatio: 0.5,
			RescoreInterval:    time.Minute,
		},
		Cache: Cache{
			Backend:    CacheMemory,
			TTL:        time.Minute,
			MaxEntries: 10_000,
			RedisAddr:  "localhost:6379",
		},
		API: API{
			RateLimitRPS:   20,
			RateLimitBurst: 40,
		},
		Log: Log{
			Level:  "info",
			Format: "text",
		},
	}
}

// Load resolves the configuration from lookup on top of Default and validates it.
// Variables that are unset or empty keep their default.
func Load(lookup Lookup) (Config, error) {
	cfg := Default()
	p := &parser{lookup: lookup}

	p.str("HTTP_ADDR", &cfg.HTTPAddr)
	p.str("DATABASE_URL", &cfg.DatabaseURL)

	p.str("PROVIDER1_URL", &cfg.Providers.Provider1URL)
	p.str("PROVIDER2_URL", &cfg.Providers.Provider2URL)
	p.float("PROVIDER_RATE_LIMIT_RPS", &cfg.Providers.RateLimitRPS)
	p.integer("PROVIDER_BURST", &cfg.Providers.Burst)
	p.duration("PROVIDER_TIMEOUT", &cfg.Providers.Timeout)
	p.integer("PROVIDER_MAX_PAGES", &cfg.Providers.MaxPages)
	p.integer("PROVIDER_MAX_RETRIES", &cfg.Providers.MaxRetries)
	p.int64("PROVIDER_MAX_BODY_BYTES", &cfg.Providers.MaxBodyBytes)

	p.boolean("SYNC_ON_START", &cfg.Sync.OnStart)
	p.duration("SYNC_INTERVAL", &cfg.Sync.Interval)
	p.duration("SYNC_TIMEOUT", &cfg.Sync.Timeout)
	p.str("SYNC_TOKEN", &cfg.Sync.Token)
	p.float("DEACTIVATE_MAX_RATIO", &cfg.Sync.DeactivateMaxRatio)
	p.duration("RESCORE_INTERVAL", &cfg.Sync.RescoreInterval)

	p.str("CACHE_BACKEND", &cfg.Cache.Backend)
	p.duration("CACHE_TTL", &cfg.Cache.TTL)
	p.integer("CACHE_MAX_ENTRIES", &cfg.Cache.MaxEntries)
	p.str("REDIS_ADDR", &cfg.Cache.RedisAddr)

	p.float("API_RATE_LIMIT_RPS", &cfg.API.RateLimitRPS)
	p.integer("API_RATE_LIMIT_BURST", &cfg.API.RateLimitBurst)
	p.boolean("TRUST_PROXY", &cfg.API.TrustProxy)

	p.str("LOG_LEVEL", &cfg.Log.Level)
	p.str("LOG_FORMAT", &cfg.Log.Format)

	if err := p.result(); err != nil {
		return Config{}, err
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// Validate checks the configuration for values the service cannot run with.
func (c Config) Validate() error {
	var errs []error
	fail := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }

	if strings.TrimSpace(c.HTTPAddr) == "" {
		fail("HTTP_ADDR must not be empty")
	}
	if strings.TrimSpace(c.DatabaseURL) == "" {
		fail("DATABASE_URL must not be empty")
	}
	for _, p := range []struct{ key, raw string }{
		{"PROVIDER1_URL", c.Providers.Provider1URL},
		{"PROVIDER2_URL", c.Providers.Provider2URL},
	} {
		if err := validateHTTPURL(p.raw); err != nil {
			fail("%s: %v", p.key, err)
		}
	}
	if c.Providers.RateLimitRPS <= 0 {
		fail("PROVIDER_RATE_LIMIT_RPS must be > 0")
	}
	if c.Providers.Burst < 1 {
		fail("PROVIDER_BURST must be >= 1")
	}
	if c.Providers.Timeout <= 0 {
		fail("PROVIDER_TIMEOUT must be > 0")
	}
	if c.Providers.MaxPages < 1 {
		fail("PROVIDER_MAX_PAGES must be >= 1")
	}
	if c.Providers.MaxRetries < 0 {
		fail("PROVIDER_MAX_RETRIES must be >= 0")
	}
	if c.Providers.MaxBodyBytes <= 0 {
		fail("PROVIDER_MAX_BODY_BYTES must be > 0")
	}
	if c.Sync.Interval < 0 {
		fail("SYNC_INTERVAL must be >= 0 (0 disables the scheduler)")
	}
	if c.Sync.Timeout <= 0 {
		fail("SYNC_TIMEOUT must be > 0")
	}
	if c.Sync.DeactivateMaxRatio < 0 || c.Sync.DeactivateMaxRatio > 1 {
		fail("DEACTIVATE_MAX_RATIO must be within [0, 1]")
	}
	if c.Sync.RescoreInterval < 0 {
		fail("RESCORE_INTERVAL must be >= 0 (0 disables rescoring)")
	}
	switch c.Cache.Backend {
	case CacheMemory:
	case CacheRedis:
		if strings.TrimSpace(c.Cache.RedisAddr) == "" {
			fail("REDIS_ADDR must not be empty when CACHE_BACKEND=redis")
		}
	default:
		fail("CACHE_BACKEND must be %q or %q, got %q", CacheMemory, CacheRedis, c.Cache.Backend)
	}
	if c.Cache.TTL < 0 {
		fail("CACHE_TTL must be >= 0 (0 disables caching)")
	}
	if c.Cache.MaxEntries < 1 {
		fail("CACHE_MAX_ENTRIES must be >= 1")
	}
	if c.API.RateLimitRPS <= 0 {
		fail("API_RATE_LIMIT_RPS must be > 0")
	}
	if c.API.RateLimitBurst < 1 {
		fail("API_RATE_LIMIT_BURST must be >= 1")
	}
	switch strings.ToLower(c.Log.Level) {
	case "debug", "info", "warn", "error":
	default:
		fail("LOG_LEVEL must be one of debug, info, warn, error; got %q", c.Log.Level)
	}
	switch strings.ToLower(c.Log.Format) {
	case "json", "text":
	default:
		fail("LOG_FORMAT must be json or text; got %q", c.Log.Format)
	}
	return errors.Join(errs...)
}

func validateHTTPURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("invalid URL: %w", err)
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("must be an absolute http(s) URL, got %q", raw)
	}
	return nil
}

// parser reads typed values from the lookup, collecting errors instead of
// failing fast so that one startup error lists every misconfigured variable.
type parser struct {
	lookup Lookup
	errs   []error
}

func (p *parser) raw(key string) (string, bool) {
	v, ok := p.lookup(key)
	if !ok || strings.TrimSpace(v) == "" {
		return "", false
	}
	return strings.TrimSpace(v), true
}

func (p *parser) fail(key, value string, err error) {
	p.errs = append(p.errs, fmt.Errorf("%s=%q: %w", key, value, err))
}

func (p *parser) str(key string, dst *string) {
	if v, ok := p.raw(key); ok {
		*dst = v
	}
}

func (p *parser) integer(key string, dst *int) {
	if v, ok := p.raw(key); ok {
		n, err := strconv.Atoi(v)
		if err != nil {
			p.fail(key, v, errors.New("expected an integer"))
			return
		}
		*dst = n
	}
}

func (p *parser) int64(key string, dst *int64) {
	if v, ok := p.raw(key); ok {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			p.fail(key, v, errors.New("expected an integer"))
			return
		}
		*dst = n
	}
}

func (p *parser) float(key string, dst *float64) {
	if v, ok := p.raw(key); ok {
		f, err := strconv.ParseFloat(v, 64)
		if err != nil {
			p.fail(key, v, errors.New("expected a number"))
			return
		}
		*dst = f
	}
}

func (p *parser) boolean(key string, dst *bool) {
	if v, ok := p.raw(key); ok {
		b, err := strconv.ParseBool(v)
		if err != nil {
			p.fail(key, v, errors.New("expected true or false"))
			return
		}
		*dst = b
	}
}

func (p *parser) duration(key string, dst *time.Duration) {
	if v, ok := p.raw(key); ok {
		d, err := time.ParseDuration(v)
		if err != nil {
			p.fail(key, v, errors.New("expected a duration such as 30s or 10m"))
			return
		}
		*dst = d
	}
}

func (p *parser) result() error {
	return errors.Join(p.errs...)
}
