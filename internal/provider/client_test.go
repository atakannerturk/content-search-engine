package provider

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// recorder captures the delays a Client asked to sleep for, without sleeping.
type recorder struct {
	delays []time.Duration
}

func (r *recorder) sleep(_ context.Context, d time.Duration) error {
	r.delays = append(r.delays, d)
	return nil
}

func newTestClient(t *testing.T, rec *recorder, mutate func(*ClientOptions)) *Client {
	t.Helper()
	opts := ClientOptions{
		Timeout:     2 * time.Second,
		MaxRetries:  3,
		BaseBackoff: 100 * time.Millisecond,
		MaxBackoff:  time.Second,
		Sleep:       rec.sleep,
		Jitter:      func() float64 { return 0 }, // deterministic: exactly half the computed backoff
		Logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	if mutate != nil {
		mutate(&opts)
	}
	return NewClient(opts)
}

func TestGetRetriesTransientFailuresAndHonoursRetryAfter(t *testing.T) {
	t.Parallel()

	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		switch calls.Add(1) {
		case 1:
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
		case 2:
			w.WriteHeader(http.StatusInternalServerError)
		default:
			_, _ = w.Write([]byte(`{"ok":true}`))
		}
	}))
	defer srv.Close()

	rec := &recorder{}
	c := newTestClient(t, rec, nil)

	body, err := c.Get(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if string(body) != `{"ok":true}` {
		t.Fatalf("body = %q", body)
	}
	if calls.Load() != 3 {
		t.Fatalf("server calls = %d, want 3", calls.Load())
	}
	if len(rec.delays) != 2 {
		t.Fatalf("sleeps = %v, want 2 (one per retry)", rec.delays)
	}
	if rec.delays[0] != time.Second {
		t.Errorf("first retry must honour Retry-After: %v", rec.delays[0])
	}
	// Second retry: base 100ms * 2^(2-1) = 200ms, equal jitter with jitter 0 => 100ms.
	if rec.delays[1] != 100*time.Millisecond {
		t.Errorf("second retry backoff = %v, want 100ms", rec.delays[1])
	}
	stats := c.Stats()
	if stats.Requests != 3 || stats.Retries != 2 {
		t.Fatalf("stats = %+v, want 3 requests and 2 retries", stats)
	}
}

func TestGetDoesNotRetryClientErrors(t *testing.T) {
	t.Parallel()

	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		http.NotFound(w, nil)
	}))
	defer srv.Close()

	rec := &recorder{}
	c := newTestClient(t, rec, nil)

	_, err := c.Get(context.Background(), srv.URL+"/missing")
	var httpErr *HTTPError
	if !errors.As(err, &httpErr) || httpErr.StatusCode != http.StatusNotFound {
		t.Fatalf("expected HTTPError 404, got %v", err)
	}
	if calls.Load() != 1 || len(rec.delays) != 0 {
		t.Fatalf("404 must not be retried: calls=%d sleeps=%v", calls.Load(), rec.delays)
	}
}

func TestGetGivesUpAfterMaxRetries(t *testing.T) {
	t.Parallel()

	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	rec := &recorder{}
	c := newTestClient(t, rec, func(o *ClientOptions) { o.MaxRetries = 2 })

	_, err := c.Get(context.Background(), srv.URL)
	if err == nil {
		t.Fatal("expected an error")
	}
	var httpErr *HTTPError
	if !errors.As(err, &httpErr) || httpErr.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("expected the last HTTPError to be wrapped, got %v", err)
	}
	if !strings.Contains(err.Error(), "3 attempts") {
		t.Fatalf("error should mention the attempt count: %v", err)
	}
	if calls.Load() != 3 {
		t.Fatalf("calls = %d, want 3", calls.Load())
	}
}

func TestGetRefusesExcessiveRetryAfter(t *testing.T) {
	t.Parallel()

	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Retry-After", "3600")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	rec := &recorder{}
	c := newTestClient(t, rec, func(o *ClientOptions) { o.MaxRetryAfter = 5 * time.Second })

	_, err := c.Get(context.Background(), srv.URL)
	var httpErr *HTTPError
	if !errors.As(err, &httpErr) || httpErr.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("expected HTTPError 429, got %v", err)
	}
	if calls.Load() != 1 || len(rec.delays) != 0 {
		t.Fatalf("an hour-long Retry-After must not be waited for: calls=%d sleeps=%v", calls.Load(), rec.delays)
	}
}

func TestGetRetriesNetworkErrors(t *testing.T) {
	t.Parallel()

	var attempts atomic.Int32
	rt := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if attempts.Add(1) == 1 {
			return nil, errors.New("connection reset by peer")
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader("ok")),
			Header:     make(http.Header),
			Request:    req,
		}, nil
	})

	rec := &recorder{}
	c := newTestClient(t, rec, func(o *ClientOptions) { o.HTTPClient = &http.Client{Transport: rt} })

	body, err := c.Get(context.Background(), "http://provider.test/feed")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if string(body) != "ok" || attempts.Load() != 2 {
		t.Fatalf("body=%q attempts=%d", body, attempts.Load())
	}
}

func TestGetLimitsBodySize(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(strings.Repeat("x", 2048)))
	}))
	defer srv.Close()

	rec := &recorder{}
	c := newTestClient(t, rec, func(o *ClientOptions) { o.MaxBodyBytes = 1024 })

	_, err := c.Get(context.Background(), srv.URL)
	if !errors.Is(err, ErrBodyTooLarge) {
		t.Fatalf("expected ErrBodyTooLarge, got %v", err)
	}
	if len(rec.delays) != 0 {
		t.Fatalf("oversized bodies must not be retried: %v", rec.delays)
	}
}

func TestGetStopsWhenContextIsCancelled(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	c := newTestClient(t, &recorder{}, func(o *ClientOptions) {
		o.Sleep = func(ctx context.Context, _ time.Duration) error {
			cancel()
			return ctx.Err()
		}
	})

	_, err := c.Get(ctx, srv.URL)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
}

func TestRateLimiterIsConsultedBeforeEveryAttempt(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()

	rec := &recorder{}
	// One token per minute with a burst of one: the second request must wait.
	c := newTestClient(t, rec, func(o *ClientOptions) { o.RateLimit = 1.0 / 60; o.Burst = 1 })

	for i := 0; i < 2; i++ {
		if _, err := c.Get(context.Background(), srv.URL); err != nil {
			t.Fatalf("Get %d: %v", i, err)
		}
	}
	if got := c.Stats().RateLimitWaits; got != 1 {
		t.Fatalf("RateLimitWaits = %d, want 1", got)
	}
	if len(rec.delays) != 1 || rec.delays[0] <= 0 {
		t.Fatalf("expected one positive limiter delay, got %v", rec.delays)
	}
}

func TestParseRetryAfter(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	if got := parseRetryAfter("7", now); got != 7*time.Second {
		t.Errorf("seconds form = %v", got)
	}
	if got := parseRetryAfter(now.Add(90*time.Second).UTC().Format(http.TimeFormat), now); got != 90*time.Second {
		t.Errorf("http-date form = %v", got)
	}
	for _, v := range []string{"", "-5", "soon", now.Add(-time.Minute).Format(http.TimeFormat)} {
		if got := parseRetryAfter(v, now); got != 0 {
			t.Errorf("parseRetryAfter(%q) = %v, want 0", v, got)
		}
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
