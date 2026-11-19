// Package activity tracks READY-managed tool invocations for a single turn.
package activity

import (
	"context"
	"sync"
)

type Snapshot struct {
	Sequence        uint64
	ToolName        string
	AdditionalTools int
}

type Tracker struct {
	mu       sync.Mutex
	next     uint64
	sequence uint64
	active   map[uint64]string
	updates  chan Snapshot
	closed   bool
}

func New() *Tracker {
	return &Tracker{active: make(map[uint64]string), updates: make(chan Snapshot, 1)}
}
func (t *Tracker) Updates() <-chan Snapshot { return t.updates }

// Start returns an idempotent completion function. Publishing never blocks.
func (t *Tracker) Start(name string) func() {
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()

		return func() {}
	}
	t.next++
	id := t.next
	t.active[id] = name
	t.publishLocked()
	t.mu.Unlock()
	var once sync.Once

	return func() {
		once.Do(func() {
			t.mu.Lock()
			defer t.mu.Unlock()
			if t.closed {
				return
			}
			delete(t.active, id)
			t.publishLocked()
		})
	}
}

func (t *Tracker) publishLocked() {
	t.sequence++
	s := Snapshot{Sequence: t.sequence}
	var first uint64
	for id, name := range t.active {
		if first == 0 || id < first {
			first = id
			s.ToolName = name
		}
	}
	if len(t.active) > 0 {
		s.AdditionalTools = len(t.active) - 1
	}
	select {
	case t.updates <- s:
	default:
		select {
		case <-t.updates:
		default:
		}
		select {
		case t.updates <- s:
		default:
		}
	}
}

func (t *Tracker) Close() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.closed {
		t.closed = true
		close(t.updates)
	}
}

type contextKey struct{}

func WithTracker(ctx context.Context, tracker *Tracker) context.Context {
	return context.WithValue(ctx, contextKey{}, tracker)
}

func Enabled(ctx context.Context) bool {
	t, ok := ctx.Value(contextKey{}).(*Tracker)

	return ok && t != nil
}

func Start(ctx context.Context, name string) func() {
	if t, ok := ctx.Value(contextKey{}).(*Tracker); ok && t != nil {
		return t.Start(name)
	}

	return func() {}
}
