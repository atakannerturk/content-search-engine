package cache_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/atakannerturk/content-search-engine/internal/cache"
)

// redisHarness pairs a Redis cache under test with a raw client that inspects
// the server and removes the test's keys afterwards. Every test gets its own
// prefix, so parallel tests and repeated runs never see each other's keys.
type redisHarness struct {
	cache  *cache.Redis
	raw    *redis.Client
	prefix string
}

// redisAddr returns the address of the test server, skipping the test when
// TEST_REDIS_ADDR is not set.
func redisAddr(t *testing.T) string {
	t.Helper()
	addr := os.Getenv("TEST_REDIS_ADDR")
	if addr == "" {
		t.Skip("TEST_REDIS_ADDR is not set")
	}
	return addr
}

func newRedisHarness(t *testing.T) *redisHarness {
	t.Helper()
	addr := redisAddr(t)
	prefix := fmt.Sprintf("cse-test:%s:%d", t.Name(), time.Now().UnixNano())
	c, err := cache.NewRedis(addr, cache.WithRedisPrefix(prefix))
	if err != nil {
		t.Fatalf("NewRedis: %v", err)
	}
	h := &redisHarness{cache: c, raw: redis.NewClient(&redis.Options{Addr: addr}), prefix: prefix}
	t.Cleanup(func() { h.cleanup(t) })
	return h
}

// cleanup deletes every key below the prefix and closes both clients.
func (h *redisHarness) cleanup(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	iter := h.raw.Scan(ctx, 0, h.prefix+":*", 100).Iterator()
	for iter.Next(ctx) {
		if err := h.raw.Del(ctx, iter.Val()).Err(); err != nil {
			t.Logf("cleanup: DEL %s: %v", iter.Val(), err)
		}
	}
	if err := iter.Err(); err != nil {
		t.Logf("cleanup: SCAN %s:*: %v", h.prefix, err)
	}
	if err := h.raw.Close(); err != nil {
		t.Logf("cleanup: close raw client: %v", err)
	}
	if err := h.cache.Close(); err != nil {
		t.Logf("cleanup: close cache: %v", err)
	}
}

func TestRedisContract(t *testing.T) {
	t.Parallel()
	redisAddr(t) // skip the whole suite, not each subtest, when no server is configured
	runContract(t, func(t *testing.T) cache.Cache { return newRedisHarness(t).cache })
}

func TestRedisKeyLayout(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	h := newRedisHarness(t)
	if err := h.cache.Set(ctx, "search:v1:q", []byte("v"), time.Minute); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if _, err := h.cache.BumpGeneration(ctx); err != nil {
		t.Fatalf("BumpGeneration: %v", err)
	}

	tests := []struct {
		key  string
		want string
	}{
		{h.prefix + ":search:v1:q", "v"},
		{h.prefix + ":generation", "1"},
	}
	for _, tc := range tests {
		got, err := h.raw.Get(ctx, tc.key).Result()
		if err != nil {
			t.Errorf("GET %s: %v", tc.key, err)
			continue
		}
		if got != tc.want {
			t.Errorf("GET %s = %q, want %q", tc.key, got, tc.want)
		}
	}
}

func TestRedisSetAppliesTTL(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	h := newRedisHarness(t)
	const ttl = 90 * time.Second
	if err := h.cache.Set(ctx, "k", []byte("v"), ttl); err != nil {
		t.Fatalf("Set: %v", err)
	}

	remaining, err := h.raw.TTL(ctx, h.prefix+":k").Result()
	if err != nil {
		t.Fatalf("TTL: %v", err)
	}
	if remaining <= 0 || remaining > ttl {
		t.Fatalf("TTL = %v, want within (0, %v]", remaining, ttl)
	}
}

func TestRedisGenerationStartsAtZero(t *testing.T) {
	t.Parallel()

	h := newRedisHarness(t)
	gen, err := h.cache.Generation(context.Background())
	if err != nil {
		t.Fatalf("Generation: %v", err)
	}
	if gen != 0 {
		t.Fatalf("Generation = %d, want 0 for a fresh prefix", gen)
	}
}

func TestRedisGenerationRejectsCorruptCounter(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	h := newRedisHarness(t)
	if err := h.raw.Set(ctx, h.prefix+":generation", "not-a-number", 0).Err(); err != nil {
		t.Fatalf("SET: %v", err)
	}

	_, err := h.cache.Generation(ctx)
	if err == nil {
		t.Fatal("Generation accepted a non-numeric counter")
	}
	if !strings.HasPrefix(err.Error(), "cache/redis: generation:") {
		t.Fatalf("error = %q, want the operation as prefix", err)
	}
}

// TestRedisWrapsClientErrors drives every operation against a closed client,
// which fails locally with redis.ErrClosed. That keeps the failure path
// deterministic and independent of a server, so this test never skips.
func TestRedisWrapsClientErrors(t *testing.T) {
	t.Parallel()

	client := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1"})
	if err := client.Close(); err != nil {
		t.Fatalf("closing the client double: %v", err)
	}
	r, err := cache.NewRedis("", cache.WithRedisClient(client))
	if err != nil {
		t.Fatalf("NewRedis: %v", err)
	}

	ctx := context.Background()
	tests := []struct {
		name       string
		wantPrefix string
		call       func() error
	}{
		{"get", "cache/redis: get:", func() error { _, _, err := r.Get(ctx, "k"); return err }},
		{"set", "cache/redis: set:", func() error { return r.Set(ctx, "k", []byte("v"), time.Minute) }},
		{"set with zero ttl", "cache/redis: set:", func() error { return r.Set(ctx, "k", nil, 0) }},
		{"delete", "cache/redis: delete:", func() error { return r.Delete(ctx, "k") }},
		{"generation", "cache/redis: generation:", func() error { _, err := r.Generation(ctx); return err }},
		{"bump generation", "cache/redis: bump generation:", func() error { _, err := r.BumpGeneration(ctx); return err }},
		{"ping", "cache/redis: ping:", func() error { return r.Ping(ctx) }},
		{"close", "cache/redis: close:", r.Close},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.call()
			if !errors.Is(err, redis.ErrClosed) {
				t.Fatalf("error = %v, want it to wrap redis.ErrClosed", err)
			}
			if !strings.HasPrefix(err.Error(), tc.wantPrefix) {
				t.Fatalf("error = %q, want prefix %q", err, tc.wantPrefix)
			}
		})
	}
}

func TestNewRedisValidatesArguments(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		addr    string
		opts    []cache.RedisOption
		wantErr bool
	}{
		{"address only", "localhost:6379", nil, false},
		{"empty address without client", "", nil, true},
		{"empty prefix", "localhost:6379", []cache.RedisOption{cache.WithRedisPrefix("")}, true},
		{
			"injected client without address", "",
			[]cache.RedisOption{cache.WithRedisClient(redis.NewClient(&redis.Options{Addr: "127.0.0.1:1"}))},
			false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r, err := cache.NewRedis(tc.addr, tc.opts...)
			if (err != nil) != tc.wantErr {
				t.Fatalf("NewRedis error = %v, wantErr %v", err, tc.wantErr)
			}
			if r != nil {
				t.Cleanup(func() { _ = r.Close() })
			}
		})
	}
}
