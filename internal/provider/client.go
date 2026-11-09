package provider

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"math/rand/v2"
	"net/http"
	"strconv"
	"sync/atomic"
	"time"

	"golang.org/x/time/rate"
)

// ErrBodyTooLarge is returned when a provider response exceeds the configured limit.
var ErrBodyTooLarge = errors.New("response body exceeds the configured limit")

// HTTPError is returned for non-2xx provider responses.
type HTTPError struct {
	StatusCode int
	URL        string
	// RetryAfter is the delay the server asked for, when it sent one.
	RetryAfter time.Duration
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("provider returned HTTP %d for %s", e.StatusCode, e.URL)
}

// Temporary reports whether the response is worth retrying.
func (e *HTTPError) Temporary() bool {
	return e.StatusCode == http.StatusTooManyRequests || e.StatusCode >= 500
}

// Stats are cumulative counters of one Client; callers diff two snapshots to
// attribute activity to a run.
type Stats struct {
	Requests       int64
	Retries        int64
	RateLimitWaits int64
}

// ClientOptions configures a Client. Zero values select the defaults noted.
type ClientOptions struct {
	// HTTPClient performs the requests; defaults to http.DefaultClient without a
	// Timeout, because Timeout below bounds every attempt through its context.
	HTTPClient *http.Client
	// Timeout bounds one attempt (connect, headers and body); default 10s.
	Timeout time.Duration
	// RateLimit and Burst form the token bucket; RateLimit <= 0 disables it.
	RateLimit float64
	Burst     int
	// MaxRetries is the number of additional attempts after the first; default 0.
	MaxRetries int
	// MaxBodyBytes bounds a response body; default 10 MiB.
	MaxBodyBytes int64
	// BaseBackoff and MaxBackoff shape the exponential backoff; defaults 500ms and 10s.
	BaseBackoff time.Duration
	MaxBackoff  time.Duration
	// MaxRetryAfter caps a Retry-After header the client is willing to honour;
	// longer waits fail the request. Default 30s.
	MaxRetryAfter time.Duration
	// Sleep waits between attempts; tests inject a no-op. Default: a timer that
	// aborts when ctx is done.
	Sleep func(ctx context.Context, d time.Duration) error
	// Jitter returns a value in [0, 1); default math/rand/v2.
	Jitter func() float64
	// UserAgent is sent with every request.
	UserAgent string
	Logger    *slog.Logger
}

// Client is an HTTP client for provider endpoints with per-provider rate
// limiting, bounded retries with exponential backoff and body size limits. It
// is safe for concurrent use.
type Client struct {
	http          *http.Client
	limiter       *rate.Limiter
	timeout       time.Duration
	maxRetries    int
	maxBody       int64
	baseBackoff   time.Duration
	maxBackoff    time.Duration
	maxRetryAfter time.Duration
	sleep         func(ctx context.Context, d time.Duration) error
	jitter        func() float64
	userAgent     string
	logger        *slog.Logger

	requests       atomic.Int64
	retries        atomic.Int64
	rateLimitWaits atomic.Int64
}

// NewClient builds a Client from opts.
func NewClient(opts ClientOptions) *Client {
	c := &Client{
		http:          opts.HTTPClient,
		timeout:       opts.Timeout,
		maxRetries:    opts.MaxRetries,
		maxBody:       opts.MaxBodyBytes,
		baseBackoff:   opts.BaseBackoff,
		maxBackoff:    opts.MaxBackoff,
		maxRetryAfter: opts.MaxRetryAfter,
		sleep:         opts.Sleep,
		jitter:        opts.Jitter,
		userAgent:     opts.UserAgent,
		logger:        opts.Logger,
	}
	if c.http == nil {
		c.http = http.DefaultClient
	}
	if c.timeout <= 0 {
		c.timeout = 10 * time.Second
	}
	if c.maxRetries < 0 {
		c.maxRetries = 0
	}
	if c.maxBody <= 0 {
		c.maxBody = 10 << 20
	}
	if c.baseBackoff <= 0 {
		c.baseBackoff = 500 * time.Millisecond
	}
	if c.maxBackoff <= 0 {
		c.maxBackoff = 10 * time.Second
	}
	if c.maxRetryAfter <= 0 {
		c.maxRetryAfter = 30 * time.Second
	}
	if c.sleep == nil {
		c.sleep = sleepContext
	}
	if c.jitter == nil {
		c.jitter = rand.Float64
	}
	if c.userAgent == "" {
		c.userAgent = "content-search-engine/1.0"
	}
	if c.logger == nil {
		c.logger = slog.Default()
	}
	if opts.RateLimit > 0 {
		burst := opts.Burst
		if burst < 1 {
			burst = 1
		}
		c.limiter = rate.NewLimiter(rate.Limit(opts.RateLimit), burst)
	}
	return c
}

