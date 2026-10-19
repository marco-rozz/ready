package executil

import (
	"container/list"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"sync"
	"time"
)

type SessionStore struct {
	mu  sync.Mutex
	ttl time.Duration
	max int

	lru *list.List               // front=most recently used
	m   map[string]*list.Element // id -> *list.Element(Value=*sessionItem)
}

type sessionItem struct {
	s        *ShellSession
	lastUsed time.Time
}

func NewSessionStore(defaultTTL time.Duration, defaultMaxSessions int) *SessionStore {
	return &SessionStore{
		ttl: defaultTTL,
		max: defaultMaxSessions,
		lru: list.New(),
		m:   map[string]*list.Element{},
	}
}

func (ss *SessionStore) SetTTL(ttl time.Duration) {
	if ttl < 0 {
		ttl = 0
	}
	ss.mu.Lock()
	ss.ttl = ttl
	ss.evictExpiredLocked(time.Now())
	ss.mu.Unlock()
}

func (ss *SessionStore) SetMaxSessions(maxSessions int) {
	if maxSessions < 0 {
		maxSessions = 0
	}
	ss.mu.Lock()
	ss.max = maxSessions
	ss.evictOverLimitLocked()
	ss.mu.Unlock()
}

func (ss *SessionStore) NewSession() *ShellSession {
	now := time.Now()
	ss.mu.Lock()
	defer ss.mu.Unlock()
	ss.evictExpiredLocked(now)
	ss.evictOverLimitLocked()

	id := newSessionID()
	s := &ShellSession{
		id:      id,
		workdir: "",
		env:     map[string]string{},
	}
	it := &sessionItem{s: s, lastUsed: now}
	e := ss.lru.PushFront(it)
	ss.m[id] = e
	ss.evictOverLimitLocked()

	return s
}

func (ss *SessionStore) Get(id string) (*ShellSession, bool) {
	now := time.Now()
	ss.mu.Lock()
	ss.evictExpiredLocked(now)
	e, ok := ss.m[id]
	if !ok || e == nil {
		ss.mu.Unlock()
		return nil, false
	}
	it, _ := e.Value.(*sessionItem)
	if it == nil || it.s == nil {
		// Corrupt entry; delete.
		ss.deleteElemLocked(e)
		ss.mu.Unlock()
		return nil, false
	}
	it.lastUsed = now
	ss.lru.MoveToFront(e)
	s := it.s
	ss.mu.Unlock()

	s.mu.RLock()
	closed := s.closed
	s.mu.RUnlock()
	if closed {
		return nil, false
	}
	return s, true
}

func (ss *SessionStore) Delete(id string) {
	ss.mu.Lock()
	e := ss.m[id]
	if e != nil {
		ss.deleteElemLocked(e)
	}
	ss.mu.Unlock()
}

func (ss *SessionStore) Size() int {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	return len(ss.m)
}

func (ss *SessionStore) evictExpiredLocked(now time.Time) {
	if ss.ttl <= 0 {
		return
	}
	// Oldest at back; stop once we find a non-expired entry.
	for e := ss.lru.Back(); e != nil; {
		prev := e.Prev()
		it, ok := e.Value.(*sessionItem)
		if !ok || it == nil {
			// Corrupt entry; delete defensively.
			ss.deleteElemLocked(e)
			e = prev
			continue
		}
		if now.Sub(it.lastUsed) <= ss.ttl {
			break
		}
		ss.deleteElemLocked(e)
		e = prev
	}
}

func (ss *SessionStore) evictOverLimitLocked() {
	if ss.max <= 0 {
		return
	}
	for ss.lru.Len() > ss.max {
		if e := ss.lru.Back(); e != nil {
			ss.deleteElemLocked(e)
		} else {
			return
		}
	}
}

func (ss *SessionStore) deleteElemLocked(e *list.Element) {
	if e == nil {
		return
	}
	it, _ := e.Value.(*sessionItem)
	if it == nil || it.s == nil {
		ss.lru.Remove(e)
		return
	}
	delete(ss.m, it.s.id)
	ss.lru.Remove(e)

	// Mark closed.
	it.s.mu.Lock()
	it.s.closed = true
	it.s.mu.Unlock()
}

func newSessionID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err == nil {
		return "sess_" + hex.EncodeToString(b[:])
	}
	now := time.Now().UTC().UnixNano()
	return fmt.Sprintf("sess_%d_%d", now, os.Getpid())
}
