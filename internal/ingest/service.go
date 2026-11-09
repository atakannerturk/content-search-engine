package ingest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/atakannerturk/content-search-engine/internal/cache"
	"github.com/atakannerturk/content-search-engine/internal/domain"
	"github.com/atakannerturk/content-search-engine/internal/provider"
	"github.com/atakannerturk/content-search-engine/internal/scoring"
)

// Scorer computes the score of an item as of a point in time.
type Scorer func(domain.Content, time.Time) domain.ScoreBreakdown

// statsReporter is implemented by providers that can report the counters of
// the HTTP client they use; the counters are attributed to runs.
type statsReporter interface {
	Stats() provider.Stats
}

// Options configure a Service. Zero values select the documented defaults.
type Options struct {
	Providers []provider.Provider
	Store     ContentWriter
	Runs      RunRecorder
	// Locker serialises runs across instances; nil means single instance.
	Locker Locker
	// Cache, when set, has its generation bumped after data changes.
	Cache cache.Cache
	// Score defaults to scoring.Score.
	Score Scorer
	// MaxPages caps the pages fetched per provider per run; default 10.
	MaxPages int
	// RunTimeout bounds one full run; default 2 minutes.
	RunTimeout time.Duration
	// DeactivateMaxRatio aborts soft deletion when the share of a provider's
	// active items that would be deactivated exceeds it; default 0.5.
	DeactivateMaxRatio float64
	// RescoreBatch is the page size of the rescoring job; default 500.
	RescoreBatch int
	// BaseContext is the lifetime of the service; asynchronous runs derive
	// from it so that shutdown cancels them. Default context.Background().
	BaseContext context.Context
	Logger      *slog.Logger
	// Now supplies the clock; default time.Now.
	Now func() time.Time
}

// Service synchronises providers into the store.
type Service struct {
	providers          []provider.Provider
	store              ContentWriter
	runs               RunRecorder
	locker             Locker
	cache              cache.Cache
	score              Scorer
	maxPages           int
	runTimeout         time.Duration
	deactivateMaxRatio float64
	rescoreBatch       int
	baseCtx            context.Context
	logger             *slog.Logger
	now                func() time.Time

	// mu serialises runs and rescoring within this process.
	mu sync.Mutex
	// activeMu guards activeBatch; it is a separate lock so that readers never
	// block behind a running synchronisation.
	activeMu    sync.Mutex
	activeBatch string
	wg          sync.WaitGroup
}

// NewService validates opts and builds a Service.
func NewService(opts Options) (*Service, error) {
	if len(opts.Providers) == 0 {
		return nil, errors.New("ingest: at least one provider is required")
	}
	if opts.Store == nil || opts.Runs == nil {
		return nil, errors.New("ingest: store and run recorder are required")
	}
	seen := make(map[string]struct{}, len(opts.Providers))
	for _, p := range opts.Providers {
		if p.Name() == "" {
			return nil, errors.New("ingest: provider with empty name")
		}
		if _, dup := seen[p.Name()]; dup {
			return nil, fmt.Errorf("ingest: duplicate provider name %q", p.Name())
		}
		seen[p.Name()] = struct{}{}
	}
	s := &Service{
		providers:          opts.Providers,
		store:              opts.Store,
		runs:               opts.Runs,
		locker:             opts.Locker,
		cache:              opts.Cache,
		score:              opts.Score,
		maxPages:           opts.MaxPages,
		runTimeout:         opts.RunTimeout,
		deactivateMaxRatio: opts.DeactivateMaxRatio,
		rescoreBatch:       opts.RescoreBatch,
		baseCtx:            opts.BaseContext,
		logger:             opts.Logger,
		now:                opts.Now,
	}
	if s.score == nil {
		s.score = scoring.Score
	}
	if s.maxPages < 1 {
		s.maxPages = 10
	}
	if s.runTimeout <= 0 {
		s.runTimeout = 2 * time.Minute
	}
	if s.deactivateMaxRatio <= 0 || s.deactivateMaxRatio > 1 {
		s.deactivateMaxRatio = 0.5
	}
	if s.rescoreBatch < 1 {
		s.rescoreBatch = 500
	}
	if s.baseCtx == nil {
		s.baseCtx = context.Background()
	}
	if s.logger == nil {
		s.logger = slog.Default()
	}
	if s.now == nil {
		s.now = time.Now
	}
	return s, nil
}

// ProviderNames lists the configured providers in registration order.
func (s *Service) ProviderNames() []string {
	names := make([]string, 0, len(s.providers))
	for _, p := range s.providers {
		names = append(names, p.Name())
	}
	return names
}

