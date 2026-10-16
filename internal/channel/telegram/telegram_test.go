package telegram

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/marco-rozz/ready/internal/channel"
	"github.com/rs/zerolog"
)

// mockBot is a test double for the botAPI interface consumed by Channel.
type mockBot struct {
	mu            sync.Mutex
	sent          []tgbotapi.MessageConfig
	sentEdits     []tgbotapi.EditMessageTextConfig
	sendErr       error
	sendErrCount  int // how many Send calls should fail (0 = all fail when sendErr != nil)
	sendCalls     int
	messageID     int // MessageID to return from Send (0 = default)
	updates       chan tgbotapi.Update
	getUpdatesErr error
	stopOnce      sync.Once
	stopped       chan struct{}
}

func newMockBot() *mockBot {
	return &mockBot{
		updates: make(chan tgbotapi.Update, 8),
		stopped: make(chan struct{}),
	}
}

func (m *mockBot) Send(_ context.Context, c tgbotapi.Chattable) (tgbotapi.Message, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.sendCalls++

	if m.sendErr != nil {
		if m.sendErrCount == 0 || m.sendCalls <= m.sendErrCount {
			return tgbotapi.Message{}, m.sendErr
		}
	}

	if mc, ok := c.(tgbotapi.MessageConfig); ok {
		m.sent = append(m.sent, mc)

		return tgbotapi.Message{MessageID: m.messageID}, nil
	}

	if ec, ok := c.(tgbotapi.EditMessageTextConfig); ok {
		m.sentEdits = append(m.sentEdits, ec)

		return tgbotapi.Message{MessageID: m.messageID}, nil
	}

	return tgbotapi.Message{MessageID: m.messageID}, nil
}

func (m *mockBot) SendChatAction(_ context.Context, _ int64, _ string) error {
	return nil
}

// GetUpdates blocks until at least one update is available, StopReceivingUpdates
// is called, or getUpdatesErr is set (returned immediately).
func (m *mockBot) GetUpdates(_ tgbotapi.UpdateConfig) ([]tgbotapi.Update, error) {
	m.mu.Lock()
	err := m.getUpdatesErr
	m.mu.Unlock()

	if err != nil {
		return nil, err
	}

	select {
	case upd := <-m.updates:
		out := []tgbotapi.Update{upd}
		for {
			select {
			case u := <-m.updates:
				out = append(out, u)
			default:
				return out, nil
			}
		}
	case <-m.stopped:
		return nil, nil
	}
}

// StopReceivingUpdates unblocks any pending GetUpdates call.
func (m *mockBot) StopReceivingUpdates() {
	m.stopOnce.Do(func() { close(m.stopped) })
}

func (m *mockBot) snapshot() []tgbotapi.MessageConfig {
	m.mu.Lock()
	defer m.mu.Unlock()

	out := make([]tgbotapi.MessageConfig, len(m.sent))
	copy(out, m.sent)

	return out
}

func textUpdate(chatID, userID int64, username, text string) tgbotapi.Update {
	return tgbotapi.Update{
		UpdateID: int(chatID),
		Message: &tgbotapi.Message{
			Chat: &tgbotapi.Chat{ID: chatID},
			From: &tgbotapi.User{ID: userID, UserName: username},
			Text: text,
		},
	}
}

func TestChannel_Name(t *testing.T) {
	bot := newMockBot()
	ch := newChannel(bot, nil, zerolog.Nop())
	defer func() { _ = ch.Close() }()

	if got := ch.Name(); got != ChannelName {
		t.Errorf("expected name %q, got %q", ChannelName, got)
	}
}

