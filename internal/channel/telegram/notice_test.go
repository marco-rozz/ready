package telegram

import (
	"context"
	"errors"
	"strings"
	"testing"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/rs/zerolog"

	"github.com/marco-rozz/ready/internal/channel"
)

func TestSendNotice_DeliveredKeepsHTMLAndCreatesNewMessage(t *testing.T) {
	bot := newMockBot()
	ch := newChannel(bot, nil, zerolog.Nop())

	defer func() { _ = ch.Close() }()

	res, err := ch.SendNotice(context.Background(), map[string]string{MetadataChatID: "42"}, "<b>Report 1</b>: ok & <script>x</script>")
	if err != nil || res.Outcome != channel.NoticeDelivered || res.Parts != 1 || res.DeliveredParts != 1 {
		t.Fatalf("res=%+v err=%v", res, err)
	}

	sent := bot.snapshot()
	if len(sent) != 1 || !strings.Contains(sent[0].Text, "<b>Report 1</b>") || strings.Contains(sent[0].Text, "<script>") {
		t.Fatalf("notice must go through the reply sanitiser, got %+v", sent)
	}

	if len(bot.snapshotEdits()) != 0 {
		t.Fatal("a notice never edits a message")
	}
}

func TestSendNotice_SecondPartFailsIsPartial(t *testing.T) {
	// Two parts: the second Send fails with a non-parse error.
	failOnSecond := &failSecondBot{mockBot: newMockBot()}
	ch2 := newChannel(failOnSecond, nil, zerolog.Nop())

	defer func() { _ = ch2.Close() }()

	text := strings.Repeat("a", MaxMessageLength-10) + "\n" + strings.Repeat("b", 50)

	res, err := ch2.SendNotice(context.Background(), map[string]string{MetadataChatID: "42"}, text)
	if err != nil || res.Outcome != channel.NoticePartial || res.Parts != 2 || res.DeliveredParts != 1 {
		t.Fatalf("res=%+v err=%v", res, err)
	}
}

func TestSendNotice_FirstPartFailsIsError(t *testing.T) {
	bot := newMockBot()
	bot.sendErr = errors.New("network down")

	ch := newChannel(bot, nil, zerolog.Nop())

	defer func() { _ = ch.Close() }()

	if res, err := ch.SendNotice(context.Background(), map[string]string{MetadataChatID: "42"}, "hello"); err == nil {
		t.Fatalf("nothing shown must be an error, got %+v", res)
	}
}

func TestSendNotice_MissingChatID(t *testing.T) {
	ch := newChannel(newMockBot(), nil, zerolog.Nop())

	defer func() { _ = ch.Close() }()

	if _, err := ch.SendNotice(context.Background(), nil, "x"); !errors.Is(err, ErrMissingChatID) {
		t.Fatalf("err=%v", err)
	}
}

// failSecondBot delegates to the mock but fails every Send after the first.
type failSecondBot struct {
	*mockBot
	calls int
}

func (b *failSecondBot) Send(ctx context.Context, c tgbotapi.Chattable) (tgbotapi.Message, error) {
	b.calls++
	if b.calls > 1 {
		return tgbotapi.Message{}, errors.New("network down")
	}

	b.mu.Lock()
	b.sendErr = nil
	b.mu.Unlock()

	return b.mockBot.Send(ctx, c)
}
