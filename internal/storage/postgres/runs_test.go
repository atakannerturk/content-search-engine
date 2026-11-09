package postgres

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/atakannerturk/content-search-engine/internal/domain"
)

// assertRunEqual compares two runs; timestamps are compared as instants.
func assertRunEqual(t *testing.T, got, want domain.SyncRun) {
	t.Helper()
	if !got.StartedAt.Equal(want.StartedAt) || !timePtrEqual(got.FinishedAt, want.FinishedAt) {
		t.Fatalf("run timestamps = %v / %v, want %v / %v", got.StartedAt, got.FinishedAt, want.StartedAt, want.FinishedAt)
	}
	got.StartedAt, want.StartedAt = time.Time{}, time.Time{}
	got.FinishedAt, want.FinishedAt = nil, nil
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("run mismatch\n got: %+v\nwant: %+v", got, want)
	}
}

func TestIntegrationSyncRunLifecycle(t *testing.T) {
	repo := newTestRepo(t)
	ctx := context.Background()

	id, err := repo.CreateRun(ctx, domain.SyncRun{
		BatchID: "batch-1", Trigger: domain.TriggerManual, Provider: "provider1", StartedAt: fixedNow,
	})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	if id != 1 {
		t.Fatalf("first run id = %d, want 1 after RESTART IDENTITY", id)
	}

	runs, err := repo.ListRuns(ctx, 10)
	if err != nil {
		t.Fatalf("ListRuns: %v", err)
	}
	if len(runs) != 1 {
		t.Fatalf("ListRuns returned %d runs, want 1", len(runs))
	}
	assertRunEqual(t, runs[0], domain.SyncRun{
		ID: id, BatchID: "batch-1", Trigger: domain.TriggerManual, Provider: "provider1",
		Status: domain.SyncRunning, StartedAt: fixedNow,
		SkippedReasons: []domain.SkipReason{}, Flags: []string{},
	})

	finishedAt := fixedNow.Add(2 * time.Second)
	done := domain.SyncRun{
		ID: id, BatchID: "batch-1", Trigger: domain.TriggerManual, Provider: "provider1",
		Status: domain.SyncSucceeded, StartedAt: fixedNow, FinishedAt: &finishedAt,
		Pages: 2, Fetched: 8, Inserted: 5, Updated: 2, Skipped: 1, Deactivated: 3, Retries: 1, RateLimitWaits: 4,
		Error: "provider answered slowly",
		SkippedReasons: []domain.SkipReason{
			{ExternalID: "x9", Reason: "title: must not be empty"},
			{ExternalID: "x10", Reason: "type: unknown content type"},
		},
		Flags: []string{domain.FlagMaxPagesReached, domain.FlagDeactivationSkipped},
	}
	if err := repo.FinishRun(ctx, done); err != nil {
		t.Fatalf("FinishRun: %v", err)
	}
	runs, err = repo.ListRuns(ctx, 10)
	if err != nil {
		t.Fatalf("ListRuns after finish: %v", err)
	}
	if len(runs) != 1 {
		t.Fatalf("ListRuns returned %d runs, want 1", len(runs))
	}
	assertRunEqual(t, runs[0], done)
}

func TestIntegrationFinishRunUnknownID(t *testing.T) {
	repo := newTestRepo(t)

	err := repo.FinishRun(context.Background(), domain.SyncRun{ID: 42, Status: domain.SyncFailed})
	if !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("error = %v, want domain.ErrNotFound", err)
	}
}

func TestIntegrationMarkInterrupted(t *testing.T) {
	repo := newTestRepo(t)
	ctx := context.Background()

	for _, run := range []domain.SyncRun{
		{BatchID: "b1", Trigger: domain.TriggerStartup, Provider: "provider1", StartedAt: fixedNow},
		{BatchID: "b1", Trigger: domain.TriggerStartup, Provider: "provider2", StartedAt: fixedNow},
		{BatchID: "b0", Trigger: domain.TriggerScheduled, Provider: "provider1", Status: domain.SyncSucceeded, StartedAt: fixedNow.Add(-time.Hour)},
	} {
		if _, err := repo.CreateRun(ctx, run); err != nil {
			t.Fatalf("CreateRun: %v", err)
		}
	}

	finishedAt := fixedNow.Add(time.Minute)
	n, err := repo.MarkInterrupted(ctx, finishedAt)
	if err != nil {
		t.Fatalf("MarkInterrupted: %v", err)
	}
	if n != 2 {
		t.Fatalf("MarkInterrupted affected %d runs, want 2", n)
	}
	if n, err = repo.MarkInterrupted(ctx, finishedAt); err != nil || n != 0 {
		t.Fatalf("repeated MarkInterrupted = %d, %v; want 0", n, err)
	}

	runs, err := repo.ListRuns(ctx, 10)
	if err != nil {
		t.Fatalf("ListRuns: %v", err)
	}
	for _, run := range runs {
		switch run.BatchID {
		case "b1":
			if run.Status != domain.SyncInterrupted || !timePtrEqual(run.FinishedAt, &finishedAt) {
				t.Fatalf("run %d = %s / %v, want interrupted at %v", run.ID, run.Status, run.FinishedAt, finishedAt)
			}
		case "b0":
			if run.Status != domain.SyncSucceeded || run.FinishedAt != nil {
				t.Fatalf("finished run %d was modified: %s / %v", run.ID, run.Status, run.FinishedAt)
			}
		}
	}
}

func TestIntegrationListRunsNewestFirst(t *testing.T) {
	repo := newTestRepo(t)
	ctx := context.Background()

	later := fixedNow.Add(time.Hour)
	for _, startedAt := range []time.Time{fixedNow, later, later} {
		run := domain.SyncRun{BatchID: "b", Trigger: domain.TriggerScheduled, Provider: "provider1", StartedAt: startedAt}
		if _, err := repo.CreateRun(ctx, run); err != nil {
			t.Fatalf("CreateRun: %v", err)
		}
	}

	runIDs := func(runs []domain.SyncRun) []int64 {
		out := make([]int64, len(runs))
		for i, run := range runs {
			out[i] = run.ID
		}
		return out
	}
	// Runs 2 and 3 share a start time, so the newer id comes first.
	runs, err := repo.ListRuns(ctx, 10)
	if err != nil {
		t.Fatalf("ListRuns: %v", err)
	}
	if got := runIDs(runs); !slices.Equal(got, []int64{3, 2, 1}) {
		t.Fatalf("order = %v, want [3 2 1]", got)
	}
	if runs, err = repo.ListRuns(ctx, 2); err != nil || !slices.Equal(runIDs(runs), []int64{3, 2}) {
		t.Fatalf("ListRuns(2) = %v, %v; want [3 2]", runIDs(runs), err)
	}
}
