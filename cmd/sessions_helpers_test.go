package main

import (
	"context"
	"testing"

	"github.com/rs/zerolog"

	"github.com/marco-rozz/ready/internal/persistence"
	"github.com/marco-rozz/ready/internal/session"
	"github.com/marco-rozz/ready/internal/usage"
)

// testSessions wires a Session manager over dir (a fresh process view: a
// second call on the same dir behaves like a daemon restart).
func testSessions(t *testing.T, dir string) (*session.Manager, *historyStore, *usage.Store) {
	t.Helper()

	store, err := session.NewStore(dir, zerolog.Nop())
	if err != nil {
		t.Fatalf("session.NewStore: %v", err)
	}

	usageStore := usage.NewStore()
	hist := newHistoryStore(usageStore, zerolog.Nop())
	mgr := session.NewManager(store, hist, func(id string) persistence.UsageRecord {
		return recordFromTotals(usageStore.Snapshot(id))
	}, zerolog.Nop())

	return mgr, hist, usageStore
}

// currentHistory returns the current Session history of addr.
func currentHistory(t *testing.T, mgr *session.Manager, addr string) *persistence.ConversationHistory {
	t.Helper()

	_, h := mgr.History(context.Background(), session.Address(addr))

	return h
}
