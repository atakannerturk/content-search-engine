package postgres

import (
	"context"
	"strings"
	"testing"
)

func TestConnectRejectsInvalidDSN(t *testing.T) {
	t.Parallel()

	_, err := Connect(context.Background(), "not a dsn")
	if err == nil {
		t.Fatal("expected an error for an unparsable DSN")
	}
	if !strings.Contains(err.Error(), "parse database url") {
		t.Fatalf("error %q should say that parsing failed", err)
	}
}

func TestIntegrationPing(t *testing.T) {
	repo := newTestRepo(t)

	if err := repo.Ping(context.Background()); err != nil {
		t.Fatalf("Ping: %v", err)
	}
}