// Active reports whether a run is in progress in this process and its batch id.
func (s *Service) Active() (batchID string, running bool) {
	s.activeMu.Lock()
	defer s.activeMu.Unlock()
	return s.activeBatch, s.activeBatch != ""
}

// BatchResult is the outcome of one synchronisation of every provider.
type BatchResult struct {
	BatchID string
	Trigger domain.SyncTrigger
	Runs    []domain.SyncRun
	// Changed reports whether any item was inserted, updated, deactivated or rescored.
	Changed bool
}

// Failed reports whether any provider run did not succeed.
func (b BatchResult) Failed() bool {
	for _, r := range b.Runs {
		if r.Status != domain.SyncSucceeded {
			return true
		}
	}
	return false
}

// Run synchronises every provider and returns when the batch has finished.
// It returns domain.ErrSyncInProgress when a run is already active in this
// process or, when a Locker is configured, in another instance.
func (s *Service) Run(ctx context.Context, trigger domain.SyncTrigger) (BatchResult, error) {
	if !s.mu.TryLock() {
		return BatchResult{}, domain.ErrSyncInProgress
	}
	defer s.mu.Unlock()
	batchID := newBatchID()
	s.setActive(batchID)
	defer s.setActive("")
	return s.runLocked(ctx, batchID, trigger)
}

// Start begins a synchronisation in the background and returns its batch id.
// The run derives from the service BaseContext, not from a request, so a
// client disconnect does not abort it while shutdown does.
func (s *Service) Start(trigger domain.SyncTrigger) (string, error) {
	if !s.mu.TryLock() {
		active, _ := s.Active()
		return active, domain.ErrSyncInProgress
	}
	batchID := newBatchID()
	s.setActive(batchID)
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer s.mu.Unlock()
		defer s.setActive("")
		if _, err := s.runLocked(s.baseCtx, batchID, trigger); err != nil {
			s.logger.Error("background synchronisation failed", "batch_id", batchID, "error", err)
		}
	}()
	return batchID, nil
}

// Wait blocks until background runs started with Start have finished.
func (s *Service) Wait() { s.wg.Wait() }

func (s *Service) setActive(batchID string) {
	s.activeMu.Lock()
	defer s.activeMu.Unlock()
	s.activeBatch = batchID
}

// runLocked executes a batch; the caller holds s.mu.
func (s *Service) runLocked(ctx context.Context, batchID string, trigger domain.SyncTrigger) (BatchResult, error) {
	ctx, cancel := context.WithTimeout(ctx, s.runTimeout)
	defer cancel()

	result := BatchResult{BatchID: batchID, Trigger: trigger}
	logger := s.logger.With("batch_id", batchID, "trigger", trigger)

	if s.locker != nil {
		release, ok, err := s.locker.TryLock(ctx)
		if err != nil {
			return result, fmt.Errorf("acquire sync lock: %w", err)
		}
		if !ok {
			logger.Info("synchronisation skipped: another instance holds the lock")
			result.Runs = s.recordSkipped(ctx, batchID, trigger)
			return result, fmt.Errorf("another instance is synchronising: %w", domain.ErrSyncInProgress)
		}
		defer release()
	}

	logger.Info("synchronisation started", "providers", s.ProviderNames())
	result.Runs = make([]domain.SyncRun, len(s.providers))
	var wg sync.WaitGroup
	for i, p := range s.providers {
		wg.Add(1)
		go func(i int, p provider.Provider) {
			defer wg.Done()
			result.Runs[i] = s.syncProvider(ctx, batchID, trigger, p)
		}(i, p)
	}
	wg.Wait()

	for _, r := range result.Runs {
		if r.Inserted+r.Updated+r.Deactivated > 0 {
			result.Changed = true
		}
	}

	rescored, err := s.rescoreLocked(ctx)
	if err != nil {
		logger.Warn("rescoring after synchronisation failed", "error", err)
	}
	if rescored > 0 {
		result.Changed = true
	}
	if result.Changed {
		s.bumpCache(ctx)
	}

	logger.Info("synchronisation finished", "changed", result.Changed, "failed", result.Failed(), "rescored", rescored)
	return result, nil
}

// recordSkipped writes an audit row per provider for a batch that could not
// take the cross-instance lock.
func (s *Service) recordSkipped(ctx context.Context, batchID string, trigger domain.SyncTrigger) []domain.SyncRun {
	now := s.now().UTC()
	runs := make([]domain.SyncRun, 0, len(s.providers))
	for _, p := range s.providers {
		run := domain.SyncRun{
			BatchID: batchID, Trigger: trigger, Provider: p.Name(),
			Status: domain.SyncSkippedLocked, StartedAt: now, FinishedAt: &now,
		}
		id, err := s.runs.CreateRun(ctx, run)
		if err != nil {
			s.logger.Warn("record skipped run", "provider", p.Name(), "error", err)
		}
		run.ID = id
		runs = append(runs, run)
	}
	return runs
}

