// Package usage implements daemon-authoritative per-session LLM token
// accounting (the tui-usage-observability, the architecture decision records).
//
// The raw numbers are eino `schema.TokenUsage` values attached by the
// eino-ext provider components to assistant responses (Generate) or stream
// chunks (Stream). This package owns:
//
//   - Store: the mutex-protected per-Session accumulator registry. Keys are
//     conversation Session IDs (session-lifecycle D7), so totals survive
//     ZMQ reconnects and Connection expiry, start from zero with a new
//     Session, and are rebuilt at restore from the Session file's
//     turn_usage records (Seed) — then dropped when the Session ends.
//   - Accumulator: the per-session handle placed into a turn's
//     context.Context. Model calls executed without an accumulator in
//     context record nothing — which makes boot-time and recovery probes
//     unattributed BY CONSTRUCTION (no exclusion flags, no allow-lists).
//   - Recorder model wrappers: transparent `model.ToolCallingChatModel` and
//     `model.AgenticModel` decorators that delegate unchanged and, on each
//     response (or stream-final usage chunk), add the reported
//     `schema.TokenUsage` to the accumulator found in the call context.
//
// Counting rule: a turn counts once per delivered reply;
// LLM-call and aux-call counters increment per model call; every reported
// PromptTokens/CompletionTokens observation adds up (failover retries are
// additive — that is the honest cost). Cache% is derived downstream
// (cached/prompt) and displayed only when the provider reports cached
// tokens; Ollama never does.
package usage

import (
	"context"
	"sync"

	"github.com/cloudwego/eino/schema"
)

// Totals is the cumulative usage picture for one Session. It doubles as
// the wire-protocol payload shape: cmd/serve.go maps it to
// protocol.UsageInfo when attaching it to replies and handshakes.
//
// CachedTokens is meaningful only when the provider reports cached prompt
// tokens (Ollama never does); consumers MUST treat a session whose
// CachedTokens is 0 with no cache reporting as "absent", never "0%" —
// the HasCached flag makes that distinction explicit.
type Totals struct {
	ReadTokens    int `json:"read_tokens"`
	WrittenTokens int `json:"written_tokens"`
	CachedTokens  int `json:"cached_tokens,omitempty"`
	TurnCount     int `json:"turn_count"`
	LLMCalls      int `json:"llm_calls"`
	AuxCalls      int `json:"aux_calls"`

	// HasCached reports whether any provider on this session ever reported
	// cached prompt tokens. It is NOT serialised: the wire contract derives
	// it from cached_tokens presence (omitempty).
	HasCached bool `json:"-"`

	// LastMainPromptTokens is the most recent PromptTokens observation of a
	// MAIN-agent call (auxiliary calls — formatter post-step, compaction
	// summariser — never update it: session-lifecycle D8b). It anchors the
	// context-occupancy percentage and the compaction trigger. In-memory
	// only: never restored (the prompt prefix may differ after a restart).
	LastMainPromptTokens int `json:"-"`
}

// Snapshot returns a copy of t safe to hand across goroutines. A nil
// receiver yields nil — the "no data yet" signal used by reply attachment
// and handshake seeding.
func (t *Totals) Snapshot() *Totals {
	if t == nil {
		return nil
	}

	cp := *t

	return &cp
}

// accumulator is the per-session record behind an Accumulator handle.
type accumulator struct {
	mu     sync.Mutex
	totals Totals
}

// add records one model-call usage observation.
func (a *accumulator) add(u schema.TokenUsage, aux bool) {
	a.mu.Lock()
	defer a.mu.Unlock()

	a.totals.ReadTokens += u.PromptTokens
	a.totals.WrittenTokens += u.CompletionTokens
	if u.PromptTokenDetails.CachedTokens > 0 {
		a.totals.CachedTokens += u.PromptTokenDetails.CachedTokens
		a.totals.HasCached = true
	}
	if u.PromptTokens > 0 && !aux {
		a.totals.LastMainPromptTokens = u.PromptTokens
	}
	if aux {
		a.totals.AuxCalls++
	} else {
		a.totals.LLMCalls++
	}
}

// addTurn counts one delivered reply (success, terminal failure, and
// llm_unavailable alike).
func (a *accumulator) addTurn() {
	a.mu.Lock()
	defer a.mu.Unlock()

	a.totals.TurnCount++
}

// snapshot copies the current totals. Returns nil when the session has no
// recorded usage yet (fresh daemon) so callers can omit the segment.
func (a *accumulator) snapshot() *Totals {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.totals.LLMCalls == 0 && a.totals.AuxCalls == 0 && a.totals.TurnCount == 0 {
		return nil
	}

	return a.totals.Snapshot()
}

