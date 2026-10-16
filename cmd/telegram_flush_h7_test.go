package main

// Regression tests for criticalities-review-2026-09-24 H7: the Telegram
// streaming flush used to replace the message text with ONLY the chunks
// buffered since the last flush (queue-only), and a flush racing the
// completion path could land after the final edit and overwrite the complete
// response with stale partial content.

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/rs/zerolog"
)

// recordingEditSender records every editMessageText text it is asked to send.
type recordingEditSender struct {
	mu    sync.Mutex
	edits []string
}

func (r *recordingEditSender) SendStreamMessage(_ context.Context, _ int64, _ string) (int, error) {
	return 1, nil
}

func (r *recordingEditSender) SendStreamEdit(_ context.Context, _ int64, _ int, content string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.edits = append(r.edits, content)

	return nil
}

func newTelegramStreamState() *streamState {
	return &streamState{
		tgChatID:    42,
		tgMessageID: 42,
		tgDone:      make(chan struct{}),
		// tgTicker is only used by the background flusher goroutine; nil is
		// fine for direct flushTelegram calls.
		tgTicker: nil,
	}
}

func deliverChunks(st *streamState, chunks ...string) {
	for _, c := range chunks {
		st.tgFull.WriteString(c)
		st.tgBuffer.WriteString(c)
		st.tgDirty = true
	}
}

// TestFlushTelegram_Cumulative pins H7 (queue-only edit): a flush mid-stream
// edits with the FULL accumulated text, never only the last flush window.
func TestFlushTelegram_Cumulative(t *testing.T) {
	ts := &recordingEditSender{}
	st := newTelegramStreamState()

	deliverChunks(st, "light ")
	st.flushTelegram(context.Background(), ts, zerolog.Nop())

	deliverChunks(st, "and fan ")
	st.flushTelegram(context.Background(), ts, zerolog.Nop())

	if len(ts.edits) != 2 {
		t.Fatalf("edits = %v, want 2", ts.edits)
	}

	if !strings.HasPrefix(ts.edits[0], "light ") {
		t.Fatalf("first edit = %q", ts.edits[0])
	}

	if ts.edits[1] != "light and fan " {
		t.Fatalf("second edit = %q, want cumulative %q (queue-only flush replaced the whole message with the tail)", ts.edits[1], "light and fan ")
	}
}

// TestFlushTelegram_NeverFollowsTheFinalEdit pins the H7 race: after the
// completion path set tgClosed under the mutex, every subsequent flush is a
// no-op — the final edit is the last edit.
func TestFlushTelegram_NeverFollowsTheFinalEdit(t *testing.T) {
	ts := &recordingEditSender{}
	st := newTelegramStreamState()

	deliverChunks(st, "partial ")
	st.flushTelegram(context.Background(), ts, zerolog.Nop())

	// Simulate the completion path: tgClosed set under the mutex (what
	// cleanup/terminal do), then the final edit happens.
	st.tgMu.Lock()
	st.tgClosed = true
	close(st.tgDone)
	st.tgMu.Unlock()

	// A flush that was in flight (or fires later) must not edit again.
	st.flushTelegram(context.Background(), ts, zerolog.Nop())

	if len(ts.edits) != 1 {
		t.Fatalf("edits after tgClosed = %v, want only the pre-close flush (stale partial content must not follow the final edit)", ts.edits)
	}
}

// TestFlushTelegram_NoFlushWhenNothingNew pins the dirty-flag behaviour: a
// cumulative flush only fires when a new chunk arrived since the last one.
func TestFlushTelegram_NoFlushWhenNothingNew(t *testing.T) {
	ts := &recordingEditSender{}
	st := newTelegramStreamState()

	deliverChunks(st, "hello")
	st.flushTelegram(context.Background(), ts, zerolog.Nop())
	// No new chunks: a second flush must not re-send.
	st.flushTelegram(context.Background(), ts, zerolog.Nop())

	if len(ts.edits) != 1 {
		t.Fatalf("edits = %v, want 1 (no redundant cumulative edits)", ts.edits)
	}
}