// syncProvider fetches, scores and stores the items of one provider and
// returns its audit record. It never panics on provider errors: a failing
// provider yields a failed run and leaves the other providers untouched.
func (s *Service) syncProvider(ctx context.Context, batchID string, trigger domain.SyncTrigger, p provider.Provider) domain.SyncRun {
	startedAt := s.now().UTC()
	run := domain.SyncRun{
		BatchID: batchID, Trigger: trigger, Provider: p.Name(),
		Status: domain.SyncRunning, StartedAt: startedAt,
	}
	logger := s.logger.With("batch_id", batchID, "provider", p.Name())

	id, err := s.runs.CreateRun(ctx, run)
	if err != nil {
		run.Status = domain.SyncFailed
		run.Error = fmt.Sprintf("record run: %v", err)
		logger.Error("cannot record synchronisation run", "error", err)
		return run
	}
	run.ID = id

	var statsBefore provider.Stats
	reporter, hasStats := p.(statsReporter)
	if hasStats {
		statsBefore = reporter.Stats()
	}

	items, complete := s.fetchAll(ctx, p, &run, startedAt, logger)

	if len(items) > 0 {
		res, err := s.store.UpsertBatch(ctx, items, startedAt)
		if err != nil {
			s.fail(&run, fmt.Errorf("store %d items: %w", len(items), err))
		} else {
			run.Inserted, run.Updated = res.Inserted, res.Updated
		}
	}

	if run.Status == domain.SyncRunning && complete && len(items) > 0 {
		s.deactivateUnseen(ctx, p.Name(), len(items), &run, logger)
	}

	if hasStats {
		after := reporter.Stats()
		run.Retries = int(after.Retries - statsBefore.Retries)
		run.RateLimitWaits = int(after.RateLimitWaits - statsBefore.RateLimitWaits)
	}

	if run.Status == domain.SyncRunning {
		run.Status = domain.SyncSucceeded
	}
	finishedAt := s.now().UTC()
	run.FinishedAt = &finishedAt

	// The run must be recorded even when ctx was cancelled by a shutdown.
	finishCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := s.runs.FinishRun(finishCtx, run); err != nil {
		logger.Error("cannot finish synchronisation run", "error", err)
	}
	logger.Info("provider synchronised",
		"status", run.Status, "pages", run.Pages, "fetched", run.Fetched, "inserted", run.Inserted,
		"updated", run.Updated, "skipped", run.Skipped, "deactivated", run.Deactivated,
		"retries", run.Retries, "flags", run.Flags, "error", run.Error)
	return run
}

// fetchAll walks the pages of p, scoring and de-duplicating items. complete
// is true when the provider itself signalled the end of its data, which is
// the precondition for soft-deleting items it no longer returns.
func (s *Service) fetchAll(ctx context.Context, p provider.Provider, run *domain.SyncRun, now time.Time, logger *slog.Logger) ([]domain.ScoredContent, bool) {
	byID := make(map[string]int)
	var items []domain.ScoredContent
	cursor := ""

	for page := 1; ; page++ {
		if page > s.maxPages {
			run.Flags = append(run.Flags, domain.FlagMaxPagesReached)
			logger.Warn("stopping: page cap reached", "max_pages", s.maxPages)
			return items, false
		}
		pg, err := p.Fetch(ctx, cursor)
		if err != nil {
			s.fail(run, fmt.Errorf("fetch page %d: %w", page, err))
			return items, false
		}
		run.Pages++
		run.Fetched += len(pg.Items) + len(pg.Skipped)
		run.Skipped += len(pg.Skipped)
		for _, skip := range pg.Skipped {
			if len(run.SkippedReasons) < domain.MaxSkipReasons {
				run.SkippedReasons = append(run.SkippedReasons, skip)
			}
		}

		fresh := 0
		for _, c := range pg.Items {
			if err := c.Validate(); err != nil {
				run.Skipped++
				if len(run.SkippedReasons) < domain.MaxSkipReasons {
					run.SkippedReasons = append(run.SkippedReasons, domain.SkipReason{ExternalID: c.ExternalID, Reason: err.Error()})
				}
				continue
			}
			scored := domain.ScoredContent{Content: c, Score: s.score(c, now), ScoredAt: now}
			if idx, dup := byID[c.ID]; dup {
				items[idx] = scored // later pages win over earlier duplicates
				continue
			}
			byID[c.ID] = len(items)
			items = append(items, scored)
			fresh++
		}

		switch {
		case pg.NextCursor == "":
			return items, true
		case pg.NextCursor == cursor:
			run.Flags = append(run.Flags, domain.FlagProviderIgnoresPaging)
			logger.Warn("stopping: provider returned the same cursor", "cursor", cursor)
			return items, false
		case fresh == 0:
			run.Flags = append(run.Flags, domain.FlagNoNewIDsStop)
			logger.Warn("stopping: page produced no new items", "page", page)
			return items, false
		}
		cursor = pg.NextCursor
	}
}

