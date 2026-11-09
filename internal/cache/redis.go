package cache

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// DefaultRedisPrefix namespaces the keys this service writes to Redis.
const DefaultRedisPrefix = "cse"

// generationKey is the key, below the prefix, that holds the generation counter.
const generationKey = "generation"

// RedisOption configures a Redis cache.
type RedisOption func(*Redis)

// WithRedisPrefix sets the namespace prepended to every key, so that several
// services can share one server. The default is DefaultRedisPrefix.
func WithRedisPrefix(prefix string) RedisOption {
	return func(r *Redis) { r.prefix = prefix }
}

// WithRedisClient uses client instead of dialling an address; the cache takes
// ownership and closes it on Close. Tests inject a client or a failing double.
func WithRedisClient(client redis.UniversalClient) RedisOption {
	return func(r *Redis) { r.client = client }
}

// Redis is a Cache backed by a Redis server, for deployments that run several
// instances of the service and need a shared cache and generation counter.
// Keys are stored as "<prefix>:<key>" and expiry is delegated to the server.
type Redis struct {
	client redis.UniversalClient
	prefix string
}

var _ Cache = (*Redis)(nil)

// NewRedis returns a cache talking to the server at addr ("host:port").
// Connections are opened lazily, so an unreachable server surfaces from Ping
// or the first command rather than from here. addr may be empty when a client
// is injected with WithRedisClient.
func NewRedis(addr string, opts ...RedisOption) (*Redis, error) {
	r := &Redis{prefix: DefaultRedisPrefix}
	for _, opt := range opts {
		opt(r)
	}
	if r.prefix == "" {
		return nil, errors.New("cache/redis: prefix must not be empty")
	}
	if r.client == nil {
		if addr == "" {
			return nil, errors.New("cache/redis: address must not be empty")
		}
		r.client = redis.NewClient(&redis.Options{Addr: addr})
	}
	return r, nil
}

// Get returns the value stored under key; the boolean is false when the key is
// absent or has expired.
func (r *Redis) Get(ctx context.Context, key string) ([]byte, bool, error) {
	value, err := r.client.Get(ctx, r.key(key)).Bytes()
	if errors.Is(err, redis.Nil) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("cache/redis: get: %w", err)
	}
	return value, true, nil
}

// Set stores value under key with ttl as its expiration. A non-positive ttl
// deletes the key instead, so a caller disabling caching never leaves a stale
// entry behind.
func (r *Redis) Set(ctx context.Context, key string, value []byte, ttl time.Duration) error {
	var err error
	if ttl <= 0 {
		err = r.del(ctx, key)
	} else {
		err = r.client.Set(ctx, r.key(key), value, ttl).Err()
	}
	if err != nil {
		return fmt.Errorf("cache/redis: set: %w", err)
	}
	return nil
}

// Delete removes key; deleting an absent key is not an error.
func (r *Redis) Delete(ctx context.Context, key string) error {
	if err := r.del(ctx, key); err != nil {
		return fmt.Errorf("cache/redis: delete: %w", err)
	}
	return nil
}

// Generation returns the shared generation counter; a counter that was never
// bumped reads as zero.
func (r *Redis) Generation(ctx context.Context) (uint64, error) {
	gen, err := r.client.Get(ctx, r.key(generationKey)).Uint64()
	if errors.Is(err, redis.Nil) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("cache/redis: generation: %w", err)
	}
	return gen, nil
}

// BumpGeneration atomically increments the shared counter and returns the new
// value. INCR creates a missing counter at one, so the first bump after a
// fresh start yields one.
func (r *Redis) BumpGeneration(ctx context.Context) (uint64, error) {
	gen, err := r.client.Incr(ctx, r.key(generationKey)).Result()
	if err != nil {
		return 0, fmt.Errorf("cache/redis: bump generation: %w", err)
	}
	return uint64(gen), nil
}

// Ping checks that the server answers.
func (r *Redis) Ping(ctx context.Context) error {
	if err := r.client.Ping(ctx).Err(); err != nil {
		return fmt.Errorf("cache/redis: ping: %w", err)
	}
	return nil
}

// Close releases the connection pool; the cache must not be used afterwards.
func (r *Redis) Close() error {
	if err := r.client.Close(); err != nil {
		return fmt.Errorf("cache/redis: close: %w", err)
	}
	return nil
}

// del removes key and returns the raw client error, so each caller can wrap
// it with the name of the operation the user invoked.
func (r *Redis) del(ctx context.Context, key string) error {
	return r.client.Del(ctx, r.key(key)).Err()
}

// key namespaces a caller key.
func (r *Redis) key(key string) string {
	return r.prefix + ":" + key
}
