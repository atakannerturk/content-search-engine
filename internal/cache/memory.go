package cache

import (
	"bytes"
	"container/list"
	"context"
	"sync"
	"sync/atomic"
	"time"
)

// MemoryOption configures a Memory cache.
type MemoryOption func(*Memory)

// WithClock replaces the time source used for expiry; tests inject a manual
// clock. now must not be nil.
func WithClock(now func() time.Time) MemoryOption {
	return func(m *Memory) { m.now = now }
}

// Memory is an in-process Cache for single-instance deployments.
//
// It holds at most maxEntries entries and evicts the least recently used one
// when a new key would exceed that bound; Get counts as a use. Expiry is lazy:
// an expired entry is reclaimed when it is read or evicted, never by a
// background goroutine, so the entry bound, not the TTL, is what protects
// memory. Values are copied on the way in and out, so callers may reuse their
// buffers. Memory is safe for concurrent use.
type Memory struct {
	maxEntries int
	now        func() time.Time
	generation atomic.Uint64

	mu      sync.Mutex
	entries map[string]*list.Element
	// order lists the entries from most to least recently used.
	order *list.List
}

// memoryEntry is the payload of a list element; the key is kept so an eviction
// from the tail of the list can also remove the map index.
type memoryEntry struct {
	key       string
	value     []byte
	expiresAt time.Time
}

var _ Cache = (*Memory)(nil)

// NewMemory returns an empty cache holding at most maxEntries entries; a value
// below one is raised to one. The generation starts at one.
func NewMemory(maxEntries int, opts ...MemoryOption) *Memory {
	if maxEntries < 1 {
		maxEntries = 1
	}
	m := &Memory{
		maxEntries: maxEntries,
		now:        time.Now,
		entries:    make(map[string]*list.Element),
		order:      list.New(),
	}
	m.generation.Store(1)
	for _, opt := range opts {
		opt(m)
	}
	return m
}

// Get returns a copy of the value stored under key. An expired entry is
// removed and reported as a miss; a hit marks the entry as most recently used.
func (m *Memory) Get(_ context.Context, key string) ([]byte, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	el, ok := m.entries[key]
	if !ok {
		return nil, false, nil
	}
	e := el.Value.(*memoryEntry)
	if !e.expiresAt.After(m.now()) {
		m.remove(el)
		return nil, false, nil
	}
	m.order.MoveToFront(el)
	return bytes.Clone(e.value), true, nil
}

// Set stores a copy of value under key for ttl, replacing any existing entry.
// A non-positive ttl stores nothing and removes the existing entry, which is
// what an immediately expiring key would amount to. When the cache is full the
// least recently used entry makes room.
func (m *Memory) Set(_ context.Context, key string, value []byte, ttl time.Duration) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if el, ok := m.entries[key]; ok {
		m.remove(el)
	}
	if ttl <= 0 {
		return nil
	}
	// The size never exceeds maxEntries, so one eviction is always enough.
	if m.order.Len() >= m.maxEntries {
		m.remove(m.order.Back())
	}
	e := &memoryEntry{key: key, value: bytes.Clone(value), expiresAt: m.now().Add(ttl)}
	m.entries[key] = m.order.PushFront(e)
	return nil
}

// Delete removes key; deleting an absent key is not an error.
func (m *Memory) Delete(_ context.Context, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if el, ok := m.entries[key]; ok {
		m.remove(el)
	}
	return nil
}

// Generation returns the current invalidation generation.
func (m *Memory) Generation(context.Context) (uint64, error) {
	return m.generation.Load(), nil
}

// BumpGeneration increments and returns the generation.
func (m *Memory) BumpGeneration(context.Context) (uint64, error) {
	return m.generation.Add(1), nil
}

// Ping always succeeds: the cache lives in the process.
func (m *Memory) Ping(context.Context) error { return nil }

// Close is a no-op kept for interface parity; it may be called repeatedly.
// Memory holds nothing the garbage collector cannot reclaim.
func (m *Memory) Close() error { return nil }

// Len returns the number of stored entries, including expired ones that have
// not been reclaimed yet.
func (m *Memory) Len() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.order.Len()
}

// remove unlinks el from the recency list and the index. The caller holds mu.
func (m *Memory) remove(el *list.Element) {
	m.order.Remove(el)
	delete(m.entries, el.Value.(*memoryEntry).key)
}
