package telegram

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/rs/zerolog"
)

// reactingBot is a mockBot that also supports reactions.
type reactingBot struct {
	*mockBot
	mu    sync.Mutex
	calls []reactionCall
	err   error
}

type reactionCall struct {
	chatID    int64
	messageID int
	emoji     string
}

func (b *reactingBot) SetReaction(_ context.Context, chatID int64, messageID int, emoji string) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.calls = append(b.calls, reactionCall{chatID, messageID, emoji})

	return b.err
}

func TestTranslate_ExposesMessageID(t *testing.T) {
	ch := newChannel(newMockBot(), nil, zerolog.Nop())
	defer func() { _ = ch.Close() }()

	upd := chatUpdate(7, 42, chatPrivate, "alice", "hi")
	upd.Message.MessageID = 991

	msg := ch.translate(upd)
	if msg == nil || msg.Metadata[MetadataMessageID] != "991" {
		t.Fatalf("metadata = %+v", msg)
	}
}

func TestAcknowledgeSteer_RequestShape(t *testing.T) {
	bot := &reactingBot{mockBot: newMockBot()}
	ch := newChannel(bot, nil, zerolog.Nop())
	defer func() { _ = ch.Close() }()

	// A group chat (negative id) steers like a private chat.
	ch.AcknowledgeSteer(context.Background(), map[string]string{MetadataChatID: "-100123", MetadataMessageID: "55"})

	if len(bot.calls) != 1 || bot.calls[0] != (reactionCall{chatID: -100123, messageID: 55, emoji: "👀"}) {
		t.Fatalf("calls = %+v", bot.calls)
	}
}

func TestAcknowledgeSteer_FailureIsIsolated(t *testing.T) {
	bot := &reactingBot{mockBot: newMockBot(), err: errors.New("Bad Request: reactions are not allowed")}
	ch := newChannel(bot, nil, zerolog.Nop())
	defer func() { _ = ch.Close() }()

	// No panic, no error surfaced, nothing to handle: best-effort.
	ch.AcknowledgeSteer(context.Background(), map[string]string{MetadataChatID: "7", MetadataMessageID: "1"})

	if len(bot.calls) != 1 {
		t.Fatalf("calls = %d, want 1 attempt", len(bot.calls))
	}
}

func TestAcknowledgeSteer_SkipsWhenUnsupportedOrIncomplete(t *testing.T) {
	// A bot without reaction support: silently skipped.
	plain := newChannel(newMockBot(), nil, zerolog.Nop())
	defer func() { _ = plain.Close() }()

	plain.AcknowledgeSteer(context.Background(), map[string]string{MetadataChatID: "7", MetadataMessageID: "1"})

	bot := &reactingBot{mockBot: newMockBot()}
	ch := newChannel(bot, nil, zerolog.Nop())
	defer func() { _ = ch.Close() }()

	for _, md := range []map[string]string{
		{MetadataChatID: "7"},
		{MetadataMessageID: "1"},
		{MetadataChatID: "x", MetadataMessageID: "1"},
		{MetadataChatID: "7", MetadataMessageID: "0"},
	} {
		ch.AcknowledgeSteer(context.Background(), md)
	}

	if len(bot.calls) != 0 {
		t.Fatalf("calls = %+v, want none for incomplete metadata", bot.calls)
	}
}
