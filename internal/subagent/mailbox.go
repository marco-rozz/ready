// mailbox.go — one bounded FIFO per child holding owner mail that arrives
// while the child is running (openspec change subagent-runtime, task 1.3,
// design D7). Full ⇒ the fixed backpressure error to the sender, never a
// silent drop. Only the owner (or, at depth 2, the parent child) sends; peer
// mail does not exist at this layer — the Runtime resolves the sender.
// Mailboxes are volatile: a daemon restart discards queued, undelivered mail
// (documented, deterministic).
package subagent

import (
	"errors"
	"sync"
	"time"
)

// ErrMailboxEmpty is returned by Pop on an empty mailbox.
var ErrMailboxEmpty = errors.New("subagent mailbox empty")

// mailbox is a bounded FIFO of Mail.
type mailbox struct {
	mu    sync.Mutex
	items []Mail
	cap   int
}

func newMailbox(capacity int) *mailbox {
	if capacity <= 0 {
		capacity = DefaultMailboxBuffer
	}

	return &mailbox{cap: capacity}
}

// Push appends m unless the mailbox is full (ErrMailboxFull).
func (mb *mailbox) Push(m Mail) error {
	mb.mu.Lock()
	defer mb.mu.Unlock()

	if len(mb.items) >= mb.cap {
		return ErrMailboxFull
	}

	m.QueuedAt = time.Now()
	mb.items = append(mb.items, m)

	return nil
}

// Pop removes and returns the oldest mail, or ErrMailboxEmpty.
func (mb *mailbox) Pop() (Mail, error) {
	mb.mu.Lock()
	defer mb.mu.Unlock()

	if len(mb.items) == 0 {
		return Mail{}, ErrMailboxEmpty
	}

	m := mb.items[0]
	mb.items = mb.items[1:]

	return m, nil
}

// Len returns the queued count.
func (mb *mailbox) Len() int {
	mb.mu.Lock()
	defer mb.mu.Unlock()

	return len(mb.items)
}
