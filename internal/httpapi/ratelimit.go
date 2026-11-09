package httpapi

import (
	"math"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// RateLimit configures the per-client inbound limiter.
type RateLimit struct {
	// RPS is the sustained request rate per client; <= 0 disables limiting.
	RPS float64
	// Burst is the number of requests a client may make at once.
	Burst int
	// TrustProxy makes the first X-Forwarded-For address the client identity.
	// Leave it off unless a trusted reverse proxy sets the header, otherwise
	// clients can choose their own bucket.
	TrustProxy bool
}

const (
	// limiterIdleTTL is how long an idle client keeps its bucket.
	limiterIdleTTL = 3 * time.Minute
	// limiterSweepEvery bounds the bookkeeping cost: expired buckets are
	// evicted once per this many lookups.
	limiterSweepEvery = 256
)

type clientBucket struct {
	limiter  *rate.Limiter
	lastSeen time.Time
}

// ipLimiter is a token bucket per client address with idle eviction. It is
// safe for concurrent use.
type ipLimiter struct {
	cfg     RateLimit
	now     func() time.Time
	mu      sync.Mutex
	buckets map[string]*clientBucket
	lookups int
}

func newIPLimiter(cfg RateLimit, now func() time.Time) *ipLimiter {
	if cfg.Burst < 1 {
		cfg.Burst = 1
	}
	if now == nil {
		now = time.Now
	}
	return &ipLimiter{cfg: cfg, now: now, buckets: make(map[string]*clientBucket)}
}

// enabled reports whether limiting is active.
func (l *ipLimiter) enabled() bool { return l != nil && l.cfg.RPS > 0 }

// reserve admits the request immediately (ok) or returns how long the client
// should wait before retrying.
func (l *ipLimiter) reserve(key string) (retryAfter time.Duration, ok bool) {
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()

	l.lookups++
	if l.lookups%limiterSweepEvery == 0 {
		l.sweep(now)
	}
	b, found := l.buckets[key]
	if !found {
		b = &clientBucket{limiter: rate.NewLimiter(rate.Limit(l.cfg.RPS), l.cfg.Burst)}
		l.buckets[key] = b
	}
	b.lastSeen = now

	res := b.limiter.ReserveN(now, 1)
	if !res.OK() {
		return time.Second, false
	}
	if delay := res.DelayFrom(now); delay > 0 {
		res.CancelAt(now)
		return delay, false
	}
	return 0, true
}

// sweep drops buckets idle for longer than limiterIdleTTL; caller holds mu.
func (l *ipLimiter) sweep(now time.Time) {
	for key, b := range l.buckets {
		if now.Sub(b.lastSeen) > limiterIdleTTL {
			delete(l.buckets, key)
		}
	}
}

// size returns the number of tracked clients (for tests).
func (l *ipLimiter) size() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.buckets)
}

// middleware rejects requests above the configured rate with a 429 problem
// and a Retry-After header in whole seconds.
func (l *ipLimiter) middleware(next http.Handler) http.Handler {
	if !l.enabled() {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		retryAfter, ok := l.reserve(clientIP(r, l.cfg.TrustProxy))
		if !ok {
			seconds := int(math.Ceil(retryAfter.Seconds()))
			if seconds < 1 {
				seconds = 1
			}
			w.Header().Set("Retry-After", strconv.Itoa(seconds))
			writeProblem(w, newProblem(r, http.StatusTooManyRequests, "rate limit exceeded, retry later"))
			return
		}
		next.ServeHTTP(w, r)
	})
}

// clientIP identifies the caller. Without a trusted proxy the TCP peer
// address is used; with one, the first X-Forwarded-For entry.
func clientIP(r *http.Request, trustProxy bool) string {
	if trustProxy {
		if fwd := r.Header.Get("X-Forwarded-For"); fwd != "" {
			first, _, _ := strings.Cut(fwd, ",")
			if ip := strings.TrimSpace(first); ip != "" {
				return ip
			}
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
