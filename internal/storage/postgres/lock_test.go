package postgres

import (
	"context"
	"testing"
)

func TestIntegrationTryLock(t *testing.T) {
	repo := newTestRepo(t)
	ctx := context.Background()

	release, ok, err := repo.TryLock(ctx)
	if err != nil || !ok || release == nil {
		t.Fatalf("first TryLock = (%v, %v, %v), want the lock", release != nil, ok, err)
	}

	// Another holder (another pool connection) must be refused while the lock is held.
	second, ok, err := repo.TryLock(ctx)
	if err != nil || ok || second != nil {
		t.Fatalf("TryLock while held = (%v, %v, %v), want (nil, false, nil)", second != nil, ok, err)
	}

	release()

	release, ok, err = repo.TryLock(ctx)
	if err != nil || !ok {
		t.Fatalf("TryLock after release = (%v, %v), want the lock", ok, err)
	}
	release()
}
