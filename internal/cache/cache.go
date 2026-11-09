// Package cache defines the response cache used by the search service and
// provides an in-memory and a Redis implementation.
//
// Invalidation is generation based: every search key is prefixed with the
// current generation (see Key), and a synchronisation run bumps the generation
// once it has written new data. Old entries become unreachable immediately and
// are reclaimed by their TTL, so invalidation costs one counter increment
// regardless of how many entries exist.
package cache

import (
	"context"
	"fmt"
	"time"
)

// Cache is a byte-oriented key/value store with per-entry expiry and a
// monotonically increasing generation counter. Implementations are safe for
// concurrent use.
type Cache interface {
	// Get returns the value stored under key. The boolean is false when the key
	// is absent or expired; an error signals a backend failure.
	Get(ctx context.Context, key string) ([]byte, bool, error)
	// Set stores value under key for ttl. A non-positive ttl stores nothing.
	Set(ctx context.Context, key string, value []byte, ttl time.Duration) error
	// Delete removes key; deleting an absent key is not an error.
	Delete(ctx context.Context, key string) error
	// Generation returns the current invalidation generation.
	Generation(ctx context.Context) (uint64, error)
	// BumpGeneration increments and returns the generation.
	BumpGeneration(ctx context.Context) (uint64, error)
	// Ping reports whether the backend is reachable; used by readiness checks.
	Ping(ctx context.Context) error
	// Close releases backend resources.
	Close() error
}

// Key builds a namespaced, generation-prefixed key.
func Key(namespace string, generation uint64, suffix string) string {
	return fmt.Sprintf("%s:v%d:%s", namespace, generation, suffix)
}