// Store is the daemon-global usage registry, keyed by Session ID. It is
// safe for concurrent use across session workers, aux consumers, and the
// ZMQ reply path.
type Store struct {
	mu   sync.Mutex
	keys map[string]*Accumulator
}

// NewStore creates an empty store. Persistence lives in the Session file
// (turn_usage records); the store is re-seeded at restore via Seed.
func NewStore() *Store {
	return &Store{keys: make(map[string]*Accumulator)}
}

// Accumulator returns the (lazily created) accumulator handle for a session
// key. Handles are stable for the store's lifetime: a reconnecting client
// continues the same totals.
func (s *Store) Accumulator(key string) *Accumulator {
	s.mu.Lock()
	defer s.mu.Unlock()

	acc, ok := s.keys[key]
	if !ok {
		acc = &Accumulator{store: s, key: key, inner: &accumulator{}}
		s.keys[key] = acc
	}

	return acc
}

// Accumulator is an opaque per-session handle carried in turn contexts.
// Callers treat it as a value: copying it keeps pointing at the same
// underlying record.
type Accumulator struct {
	store *Store
	key   string
	inner *accumulator
}

// Key reports the session key this accumulator attributes to.
func (a *Accumulator) Key() string { return a.key }

// AddUsage records one model-call usage observation against the session.
func (a *Accumulator) AddUsage(u schema.TokenUsage, aux bool) { a.inner.add(u, aux) }

// AddTurn counts one delivered reply for the session.
func (a *Accumulator) AddTurn() { a.inner.addTurn() }

// Snapshot returns the session's cumulative totals, or nil when the
// session has recorded nothing yet.
func (a *Accumulator) Snapshot() *Totals { return a.inner.snapshot() }

// Snapshot returns the totals for a session key directly, or nil when the
// key has no accumulator or has recorded nothing yet. Used by the ZMQ
// reply-attach and handshake-seed paths, which hold the key but not a
// context.
func (s *Store) Snapshot(key string) *Totals {
	s.mu.Lock()
	acc, ok := s.keys[key]
	s.mu.Unlock()

	if !ok {
		return nil
	}

	return acc.inner.snapshot()
}

// ctxKey is the private context-key type for attribution. Unexported so no
// other package can forge an accumulator into a context.
type ctxKey struct{}

// auxKey marks a context as carrying an auxiliary-consumer call
// (formatter post-step, compaction summariser) so the recorder counts it
// under AuxCalls instead of LLMCalls.
type auxKey struct{}

// WithAccumulator returns a copy of ctx carrying acc. Every model call
// executed under the returned context attributes its usage to acc's
// session; calls without this value in context record nothing.
func WithAccumulator(ctx context.Context, acc *Accumulator) context.Context {
	return context.WithValue(ctx, ctxKey{}, acc)
}

// FromContext extracts the accumulator from ctx, or nil when absent
// (probes, tests, non-turn callers).
func FromContext(ctx context.Context) *Accumulator {
	acc, _ := ctx.Value(ctxKey{}).(*Accumulator)

	return acc
}

// WithAux returns a copy of ctx marked as an auxiliary-consumer call. The
// orchestrator's AuxModel adapter wraps its candidate calls with this so
// the recorder can separate aux counters from main ReAct counters.
func WithAux(ctx context.Context) context.Context {
	return context.WithValue(ctx, auxKey{}, true)
}

// isAux reports whether ctx is marked as an auxiliary call.
func isAux(ctx context.Context) bool {
	v, _ := ctx.Value(auxKey{}).(bool)

	return v
}

// Seed installs restored totals for a Session whose accumulator has not
// recorded anything yet in this process (restore after a restart). It is a
// no-op when the Session already has live totals, so a late restore never
// overwrites them.
func (s *Store) Seed(key string, t Totals) {
	acc := s.Accumulator(key)

	acc.inner.mu.Lock()
	defer acc.inner.mu.Unlock()

	cur := acc.inner.totals
	if cur.LLMCalls != 0 || cur.AuxCalls != 0 || cur.TurnCount != 0 {
		return
	}

	t.LastMainPromptTokens = 0
	t.HasCached = t.CachedTokens > 0
	acc.inner.totals = t
}

// Drop releases a Session's totals (the Session ended; its closing record
// holds the final snapshot).
func (s *Store) Drop(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	delete(s.keys, key)
}
