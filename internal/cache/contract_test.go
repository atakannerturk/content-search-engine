package cache_test

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/atakannerturk/content-search-engine/internal/cache"
)

// runContract exercises the behaviour every Cache implementation must share.
// newCache returns a fresh, empty cache scoped to the given (sub)test and
// arranges for its cleanup.
func runContract(t *testing.T, newCache func(t *testing.T) cache.Cache) {
	ctx := context.Background()

	t.Run("set then get round-trips the value", func(t *testing.T) {
		t.Parallel()
		values := []struct {
			name  string
			value []byte
		}{
			{"text", []byte("hello")},
			{"binary", []byte{0, 255, '\n', '\r'}},
			{"empty", []byte{}},
		}
		for _, tc := range values {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				c := newCache(t)
				if err := c.Set(ctx, "k", tc.value, time.Minute); err != nil {
					t.Fatalf("Set: %v", err)
				}
				got, ok, err := c.Get(ctx, "k")
				if err != nil {
					t.Fatalf("Get: %v", err)
				}
				if !ok {
					t.Fatal("Get reported a miss for a key that was just set")
				}
				if !bytes.Equal(got, tc.value) {
					t.Fatalf("Get = %q, want %q", got, tc.value)
				}
			})
		}
	})

	t.Run("get misses an unknown key", func(t *testing.T) {
		t.Parallel()
		c := newCache(t)
		got, ok, err := c.Get(ctx, "missing")
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if ok || got != nil {
			t.Fatalf("Get = (%q, %v), want a miss without a value", got, ok)
		}
	})

	t.Run("set replaces an existing value", func(t *testing.T) {
		t.Parallel()
		c := newCache(t)
		for _, v := range []string{"old", "new"} {
			if err := c.Set(ctx, "k", []byte(v), time.Minute); err != nil {
				t.Fatalf("Set(%q): %v", v, err)
			}
		}
		got, ok, err := c.Get(ctx, "k")
		if err != nil || !ok {
			t.Fatalf("Get = (%q, %v, %v), want a hit", got, ok, err)
		}
		if string(got) != "new" {
			t.Fatalf("Get = %q, want %q", got, "new")
		}
	})

	t.Run("stored value is isolated from caller buffers", func(t *testing.T) {
		t.Parallel()
		c := newCache(t)
		buf := []byte("original")
		if err := c.Set(ctx, "k", buf, time.Minute); err != nil {
			t.Fatalf("Set: %v", err)
		}
		buf[0] = 'X'

		first, ok, err := c.Get(ctx, "k")
		if err != nil || !ok {
			t.Fatalf("Get = (%q, %v, %v), want a hit", first, ok, err)
		}
		if string(first) != "original" {
			t.Fatalf("mutating the caller's buffer changed the cached value: %q", first)
		}
		first[0] = 'Y'

		second, ok, err := c.Get(ctx, "k")
		if err != nil || !ok {
			t.Fatalf("Get = (%q, %v, %v), want a hit", second, ok, err)
		}
		if string(second) != "original" {
			t.Fatalf("mutating a returned value changed the cached value: %q", second)
		}
	})

	t.Run("delete removes the key", func(t *testing.T) {
		t.Parallel()
		c := newCache(t)
		if err := c.Set(ctx, "k", []byte("v"), time.Minute); err != nil {
			t.Fatalf("Set: %v", err)
		}
		if err := c.Delete(ctx, "k"); err != nil {
			t.Fatalf("Delete: %v", err)
		}
		if _, ok, err := c.Get(ctx, "k"); err != nil || ok {
			t.Fatalf("Get after Delete = (ok=%v, err=%v), want a miss", ok, err)
		}
	})

	t.Run("delete of an absent key succeeds", func(t *testing.T) {
		t.Parallel()
		c := newCache(t)
		if err := c.Delete(ctx, "absent"); err != nil {
			t.Fatalf("Delete: %v", err)
		}
	})

	t.Run("non-positive ttl stores nothing and removes the existing entry", func(t *testing.T) {
		t.Parallel()
		for _, ttl := range []time.Duration{0, -time.Second} {
			t.Run(ttl.String(), func(t *testing.T) {
				t.Parallel()
				c := newCache(t)
				if err := c.Set(ctx, "fresh", []byte("v"), ttl); err != nil {
					t.Fatalf("Set(fresh): %v", err)
				}
				if _, ok, err := c.Get(ctx, "fresh"); err != nil || ok {
					t.Fatalf("Get(fresh) = (ok=%v, err=%v), want a miss", ok, err)
				}

				if err := c.Set(ctx, "existing", []byte("v"), time.Minute); err != nil {
					t.Fatalf("Set(existing): %v", err)
				}
				if err := c.Set(ctx, "existing", []byte("v"), ttl); err != nil {
					t.Fatalf("Set(existing, %v): %v", ttl, err)
				}
				if _, ok, err := c.Get(ctx, "existing"); err != nil || ok {
					t.Fatalf("Get(existing) = (ok=%v, err=%v), want the entry removed", ok, err)
				}
			})
		}
	})

	t.Run("bump generation increments by one", func(t *testing.T) {
		t.Parallel()
		c := newCache(t)
		before, err := c.Generation(ctx)
		if err != nil {
			t.Fatalf("Generation: %v", err)
		}
		bumped, err := c.BumpGeneration(ctx)
		if err != nil {
			t.Fatalf("BumpGeneration: %v", err)
		}
		if bumped != before+1 {
			t.Fatalf("BumpGeneration = %d, want %d", bumped, before+1)
		}
		after, err := c.Generation(ctx)
		if err != nil {
			t.Fatalf("Generation: %v", err)
		}
		if after != bumped {
			t.Fatalf("Generation after bump = %d, want %d", after, bumped)
		}
	})

	t.Run("ping succeeds", func(t *testing.T) {
		t.Parallel()
		c := newCache(t)
		if err := c.Ping(ctx); err != nil {
			t.Fatalf("Ping: %v", err)
		}
	})
}

func TestKey(t *testing.T) {
	t.Parallel()

	tests := []struct {
		namespace  string
		generation uint64
		suffix     string
		want       string
	}{
		{"search", 3, "abc", "search:v3:abc"},
		{"search", 0, "", "search:v0:"},
		{"item", 42, "provider1:7", "item:v42:provider1:7"},
	}
	for _, tc := range tests {
		got := cache.Key(tc.namespace, tc.generation, tc.suffix)
		if got != tc.want {
			t.Errorf("Key(%q, %d, %q) = %q, want %q", tc.namespace, tc.generation, tc.suffix, got, tc.want)
		}
	}
}
