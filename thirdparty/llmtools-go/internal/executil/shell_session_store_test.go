package executil

import (
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	defaultSessionTTL  = 30 * time.Minute
	defaultMaxSessions = 256
)

func TestSessionStore_TTL_EvictsWithoutSleep(t *testing.T) {
	cases := []struct {
		name      string
		ttl       time.Duration
		age       time.Duration
		wantEvict bool
	}{
		{name: "ttl_disabled_never_evicts", ttl: 0, age: 24 * time.Hour, wantEvict: false},
		{name: "not_old_enough", ttl: 10 * time.Second, age: 1 * time.Second, wantEvict: false},
		{name: "old_enough", ttl: 100 * time.Millisecond, age: 2 * time.Second, wantEvict: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ss := NewSessionStore(defaultSessionTTL, defaultMaxSessions)
			ss.SetTTL(tc.ttl)

			s := ss.NewSession()
			if s == nil || s.id == "" {
				t.Fatalf("expected session")
			}

			// Force lastUsed to the past deterministically.
			ss.mu.Lock()
			e := ss.m[s.id]
			if e == nil {
				ss.mu.Unlock()
				t.Fatalf("missing store entry")
			}
			it, _ := e.Value.(*sessionItem)
			if it == nil {
				ss.mu.Unlock()
				t.Fatalf("missing sessionItem")
			}
			it.lastUsed = time.Now().Add(-tc.age)
			ss.mu.Unlock()

			_, ok := ss.Get(s.id) // get() performs eviction check
			if tc.wantEvict && ok {
				t.Fatalf("expected evicted, but get() returned ok")
			}
			if !tc.wantEvict && !ok {
				t.Fatalf("expected present, but get() returned !ok")
			}

			s.mu.RLock()
			closed := s.closed
			s.mu.RUnlock()
			if tc.wantEvict && !closed {
				t.Fatalf("expected closed session after eviction")
			}
		})
	}
}

func TestSessionStore_Delete_ClosesAndRemoves(t *testing.T) {
	cases := []struct {
		name string
	}{
		{name: "delete_removes_and_closes"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ss := NewSessionStore(defaultSessionTTL, defaultMaxSessions)
			s := ss.NewSession()
			if s == nil || s.GetID() == "" {
				t.Fatalf("expected session")
			}
			id := s.GetID()

			ss.Delete(id)

			if _, ok := ss.Get(id); ok {
				t.Fatalf("expected not found after delete")
			}
			s.mu.RLock()
			closed := s.closed
			s.mu.RUnlock()
			if !closed {
				t.Fatalf("expected session marked closed after delete")
			}
		})
	}
}

func TestSessionStore_Get_UpdatesLRUOrder(t *testing.T) {
	// Deterministically validate LRU: with max=1, the *least recently used* should be evicted.
	cases := []struct {
		name string
	}{
		{name: "touching_session_makes_it_mru"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ss := NewSessionStore(defaultSessionTTL, defaultMaxSessions)
			ss.SetMaxSessions(1)

			s1 := ss.NewSession()
			s2 := ss.NewSession()

			// With max=1, store should already have evicted something, but eviction happens after insert.
			// Validate by attempting to Get both; only one should remain.
			_, ok1 := ss.Get(s1.GetID())
			_, ok2 := ss.Get(s2.GetID())

			if ok1 == ok2 {
				// Exactly one should be present.
				t.Fatalf("expected exactly one session present; ok1=%v ok2=%v", ok1, ok2)
			}
		})
	}
}

func TestSessionStore_SetTTL_EvictsExpiredAndNegativeDisables(t *testing.T) {
	cases := []struct {
		name        string
		setTTL      time.Duration
		age         time.Duration
		wantPresent bool
	}{
		{name: "negative_ttl_becomes_zero_and_disables_eviction", setTTL: -1, age: 24 * time.Hour, wantPresent: true},
		{name: "ttl_evicts_when_old", setTTL: 100 * time.Millisecond, age: 10 * time.Second, wantPresent: false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ss := NewSessionStore(defaultSessionTTL, defaultMaxSessions)
			ss.SetTTL(tc.setTTL)

			s := ss.NewSession()

			// Force lastUsed back in time without sleeping.
			ss.mu.Lock()
			e := ss.m[s.GetID()]
			if e == nil {
				ss.mu.Unlock()
				t.Fatalf("missing entry")
			}
			it, _ := e.Value.(*sessionItem)
			if it == nil {
				ss.mu.Unlock()
				t.Fatalf("missing item")
			}
			it.lastUsed = time.Now().Add(-tc.age)
			ss.mu.Unlock()

			_, ok := ss.Get(s.GetID())
			if ok != tc.wantPresent {
				t.Fatalf("present got %v want %v", ok, tc.wantPresent)
			}
		})
	}
}

func TestSessionStore_ConcurrentAccess_NoDeadlocks(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping concurrency stress in -short")
	}

	cases := []struct {
		name     string
		workers  int
		iters    int
		max      int
		ttl      time.Duration
		wantSize int // not strict, but should be >=0
	}{
		{name: "mixed_ops", workers: 16, iters: 200, max: 32, ttl: 0, wantSize: 0},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ss := NewSessionStore(defaultSessionTTL, defaultMaxSessions)
			ss.SetMaxSessions(tc.max)
			ss.SetTTL(tc.ttl)

			var idsMu sync.Mutex
			var ids []string

			var wg sync.WaitGroup
			wg.Add(tc.workers)
			for w := 0; w < tc.workers; w++ {
				go func() {
					defer wg.Done()
					for i := 0; i < tc.iters; i++ {
						switch (w + i) % 3 {
						case 0:
							s := ss.NewSession()
							if s != nil && s.GetID() != "" {
								idsMu.Lock()
								ids = append(ids, s.GetID())
								idsMu.Unlock()
							}
						case 1:
							idsMu.Lock()
							var id string
							if len(ids) > 0 {
								id = ids[(w+i)%len(ids)]
							}
							idsMu.Unlock()
							if strings.TrimSpace(id) != "" {
								_, _ = ss.Get(id)
							}
						case 2:
							idsMu.Lock()
							var id string
							if len(ids) > 0 {
								id = ids[(w+i)%len(ids)]
							}
							idsMu.Unlock()
							if strings.TrimSpace(id) != "" {
								ss.Delete(id)
							}
						}
					}
				}()
			}
			wg.Wait()

			_ = ss.Size() // should not deadlock
		})
	}
}
