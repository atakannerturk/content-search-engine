package cache_test

import (
	"bytes"
	"context"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/atakannerturk/content-search-engine/internal/cache"
)

func newTestMemory(t *testing.T) cache.Cache {
	t.Helper()
	m := cache.NewMemory(64)
	t.Cleanup(func() { _ = m.Close() })
	return m
}

// newMemoryWithClock returns a cache whose clock the test moves by hand. Both
// the cache and advance must be used from the test goroutine only.
func newMemoryWithClock(maxEntries int) (m *cache.Memory, advance func(time.Duration)) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	m = cache.NewMemory(maxEntries, cache.WithClock(func() time.Time { return now }))
	return m, func(d time.Duration) { now = now.Add(d) }
}

// set stores a value derived from key; has reports whether key is served.
// Memory never returns an error, so both fail the test if one appears.
func set(t *testing.T, c cache.Cache, key string, ttl time.Duration) {
	t.Helper()
	if err := c.Set(context.Background(), key, []byte("value-"+key), ttl); err != nil {
		t.Fatalf("Set(%q): %v", key, err)
	}
}

func has(t *testing.T, c cache.Cache, key string) bool {
	t.Helper()
	_, ok, err := c.Get(context.Background(), key)
	if err != nil {
		t.Fatalf("Get(%q): %v", key, err)
	}
	return ok
}

func TestMemoryContract(t *testing.T) {
	t.Parallel()
	runContract(t, newTestMemory)
}

func TestMemoryGenerationStartsAtOne(t *testing.T) {
	t.Parallel()

	gen, err := cache.NewMemory(1).Generation(context.Background())
	if err != nil {
		t.Fatalf("Generation: %v", err)
	}
	if gen != 1 {
		t.Fatalf("Generation = %d, want 1", gen)
	}
}

func TestMemoryExpiresEntriesLazily(t *testing.T) {
	t.Parallel()

	m, advance := newMemoryWithClock(8)
	set(t, m, "k", 10*time.Second)

	advance(10*time.Second - time.Millisecond)
	if !has(t, m, "k") {
		t.Fatal("entry expired before its ttl elapsed")
	}

	advance(time.Millisecond)
	if n := m.Len(); n != 1 {
		t.Fatalf("Len = %d before the expired entry is read, want 1 (expiry is lazy)", n)
	}
	if has(t, m, "k") {
		t.Fatal("entry still served at its deadline")
	}
	if n := m.Len(); n != 0 {
		t.Fatalf("Len = %d after reading an expired entry, want 0", n)
	}
}

func TestMemoryEvictsLeastRecentlyUsed(t *testing.T) {
	t.Parallel()

	m := cache.NewMemory(2)
	set(t, m, "a", time.Minute)
	set(t, m, "b", time.Minute)
	// Reading a makes b the least recently used entry.
	if !has(t, m, "a") {
		t.Fatal("a missing before any eviction")
	}
	set(t, m, "c", time.Minute)

	if has(t, m, "b") {
		t.Error("b was least recently used and should have been evicted")
	}
	if !has(t, m, "a") {
		t.Error("a was read most recently and must survive")
	}
	if !has(t, m, "c") {
		t.Error("c was just inserted and must be present")
	}
	if n := m.Len(); n != 2 {
		t.Errorf("Len = %d, want 2", n)
	}
}

func TestMemoryUpdatingAKeyDoesNotEvict(t *testing.T) {
	t.Parallel()

	m := cache.NewMemory(2)
	set(t, m, "a", time.Minute)
	set(t, m, "b", time.Minute)
	set(t, m, "a", time.Minute)

	if !has(t, m, "a") || !has(t, m, "b") {
		t.Fatal("overwriting a present key must not evict anything")
	}
	if n := m.Len(); n != 2 {
		t.Fatalf("Len = %d, want 2", n)
	}
}

func TestMemoryMaxEntriesBelowOneIsRaisedToOne(t *testing.T) {
	t.Parallel()

	for _, maxEntries := range []int{0, -3} {
		t.Run(strconv.Itoa(maxEntries), func(t *testing.T) {
			t.Parallel()
			m := cache.NewMemory(maxEntries)
			set(t, m, "a", time.Minute)
			set(t, m, "b", time.Minute)

			if has(t, m, "a") {
				t.Error("a should have been evicted by b")
			}
			if !has(t, m, "b") {
				t.Error("b must be present")
			}
			if n := m.Len(); n != 1 {
				t.Errorf("Len = %d, want 1", n)
			}
		})
	}
}

func TestMemoryLenTracksSize(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	m := cache.NewMemory(8)
	steps := []struct {
		name string
		do   func() error
		want int
	}{
		{"set a", func() error { return m.Set(ctx, "a", []byte("1"), time.Minute) }, 1},
		{"set b", func() error { return m.Set(ctx, "b", []byte("2"), time.Minute) }, 2},
		{"overwrite a", func() error { return m.Set(ctx, "a", []byte("3"), time.Minute) }, 2},
		{"delete b", func() error { return m.Delete(ctx, "b") }, 1},
		{"delete absent", func() error { return m.Delete(ctx, "zzz") }, 1},
		{"set a with zero ttl", func() error { return m.Set(ctx, "a", []byte("4"), 0) }, 0},
	}
	for _, step := range steps {
		if err := step.do(); err != nil {
			t.Fatalf("%s: %v", step.name, err)
		}
		if n := m.Len(); n != step.want {
			t.Fatalf("after %s: Len = %d, want %d", step.name, n, step.want)
		}
	}
}

func TestMemoryCloseIsIdempotent(t *testing.T) {
	t.Parallel()

	m := cache.NewMemory(1)
	for i := range 2 {
		if err := m.Close(); err != nil {
			t.Fatalf("Close #%d: %v", i+1, err)
		}
	}
}

// TestMemoryConcurrentAccess hammers one small cache from many goroutines so
// that eviction, expiry bookkeeping and lookups overlap. Values are derived
// from their key, so any hit returning a foreign value exposes corruption;
// the race detector, when available, catches unsynchronised access.
func TestMemoryConcurrentAccess(t *testing.T) {
	t.Parallel()

	const (
		goroutines = 32
		iterations = 2000
		keySpace   = 64
		maxEntries = 16
	)
	ctx := context.Background()
	m := cache.NewMemory(maxEntries)

	var wg sync.WaitGroup
	for g := range goroutines {
		wg.Go(func() {
			for i := range iterations {
				key := strconv.Itoa((g*7 + i) % keySpace)
				want := []byte("value-" + key)
				var err error
				switch i % 4 {
				case 0, 1:
					err = m.Set(ctx, key, want, time.Minute)
				case 2:
					var got []byte
					var ok bool
					got, ok, err = m.Get(ctx, key)
					if err == nil && ok && !bytes.Equal(got, want) {
						t.Errorf("goroutine %d: Get(%q) = %q, want %q", g, key, got, want)
						return
					}
				default:
					err = m.Delete(ctx, key)
				}
				if err != nil {
					t.Errorf("goroutine %d: %v", g, err)
					return
				}
			}
		})
	}
	wg.Wait()

	if n := m.Len(); n > maxEntries {
		t.Fatalf("Len = %d, exceeds the bound of %d", n, maxEntries)
	}
}