// Stats returns a snapshot of the cumulative counters.
func (c *Client) Stats() Stats {
	return Stats{
		Requests:       c.requests.Load(),
		Retries:        c.retries.Load(),
		RateLimitWaits: c.rateLimitWaits.Load(),
	}
}

// Get fetches url and returns its body. Attempts that fail with a network
// error, HTTP 429 or a 5xx status are retried up to MaxRetries times with
// exponential backoff and jitter, honouring Retry-After. Other 4xx responses
// are returned immediately as *HTTPError. The rate limiter is consulted
// before every attempt, including retries.
func (c *Client) Get(ctx context.Context, url string) ([]byte, error) {
	var lastErr error
	for attempt := 0; attempt <= c.maxRetries; attempt++ {
		if attempt > 0 {
			delay := c.backoff(attempt, lastErr)
			if delay < 0 {
				return nil, fmt.Errorf("fetch %s: %w", url, lastErr)
			}
			c.retries.Add(1)
			c.logger.Warn("retrying provider request",
				"url", url, "attempt", attempt+1, "delay", delay, "error", lastErr)
			if err := c.sleep(ctx, delay); err != nil {
				return nil, fmt.Errorf("fetch %s: %w", url, err)
			}
		}
		if err := c.wait(ctx); err != nil {
			return nil, fmt.Errorf("fetch %s: %w", url, err)
		}

		body, err := c.attempt(ctx, url)
		if err == nil {
			return body, nil
		}
		lastErr = err
		if !retryable(ctx, err) {
			return nil, fmt.Errorf("fetch %s: %w", url, err)
		}
	}
	return nil, fmt.Errorf("fetch %s: giving up after %d attempts: %w", url, c.maxRetries+1, lastErr)
}

// wait blocks until the rate limiter grants a token, counting waits that
// actually delayed the request.
func (c *Client) wait(ctx context.Context) error {
	if c.limiter == nil {
		return nil
	}
	r := c.limiter.Reserve()
	if !r.OK() {
		return errors.New("rate limiter cannot satisfy the request")
	}
	delay := r.Delay()
	if delay <= 0 {
		return nil
	}
	c.rateLimitWaits.Add(1)
	if err := c.sleep(ctx, delay); err != nil {
		r.Cancel()
		return err
	}
	return nil
}

func (c *Client) attempt(ctx context.Context, url string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, http.NoBody)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", c.userAgent)
	req.Header.Set("Accept", "application/json, application/xml, text/xml, text/plain;q=0.9, */*;q=0.8")

	c.requests.Add(1)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		// Drain a little so the connection can be reused, then report.
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return nil, &HTTPError{
			StatusCode: resp.StatusCode,
			URL:        url,
			RetryAfter: parseRetryAfter(resp.Header.Get("Retry-After"), time.Now()),
		}
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, c.maxBody+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > c.maxBody {
		return nil, ErrBodyTooLarge
	}
	return body, nil
}

// backoff returns how long to wait before the given attempt (1-based retry
// count), or a negative duration when the server demanded a wait longer than
// the client tolerates.
func (c *Client) backoff(attempt int, lastErr error) time.Duration {
	var httpErr *HTTPError
	if errors.As(lastErr, &httpErr) && httpErr.RetryAfter > 0 {
		if httpErr.RetryAfter > c.maxRetryAfter {
			return -1
		}
		return httpErr.RetryAfter
	}
	exp := float64(c.baseBackoff) * math.Pow(2, float64(attempt-1))
	d := time.Duration(math.Min(exp, float64(c.maxBackoff)))
	// Equal jitter: keep at least half the delay, randomise the rest.
	return d/2 + time.Duration(float64(d/2)*c.jitter())
}

// retryable decides whether an attempt error is transient. Context errors and
// non-temporary HTTP statuses are final; everything else (connection resets,
// timeouts, 429, 5xx) is retried.
func retryable(ctx context.Context, err error) bool {
	if ctx.Err() != nil {
		return false
	}
	if errors.Is(err, ErrBodyTooLarge) {
		return false
	}
	var httpErr *HTTPError
	if errors.As(err, &httpErr) {
		return httpErr.Temporary()
	}
	return true
}

// parseRetryAfter understands both delay-seconds and HTTP-date forms.
func parseRetryAfter(value string, now time.Time) time.Duration {
	if value == "" {
		return 0
	}
	if secs, err := strconv.Atoi(value); err == nil {
		if secs < 0 {
			return 0
		}
		return time.Duration(secs) * time.Second
	}
	if at, err := http.ParseTime(value); err == nil {
		if d := at.Sub(now); d > 0 {
			return d
		}
	}
	return 0
}

func sleepContext(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
