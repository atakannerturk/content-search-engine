package domain

import "time"

// SyncTrigger records what started a synchronisation run.
type SyncTrigger string

// Known triggers.
const (
	TriggerStartup   SyncTrigger = "startup"
	TriggerScheduled SyncTrigger = "scheduled"
	TriggerManual    SyncTrigger = "manual"
)

// SyncStatus is the lifecycle state of one provider run.
type SyncStatus string

// Known statuses.
const (
	SyncRunning       SyncStatus = "running"
	SyncSucceeded     SyncStatus = "succeeded"
	SyncFailed        SyncStatus = "failed"
	SyncInterrupted   SyncStatus = "interrupted"
	SyncSkippedLocked SyncStatus = "skipped_locked"
)

// Run flags surfaced to operators; a flagged run completed but deserves a look.
const (
	FlagMaxPagesReached       = "max_pages_reached"
	FlagNoNewIDsStop          = "no_new_ids_stop"
	FlagDeactivationSkipped   = "deactivation_skipped_ratio"
	FlagProviderIgnoresPaging = "provider_ignores_pagination"
)

// SkipReason explains why one provider item was not stored.
type SkipReason struct {
	ExternalID string
	Reason     string
}

// MaxSkipReasons bounds the per-run sample of skipped items that is persisted.
const MaxSkipReasons = 20

// SyncRun is the audit record of synchronising one provider once.
// Runs started by the same trigger share a BatchID.
type SyncRun struct {
	ID         int64
	BatchID    string
	Trigger    SyncTrigger
	Provider   string
	Status     SyncStatus
	StartedAt  time.Time
	FinishedAt *time.Time

	Pages          int
	Fetched        int
	Inserted       int
	Updated        int
	Skipped        int
	Deactivated    int
	Retries        int
	RateLimitWaits int

	Error          string
	SkippedReasons []SkipReason
	Flags          []string
}