func TestChannel_Send_HTMLAndChatID(t *testing.T) {
	bot := newMockBot()
	ch := newChannel(bot, nil, zerolog.Nop())
	defer func() { _ = ch.Close() }()

	err := ch.Send(context.Background(), channel.Message{
		Content:  "hello <b>world</b>",
		Metadata: map[string]string{MetadataChatID: "42"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	sent := bot.snapshot()
	if len(sent) != 1 {
		t.Fatalf("expected 1 message, got %d", len(sent))
	}

	if sent[0].ChatID != 42 {
		t.Errorf("expected chat id 42, got %d", sent[0].ChatID)
	}

	if sent[0].ParseMode != tgbotapi.ModeHTML {
		t.Errorf("expected HTML parse mode, got %q", sent[0].ParseMode)
	}

	if sent[0].Text != "hello <b>world</b>" {
		t.Errorf("unexpected text: %q", sent[0].Text)
	}
}

func TestChannel_Send_MissingChatID(t *testing.T) {
	bot := newMockBot()
	ch := newChannel(bot, nil, zerolog.Nop())
	defer func() { _ = ch.Close() }()

	err := ch.Send(context.Background(), channel.Message{Content: "x"})
	if !errors.Is(err, ErrMissingChatID) {
		t.Fatalf("expected ErrMissingChatID, got %v", err)
	}
}

func TestChannel_Send_InvalidChatID(t *testing.T) {
	bot := newMockBot()
	ch := newChannel(bot, nil, zerolog.Nop())
	defer func() { _ = ch.Close() }()

	err := ch.Send(context.Background(), channel.Message{
		Content:  "x",
		Metadata: map[string]string{MetadataChatID: "not-a-number"},
	})
	if !errors.Is(err, ErrInvalidChatID) {
		t.Fatalf("expected ErrInvalidChatID, got %v", err)
	}
}

func TestChannel_Send_LongMessageSplit(t *testing.T) {
	bot := newMockBot()
	ch := newChannel(bot, nil, zerolog.Nop())
	defer func() { _ = ch.Close() }()

	// Build text larger than MaxMessageLength with no newlines so it must split.
	long := strings.Repeat("a", MaxMessageLength+100)

	err := ch.Send(context.Background(), channel.Message{
		Content:  long,
		Metadata: map[string]string{MetadataChatID: "7"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	sent := bot.snapshot()
	if len(sent) != 2 {
		t.Fatalf("expected 2 chunks, got %d", len(sent))
	}

	if len([]rune(sent[0].Text)) != MaxMessageLength {
		t.Errorf("expected first chunk size %d, got %d", MaxMessageLength, len([]rune(sent[0].Text)))
	}

	if len([]rune(sent[1].Text)) != 100 {
		t.Errorf("expected second chunk size 100, got %d", len([]rune(sent[1].Text)))
	}
}

func TestChannel_Receive_TextMessage(t *testing.T) {
	bot := newMockBot()
	ch := newChannel(bot, nil, zerolog.Nop())
	defer func() { _ = ch.Close() }()

	bot.updates <- textUpdate(123, 1, "alice", "ciao")

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	msg, err := ch.Receive(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if msg.ChannelName != ChannelName {
		t.Errorf("expected channel %q, got %q", ChannelName, msg.ChannelName)
	}

	if msg.Content != "ciao" {
		t.Errorf("expected content %q, got %q", "ciao", msg.Content)
	}

	if msg.Metadata[MetadataChatID] != "123" {
		t.Errorf("expected chat_id 123, got %q", msg.Metadata[MetadataChatID])
	}
}

func TestChannel_Receive_FiltersNonText(t *testing.T) {
	bot := newMockBot()
	ch := newChannel(bot, nil, zerolog.Nop())
	defer func() { _ = ch.Close() }()

	// Non-text update (empty Text) should be dropped, the next text update delivered.
	bot.updates <- tgbotapi.Update{Message: &tgbotapi.Message{Chat: &tgbotapi.Chat{ID: 1}}}
	bot.updates <- textUpdate(1, 1, "bob", "hello")

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	msg, err := ch.Receive(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if msg.Content != "hello" {
		t.Errorf("expected %q, got %q", "hello", msg.Content)
	}
}

func TestChannel_Receive_AllowFromWhitelist(t *testing.T) {
	bot := newMockBot()
	ch := newChannel(bot, []string{"42", "@alice"}, zerolog.Nop())
	defer func() { _ = ch.Close() }()

	// Unauthorised chat (id 99) should be dropped.
	bot.updates <- textUpdate(99, 5, "intruder", "knock")
	// Authorised by chat id.
	bot.updates <- textUpdate(42, 5, "anyone", "by chat")
	// Authorised by @username form.
	bot.updates <- textUpdate(7, 6, "alice", "by username")

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	first, err := ch.Receive(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if first.Content != "by chat" {
		t.Errorf("expected first allowed message %q, got %q", "by chat", first.Content)
	}

	second, err := ch.Receive(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if second.Content != "by username" {
		t.Errorf("expected second allowed message %q, got %q", "by username", second.Content)
	}
}

func TestChannel_Receive_ContextCancellation(t *testing.T) {
	bot := newMockBot()
	ch := newChannel(bot, nil, zerolog.Nop())
	defer func() { _ = ch.Close() }()

	ctx, cancel := context.WithCancel(context.Background())

	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()

	if _, err := ch.Receive(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
}

func TestChannel_PollLoop_ConflictRecovers(t *testing.T) {
	base := conflictBackoffBase
	conflictBackoffBase = time.Millisecond
	defer func() { conflictBackoffBase = base }()

	bot := newMockBot()
	// Telegram answers 409 while another listener holds the long poll. The
	// conflict is a *property* condition, not a permanent failure: once the
	// competing listener exits, our own polling succeeds again — so the poll
	// loop must retry with backoff, never close itself.
	bot.getUpdatesErr = &tgbotapi.Error{Code: 409, Message: "Conflict: terminated by other getUpdates request"}

	ch := newChannel(bot, nil, zerolog.Nop())
	defer func() { _ = ch.Close() }()

	// While the conflict persists, Receive must NOT return ErrClosed — the
	// channel stays alive, retrying in the background.
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	_, err := ch.Receive(ctx)
	cancel()

	if errors.Is(err, ErrClosed) {
		t.Fatalf("channel declared itself closed while conflict persists")
	}

	// The competing listener goes away; the next poll succeeds.
	const greeting = "ciao"

	bot.mu.Lock()
	bot.getUpdatesErr = nil
	bot.updates <- textUpdate(123, 42, "alice", greeting)
	bot.mu.Unlock()

	msg, err := ch.Receive(context.Background())
	if err != nil {
		t.Fatalf("expected message after conflict cleared, got %v", err)
	}

	if msg.Content != greeting {
		t.Fatalf("expected recovered message %q, got %q", greeting, msg.Content)
	}

	// Close must still terminate the (still-running) poll loop cleanly.
	if err := ch.Close(); err != nil {
		t.Fatalf("Close after recovery: %v", err)
	}
}

func TestChannel_Close_Idempotent(t *testing.T) {
	bot := newMockBot()
	ch := newChannel(bot, nil, zerolog.Nop())

	if err := ch.Close(); err != nil {
		t.Fatalf("first close error: %v", err)
	}

	if err := ch.Close(); err != nil {
		t.Fatalf("second close error: %v", err)
	}
}

func TestSplitMessage_NoSplitWhenShort(t *testing.T) {
	got := splitMessage("hello", MaxMessageLength)

	if len(got) != 1 || got[0] != "hello" {
		t.Errorf("expected single chunk hello, got %v", got)
	}
}

func TestSplitMessage_PrefersNewlineBoundary(t *testing.T) {
	// Build a message: 10 chars + \n + 10 chars, max=15 → should split at the newline (index 10).
	text := "aaaaaaaaaa\nbbbbbbbbbb"
	got := splitMessage(text, 15)

	if len(got) != 2 {
		t.Fatalf("expected 2 chunks, got %d (%v)", len(got), got)
	}

	if got[0] != "aaaaaaaaaa\n" {
		t.Errorf("first chunk should end at newline, got %q", got[0])
	}

	if got[1] != "bbbbbbbbbb" {
		t.Errorf("second chunk wrong: %q", got[1])
	}
}

func TestChannel_Send_FallbackToPlainText(t *testing.T) {
	bot := newMockBot()
	// First call fails (HTML rejected), second call succeeds (plain text fallback).
	bot.sendErr = errors.New("Bad Request: can't parse entities")
	bot.sendErrCount = 1

	ch := newChannel(bot, nil, zerolog.Nop())
	defer func() { _ = ch.Close() }()

	err := ch.Send(context.Background(), channel.Message{
		Content:  "✅ <b>Luce</b> spenta",
		Metadata: map[string]string{MetadataChatID: "42"},
	})
	if err != nil {
		t.Fatalf("expected fallback to succeed, got: %v", err)
	}

	sent := bot.snapshot()
	if len(sent) != 1 {
		t.Fatalf("expected 1 message (plain fallback), got %d", len(sent))
	}

	// Fallback should have no parse mode and stripped tags.
	if sent[0].ParseMode != "" {
		t.Errorf("expected empty parse mode for plain fallback, got %q", sent[0].ParseMode)
	}

	if strings.Contains(sent[0].Text, "<b>") {
		t.Errorf("plain fallback should have no HTML tags: %q", sent[0].Text)
	}
}

func TestChannel_Send_SanitizesHTML(t *testing.T) {
	bot := newMockBot()
	ch := newChannel(bot, nil, zerolog.Nop())
	defer func() { _ = ch.Close() }()

	// Send text with bare angle brackets that would break HTML mode.
	err := ch.Send(context.Background(), channel.Message{
		Content:  "temp 18 < 22 & <b>OK</b>",
		Metadata: map[string]string{MetadataChatID: "42"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	sent := bot.snapshot()
	if len(sent) != 1 {
		t.Fatalf("expected 1 message, got %d", len(sent))
	}

	want := "temp 18 &lt; 22 &amp; <b>OK</b>"
	if sent[0].Text != want {
		t.Errorf("expected sanitized text %q, got %q", want, sent[0].Text)
	}
}

func (m *mockBot) snapshotEdits() []tgbotapi.EditMessageTextConfig {
	m.mu.Lock()
	defer m.mu.Unlock()

	out := make([]tgbotapi.EditMessageTextConfig, len(m.sentEdits))
	copy(out, m.sentEdits)

	return out
}

func TestChannel_SendStreamMessage_FirstChunk(t *testing.T) {
	bot := newMockBot()
	bot.messageID = 123

	ch := newChannel(bot, nil, zerolog.Nop())
	defer func() { _ = ch.Close() }()

	msgID, err := ch.SendStreamMessage(context.Background(), 42, "stream-chunk")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if msgID != 123 {
		t.Errorf("expected messageID 123, got %d", msgID)
	}

	sent := bot.snapshot()
	if len(sent) != 1 {
		t.Fatalf("expected 1 message sent, got %d", len(sent))
	}

	if sent[0].ChatID != 42 {
		t.Errorf("expected chatID 42, got %d", sent[0].ChatID)
	}

	if sent[0].Text != "stream-chunk" {
		t.Errorf("expected text 'stream-chunk', got %q", sent[0].Text)
	}
}

func TestChannel_SendStreamEdit_UpdatesMessage(t *testing.T) {
	bot := newMockBot()
	bot.messageID = 456

	ch := newChannel(bot, nil, zerolog.Nop())
	defer func() { _ = ch.Close() }()

	err := ch.SendStreamEdit(context.Background(), 42, 456, "updated content")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	edits := bot.snapshotEdits()
	if len(edits) != 1 {
		t.Fatalf("expected 1 edit, got %d", len(edits))
	}

	if edits[0].MessageID != 456 {
		t.Errorf("expected messageID 456, got %d", edits[0].MessageID)
	}

	if edits[0].Text != "updated content" {
		t.Errorf("expected text 'updated content', got %q", edits[0].Text)
	}
}

func TestChannel_SendStreamEdit_EmptyContentNoop(t *testing.T) {
	bot := newMockBot()

	ch := newChannel(bot, nil, zerolog.Nop())
	defer func() { _ = ch.Close() }()

	err := ch.SendStreamEdit(context.Background(), 42, 1, "")
	if err != nil {
		t.Fatalf("unexpected error for empty content: %v", err)
	}

	edits := bot.snapshotEdits()
	if len(edits) != 0 {
		t.Errorf("expected 0 edits for empty content, got %d", len(edits))
	}
}

func TestChannel_SendStreamEdit_HTMLFallbackToPlainText(t *testing.T) {
	bot := newMockBot()
	bot.sendErr = errors.New("HTML rejected")
	bot.sendErrCount = 1 // only first Send fails

	ch := newChannel(bot, nil, zerolog.Nop())
	defer func() { _ = ch.Close() }()

	err := ch.SendStreamEdit(context.Background(), 42, 1, "<b>bold</b> text")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// The HTML edit fails (mock returns error on first call), so the code
	// retries as plain text. The mock only records successful sends, so we
	// expect 1 edit (the plain text fallback).
	edits := bot.snapshotEdits()
	if len(edits) != 1 {
		t.Fatalf("expected 1 edit (plain text fallback), got %d", len(edits))
	}

	// The recorded edit should be plain text (no ParseMode).
	if edits[0].ParseMode != "" {
		t.Errorf("expected plain text fallback with no ParseMode, got %q", edits[0].ParseMode)
	}

	// Verify the plain text has HTML stripped.
	if edits[0].Text != "bold text" {
		t.Errorf("expected stripped text 'bold text', got %q", edits[0].Text)
	}
}

func TestChannel_SendStreamEdit_TruncatesAt4096(t *testing.T) {
	bot := newMockBot()

	ch := newChannel(bot, nil, zerolog.Nop())
	defer func() { _ = ch.Close() }()

	long := strings.Repeat("a", MaxMessageLength+100)

	err := ch.SendStreamEdit(context.Background(), 42, 1, long)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	edits := bot.snapshotEdits()
	if len(edits) != 1 {
		t.Fatalf("expected 1 edit, got %d", len(edits))
	}

	if len([]rune(edits[0].Text)) > MaxMessageLength {
		t.Errorf("expected text truncated to %d chars, got %d", MaxMessageLength, len([]rune(edits[0].Text)))
	}
}

func TestChannel_SendStreamMessage_HTMLFallbackToPlainText(t *testing.T) {
	bot := newMockBot()
	bot.sendErr = errors.New("HTML rejected")
	bot.sendErrCount = 1

	ch := newChannel(bot, nil, zerolog.Nop())
	defer func() { _ = ch.Close() }()

	_, err := ch.SendStreamMessage(context.Background(), 42, "<i>italic</i>")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// The HTML send fails (mock returns error on first call), so the code
	// retries as plain text. The mock only records successful sends, so we
	// expect 1 send (the plain text fallback).
	sent := bot.snapshot()
	if len(sent) != 1 {
		t.Fatalf("expected 1 send (plain text fallback), got %d", len(sent))
	}

	if sent[0].ParseMode != "" {
		t.Errorf("expected plain text fallback with no ParseMode, got %q", sent[0].ParseMode)
	}

	if sent[0].Text != "italic" {
		t.Errorf("expected stripped text 'italic', got %q", sent[0].Text)
	}
}
