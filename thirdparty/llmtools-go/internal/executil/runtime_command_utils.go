package executil

import (
	"bytes"
	"sync"
)

type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}

func (b *lockedBuffer) Bytes() []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]byte(nil), b.b.Bytes()...)
}

type cappedWriter struct {
	mu        sync.Mutex
	capBytes  int
	buf       []byte // fixed size capBytes
	start     int    // ring start
	n         int    // number of valid bytes in ring
	total     int64
	truncated bool
}

func newCappedWriter(capBytes int64) *cappedWriter {
	if capBytes <= 0 {
		return &cappedWriter{}
	}

	cb := int(capBytes)
	return &cappedWriter{
		capBytes: cb,
		buf:      make([]byte, cb),
	}
}

func (w *cappedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	w.total += int64(len(p))

	if len(p) == 0 || w.capBytes <= 0 {
		return len(p), nil
	}

	// Tail-capture semantics:
	// Keep the last capBytes bytes written across all writes.
	if len(p) >= w.capBytes {
		copy(w.buf, p[len(p)-w.capBytes:])
		w.start = 0
		w.n = w.capBytes
		w.truncated = true
		return len(p), nil
	}

	// If we would exceed capacity, drop from the front (advance start).
	overflow := (w.n + len(p)) - w.capBytes
	if overflow > 0 {
		w.start = (w.start + overflow) % w.capBytes
		w.n -= overflow
		w.truncated = true
	}

	// Append at end position.
	end := (w.start + w.n) % w.capBytes
	// Copy with wrap.
	first := min(len(p), w.capBytes-end)
	copy(w.buf[end:end+first], p[:first])
	if first < len(p) {
		copy(w.buf[0:len(p)-first], p[first:])
	}
	w.n += len(p)
	return len(p), nil
}

func (w *cappedWriter) Bytes() []byte {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.n == 0 {
		return nil
	}
	out := make([]byte, w.n)
	if w.start+w.n <= w.capBytes {
		copy(out, w.buf[w.start:w.start+w.n])
		return out
	}
	// Wrapped.
	n1 := w.capBytes - w.start
	copy(out, w.buf[w.start:])
	copy(out[n1:], w.buf[:w.n-n1])
	return out
}

func (w *cappedWriter) TotalBytes() int64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.total
}

func (w *cappedWriter) Truncated() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.truncated
}
