// Command api runs the content search service: HTTP API, embedded dashboard
// and the background provider synchronisation.
//
// Flags:
//
//	-migrate-only  apply database migrations and exit
//	-sync-once     synchronise every provider once, print the outcome and exit
//	-healthcheck   probe the running server (used by the container health check)
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/atakannerturk/content-search-engine/api"
	"github.com/atakannerturk/content-search-engine/internal/cache"
	"github.com/atakannerturk/content-search-engine/internal/config"
	"github.com/atakannerturk/content-search-engine/internal/domain"
	"github.com/atakannerturk/content-search-engine/internal/httpapi"
	"github.com/atakannerturk/content-search-engine/internal/ingest"
	"github.com/atakannerturk/content-search-engine/internal/provider"
	"github.com/atakannerturk/content-search-engine/internal/provider/jsonfeed"
	"github.com/atakannerturk/content-search-engine/internal/provider/xmlfeed"
	"github.com/atakannerturk/content-search-engine/internal/search"
	"github.com/atakannerturk/content-search-engine/internal/storage/postgres"
	"github.com/atakannerturk/content-search-engine/web"
)

// Provider names are part of every stored identifier ("provider1:v1"), so
// they are fixed here rather than configured: renaming one would orphan its rows.
const (
	provider1Name = "provider1"
	provider2Name = "provider2"
)

const (
	shutdownTimeout = 10 * time.Second
	dbConnectTries  = 10
	dbConnectPause  = 2 * time.Second
)

func main() {
	if err := run(); err != nil {
		slog.Error("fatal", "error", err)
		os.Exit(1)
	}
}

func run() error {
	migrateOnly := flag.Bool("migrate-only", false, "apply database migrations and exit")
	syncOnce := flag.Bool("sync-once", false, "synchronise every provider once, print the outcome and exit")
	healthcheck := flag.Bool("healthcheck", false, "probe the running server and exit 0 when it is healthy")
	flag.Parse()

	cfg, err := config.Load(os.LookupEnv)
	if err != nil {
		return fmt.Errorf("load configuration: %w", err)
	}
	logger := newLogger(cfg.Log)
	slog.SetDefault(logger)

	if *healthcheck {
		return probe(cfg.HTTPAddr)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pool, err := connectDatabase(ctx, cfg.DatabaseURL, logger)
	if err != nil {
		return err
	}
	defer pool.Close()
	if err := postgres.Migrate(ctx, pool, logger); err != nil {
		return fmt.Errorf("migrate database: %w", err)
	}
	if *migrateOnly {
		logger.Info("migrations applied")
		return nil
	}
	repo := postgres.NewRepo(pool)

	store, err := newCache(cfg.Cache)
	if err != nil {
		return fmt.Errorf("cache: %w", err)
	}
	defer func() {
		if err := store.Close(); err != nil {
			logger.Warn("closing cache", "error", err)
		}
	}()

	providers := buildProviders(cfg.Providers, logger)
	syncer, err := ingest.NewService(ingest.Options{
		Providers:          providers,
		Store:              repo,
		Runs:               repo,
		Locker:             repo,
		Cache:              store,
		MaxPages:           cfg.Providers.MaxPages,
		RunTimeout:         cfg.Sync.Timeout,
		DeactivateMaxRatio: cfg.Sync.DeactivateMaxRatio,
		BaseContext:        ctx,
		Logger:             logger,
	})
	if err != nil {
		return err
	}
	if _, err := syncer.RecoverInterrupted(ctx); err != nil {
		logger.Warn("could not repair interrupted runs", "error", err)
	}

	if *syncOnce {
		return runOnce(ctx, syncer, logger)
	}

	searcher := search.NewService(repo, store, cfg.Cache.TTL, logger)
	handler := httpapi.NewHandler(httpapi.Deps{
		Search:    searcher,
		Sync:      syncer,
		Runs:      repo,
		Checks:    map[string]httpapi.Pinger{"database": repo, "cache": store},
		Dashboard: web.FS(),
		OpenAPI:   api.OpenAPI,
		SyncToken: cfg.Sync.Token,
		RateLimit: httpapi.RateLimit{RPS: cfg.API.RateLimitRPS, Burst: cfg.API.RateLimitBurst, TrustProxy: cfg.API.TrustProxy},
		Logger:    logger,
	})

	srv := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
		BaseContext:       func(net.Listener) context.Context { return ctx },
	}

	var jobs sync.WaitGroup
	if cfg.Sync.OnStart {
		jobs.Add(1)
		go func() {
			defer jobs.Done()
			if res, err := syncer.Run(ctx, domain.TriggerStartup); err != nil {
				logger.Warn("startup synchronisation did not run", "error", err)
			} else if res.Failed() {
				logger.Warn("startup synchronisation finished with failures", "batch_id", res.BatchID)
			}
		}()
	}
	jobs.Add(1)
	go func() {
		defer jobs.Done()
		syncer.RunScheduler(ctx, cfg.Sync.Interval, cfg.Sync.RescoreInterval)
	}()

	serverErr := make(chan error, 1)
	go func() {
		logger.Info("http server listening", "addr", cfg.HTTPAddr,
			"providers", syncer.ProviderNames(), "cache", cfg.Cache.Backend,
			"sync_interval", cfg.Sync.Interval, "rescore_interval", cfg.Sync.RescoreInterval)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErr <- err
		}
		close(serverErr)
	}()

	select {
	case err := <-serverErr:
		stop()
		jobs.Wait()
		return fmt.Errorf("http server: %w", err)
	case <-ctx.Done():
	}

	logger.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		logger.Warn("http shutdown", "error", err)
	}
	jobs.Wait()
	syncer.Wait()
	logger.Info("stopped")
	return nil
}

