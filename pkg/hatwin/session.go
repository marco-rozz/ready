package hatwin

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

// firstDynamicID is the starting value (exclusive) of the per-session dynamic
// message-id counter shared by pings and commands. It sits above the fixed
// bootstrap ids (2–7) so every id on the wire strictly increases: HA rejects
// any message id not greater than the previous one (id_reuse), which would
// kill pings after a command if the two used disjoint ranges.
const firstDynamicID int64 = 100

// wsSession is the Twin-level handle to one live WebSocket session. It is
// published into Twin.session after the bootstrap commands are written and
// cleared when the session ends. All command traffic — pings and call_service
// alike — flows through it.
//
// The id counter and the write share one mutex (D1): ids reach the wire in
// strictly increasing order regardless of which goroutine sends.
type wsSession struct {
	conn *websocket.Conn

	// mu guards nextID, the waiters map, and the write itself. handleResult
	// takes waitersMu (never mu) to deliver results, so a slow write never
	// blocks the read loop.
	mu     sync.Mutex
	nextID int64

	// waitersMu guards waiters. Lock order: waitersMu → mu (never reversed).
	waitersMu sync.Mutex
	waiters   map[int64]chan map[string]any

	// bootstrapDone is set once this session's bootstrap responses have all
	// arrived (registries consistent). The write barrier requires it.
	bootstrapDone atomic.Bool

	// closedCh is closed exactly once when the session ends; cause carries
	// the read-loop error for ErrSessionClosed wrapping.
	closedCh   chan struct{}
	closeOnce  sync.Once
	causeMu    sync.Mutex
	cause      error
	createTime time.Time
}

func newWSSession(conn *websocket.Conn) *wsSession {
	return &wsSession{
		conn:       conn,
		nextID:     firstDynamicID,
		waiters:    make(map[int64]chan map[string]any),
		closedCh:   make(chan struct{}),
		createTime: time.Now(),
	}
}

// sendRaw writes v to the wire without touching the id counter (auth
// handshake, and bootstrap commands which carry their fixed ids). It takes mu,
// so raw sends and dynamic-id sends can never interleave on the wire.
func (s *wsSession) sendRaw(v any) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	_ = s.conn.SetWriteDeadline(time.Now().Add(writeDeadline))

	return s.conn.WriteJSON(v)
}

// send allocates the next id under the write lock, stamps it into msg, writes
// the frame, and returns the id. The allocation-inside-the-lock discipline is
// what keeps ids on the wire strictly increasing under concurrent senders.
func (s *wsSession) send(msg map[string]any) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.nextID++
	id := s.nextID
	msg["id"] = id

	_ = s.conn.SetWriteDeadline(time.Now().Add(writeDeadline))

	return id, s.conn.WriteJSON(msg)
}

// call sends msg and waits for the correlated result. The waiter is
// registered before the frame is written (under the same waitersMu → mu lock
// order handleResult uses), so the result can never arrive unwaited.
func (s *wsSession) call(msg map[string]any) (int64, <-chan map[string]any, error) {
	s.waitersMu.Lock()
	s.mu.Lock()

	s.nextID++
	id := s.nextID
	msg["id"] = id
	ch := make(chan map[string]any, 1)
	s.waiters[id] = ch

	err := func() error {
		defer s.mu.Unlock()

		_ = s.conn.SetWriteDeadline(time.Now().Add(writeDeadline))

		return s.conn.WriteJSON(msg)
	}()
	s.waitersMu.Unlock()

	if err != nil {
		s.removeWaiter(id)

		return id, nil, err
	}

	return id, ch, nil
}

// wait blocks until the result for id arrives, the session ends, or ctx is
// done. On session end the waiter channel is closed — a receive from it must
// be interpreted as the typed connection failure, never as a result.
func (s *wsSession) wait(ctx context.Context, id int64, ch <-chan map[string]any) (map[string]any, error) {
	select {
	case res, ok := <-ch:
		if !ok {
			s.causeMu.Lock()
			cause := s.cause
			s.causeMu.Unlock()

			return nil, fmt.Errorf("%w: %w", ErrSessionClosed, cause)
		}

		return res, nil
	case <-s.closedCh:
		s.causeMu.Lock()
		cause := s.cause
		s.causeMu.Unlock()

		return nil, fmt.Errorf("%w: %w", ErrSessionClosed, cause)
	case <-ctx.Done():
		s.removeWaiter(id)

		return nil, ctx.Err()
	}
}

// removeWaiter drops a registration (call failed to write, or the caller gave
// up). Safe to call twice; the buffered channel absorbs a late delivery.
func (s *wsSession) removeWaiter(id int64) {
	s.waitersMu.Lock()
	defer s.waitersMu.Unlock()
	delete(s.waiters, id)
}

// routeResult delivers msg to the waiter registered for its id, reporting
// whether one existed. Called from handleResult before any bootstrap handling.
func (s *wsSession) routeResult(id int64, msg map[string]any) bool {
	s.waitersMu.Lock()
	defer s.waitersMu.Unlock()

	ch, ok := s.waiters[id]
	if !ok {
		return false
	}
	delete(s.waiters, id)
	ch <- msg

	return true
}

// close ends the session: every pending waiter fails with ErrSessionClosed
// wrapping cause, and closedCh signals all other waiters.
func (s *wsSession) close(cause error) {
	s.causeMu.Lock()
	s.cause = cause
	s.causeMu.Unlock()

	s.closeOnce.Do(func() {
		s.waitersMu.Lock()
		waiters := s.waiters
		s.waiters = make(map[int64]chan map[string]any)
		s.waitersMu.Unlock()

		for _, ch := range waiters {
			close(ch)
		}

		close(s.closedCh)
	})
}

// done returns the session-end signal channel.
func (s *wsSession) done() <-chan struct{} {
	return s.closedCh
}

// bootstrapComplete reports whether this session finished its bootstrap.
func (s *wsSession) bootstrapComplete() bool {
	return s.bootstrapDone.Load()
}

// markBootstrapDone flips this session's write-barrier flag (called by the
// bootstrap tracker when the last bootstrap response arrives).
func (s *wsSession) markBootstrapDone() {
	s.bootstrapDone.Store(true)
}
