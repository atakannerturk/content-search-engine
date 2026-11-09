package ingest

import (
	"context"
	"errors"
	"time"

	"github.com/atakannerturk/content-search-engine/internal/domain"
)

// RunScheduler triggers synchronisations every syncInterval and rescoring
// every rescoreInterval until ctx is done. A non-positive interval disables
// the corresponding job. Ticks that coincide with an active run are skipped.
func (s *Service) RunScheduler(ctx context.Context, syncInterval, rescoreInterval time.Duration) {
	syncC := tickerChan(ctx, syncInterval)
	rescoreC := tickerChan(ctx, rescoreInterval)

	for {
		select {
		case <-ctx.Done():
			return
		case <-syncC:
			res, err := s.Run(ctx, domain.TriggerScheduled)
			switch {
			case errors.Is(err, domain.ErrSyncInProgress):
				s.logger.Debug("scheduled synchronisation skipped: run in progress")
			case err != nil:
				s.logger.Error("scheduled synchronisation failed", "error", err)
			case res.Failed():
				s.logger.Warn("scheduled synchronisation finished with failures", "batch_id", res.BatchID)
			}
		case <-rescoreC:
			n, err := s.Rescore(ctx)
			switch {
			case errors.Is(err, domain.ErrSyncInProgress):
				s.logger.Debug("rescoring skipped: run in progress")
			case err != nil:
				s.logger.Error("rescoring failed", "error", err)
			case n > 0:
				s.logger.Info("rescored items with expired freshness", "count", n)
			}
		}
	}
}

// tickerChan returns a channel that fires every d, or a channel that never
// fires when d is not positive. The ticker stops when ctx is done.
func tickerChan(ctx context.Context, d time.Duration) <-chan time.Time {
	if d <= 0 {
		return nil
	}
	t := time.NewTicker(d)
	go func() {
		<-ctx.Done()
		t.Stop()
	}()
	return t.C
}