// runOnce performs a single synchronisation for the -sync-once flag and
// reports it on stdout; the exit status reflects whether every provider succeeded.
func runOnce(ctx context.Context, syncer *ingest.Service, logger *slog.Logger) error {
	res, err := syncer.Run(ctx, domain.TriggerManual)
	if err != nil {
		return fmt.Errorf("synchronise: %w", err)
	}
	for _, run := range res.Runs {
		fmt.Printf("%-10s %-10s pages=%d fetched=%d inserted=%d updated=%d skipped=%d deactivated=%d retries=%d flags=%v",
			run.Provider, run.Status, run.Pages, run.Fetched, run.Inserted, run.Updated, run.Skipped, run.Deactivated, run.Retries, run.Flags)
		if run.Error != "" {
			fmt.Printf(" error=%q", run.Error)
		}
		fmt.Println()
	}
	if res.Failed() {
		logger.Error("synchronisation finished with failures", "batch_id", res.BatchID)
		return errors.New("one or more providers failed")
	}
	return nil
}

// connectDatabase retries for a while so that `go run` right after
// `docker compose up` does not fail while PostgreSQL is still starting.
func connectDatabase(ctx context.Context, dsn string, logger *slog.Logger) (*pgxpool.Pool, error) {
	var lastErr error
	for attempt := 1; attempt <= dbConnectTries; attempt++ {
		pool, err := postgres.Connect(ctx, dsn)
		if err == nil {
			return pool, nil
		}
		lastErr = err
		logger.Warn("database not reachable yet", "attempt", attempt, "error", err)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(dbConnectPause):
		}
	}
	return nil, fmt.Errorf("connect database after %d attempts: %w", dbConnectTries, lastErr)
}

func newCache(cfg config.Cache) (cache.Cache, error) {
	switch cfg.Backend {
	case config.CacheRedis:
		return cache.NewRedis(cfg.RedisAddr)
	default:
		return cache.NewMemory(cfg.MaxEntries), nil
	}
}

// buildProviders wires the two feeds of the assignment. Adding a provider is
// one more entry here plus an adapter package implementing provider.Provider.
func buildProviders(cfg config.Providers, logger *slog.Logger) []provider.Provider {
	newClient := func() *provider.Client {
		return provider.NewClient(provider.ClientOptions{
			Timeout:      cfg.Timeout,
			RateLimit:    cfg.RateLimitRPS,
			Burst:        cfg.Burst,
			MaxRetries:   cfg.MaxRetries,
			MaxBodyBytes: cfg.MaxBodyBytes,
			Logger:       logger,
		})
	}
	return []provider.Provider{
		jsonfeed.New(provider1Name, cfg.Provider1URL, newClient(), logger),
		xmlfeed.New(provider2Name, cfg.Provider2URL, newClient(), logger),
	}
}

// probe implements the container health check against the local server.
func probe(addr string) error {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("parse HTTP_ADDR %q: %w", addr, err)
	}
	if host == "" {
		host = "127.0.0.1"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+net.JoinHostPort(host, port)+"/healthz", http.NoBody)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("healthz returned %d", resp.StatusCode)
	}
	return nil
}

func newLogger(cfg config.Log) *slog.Logger {
	var level slog.Level
	switch strings.ToLower(cfg.Level) {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	default:
		level = slog.LevelInfo
	}
	opts := &slog.HandlerOptions{Level: level}
	var handler slog.Handler
	if strings.EqualFold(cfg.Format, "json") {
		handler = slog.NewJSONHandler(os.Stdout, opts)
	} else {
		handler = slog.NewTextHandler(os.Stdout, opts)
	}
	return slog.New(handler)
}