// deactivateUnseen soft-deletes items the provider no longer returns, unless
// that would remove an implausibly large share of its catalogue.
func (s *Service) deactivateUnseen(ctx context.Context, name string, seen int, run *domain.SyncRun, logger *slog.Logger) {
	active, err := s.store.CountActive(ctx, name)
	if err != nil {
		logger.Warn("cannot count active items; skipping deactivation", "error", err)
		return
	}
	unseen := active - seen
	if unseen <= 0 {
		return
	}
	if ratio := float64(unseen) / float64(active); ratio > s.deactivateMaxRatio {
		run.Flags = append(run.Flags, domain.FlagDeactivationSkipped)
		logger.Warn("skipping deactivation: too many items vanished at once",
			"unseen", unseen, "active", active, "max_ratio", s.deactivateMaxRatio)
		return
	}
	n, err := s.store.DeactivateUnseen(ctx, name, run.StartedAt, s.now().UTC())
	if err != nil {
		logger.Warn("deactivation failed", "error", err)
		return
	}
	run.Deactivated = n
}

// fail marks a run as failed (or interrupted when the context was cancelled).
func (s *Service) fail(run *domain.SyncRun, err error) {
	if errors.Is(err, context.Canceled) {
		run.Status = domain.SyncInterrupted
	} else {
		run.Status = domain.SyncFailed
	}
	run.Error = err.Error()
}

// Rescore refreshes the persisted score of items whose freshness bonus has
// expired. It returns the number of items updated, or
// domain.ErrSyncInProgress when a run is active in this process.
func (s *Service) Rescore(ctx context.Context) (int, error) {
	if !s.mu.TryLock() {
		return 0, domain.ErrSyncInProgress
	}
	defer s.mu.Unlock()
	n, err := s.rescoreLocked(ctx)
	if n > 0 {
		s.bumpCache(ctx)
	}
	return n, err
}

// maxRescoreBatches is a safety stop for the rescoring loop; with sane data
// every batch moves its rows out of the expired set, so this is never reached.
const maxRescoreBatches = 1000

func (s *Service) rescoreLocked(ctx context.Context) (int, error) {
	total := 0
	for i := 0; i < maxRescoreBatches; i++ {
		now := s.now().UTC()
		rows, err := s.store.ListFreshnessExpired(ctx, now, s.rescoreBatch)
		if err != nil {
			return total, fmt.Errorf("list expired items: %w", err)
		}
		if len(rows) == 0 {
			return total, nil
		}
		updates := make([]domain.ScoreUpdate, 0, len(rows))
		for _, row := range rows {
			updates = append(updates, domain.ScoreUpdate{ID: row.ID, Score: s.score(row.Content, now), ScoredAt: now})
		}
		if err := s.store.UpdateScores(ctx, updates); err != nil {
			return total, fmt.Errorf("update scores: %w", err)
		}
		total += len(updates)
		if len(rows) < s.rescoreBatch {
			return total, nil
		}
	}
	return total, errors.New("rescoring did not converge")
}

// RecoverInterrupted closes runs left in the running state by a previous
// process; it is called once at startup.
func (s *Service) RecoverInterrupted(ctx context.Context) (int, error) {
	n, err := s.runs.MarkInterrupted(ctx, s.now().UTC())
	if err != nil {
		return 0, fmt.Errorf("mark interrupted runs: %w", err)
	}
	if n > 0 {
		s.logger.Warn("marked runs left over from a previous process as interrupted", "count", n)
	}
	return n, nil
}

func (s *Service) bumpCache(ctx context.Context) {
	if s.cache == nil {
		return
	}
	bumpCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if _, err := s.cache.BumpGeneration(bumpCtx); err != nil {
		s.logger.Warn("cannot invalidate cache", "error", err)
	}
}

func newBatchID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand failing is exceptional; fall back to a time-based id.
		return fmt.Sprintf("t%x", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}
