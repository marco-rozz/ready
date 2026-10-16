package telegram

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/rs/zerolog"
)

// commandBot is a mockBot that also implements commandRegistrar.
type commandBot struct {
	*mockBot

	mu       sync.Mutex
	commands []tgbotapi.BotCommand
	setErr   error
	set      chan struct{}
}

func newCommandBot(setErr error) *commandBot {
	return &commandBot{mockBot: newMockBot(), setErr: setErr, set: make(chan struct{}, 1)}
}

func (b *commandBot) BotUserName() string { return "ReadyBot" }

func (b *commandBot) SetCommands(_ context.Context, commands []tgbotapi.BotCommand) error {
	b.mu.Lock()
	b.commands = commands
	b.mu.Unlock()

	b.set <- struct{}{}

	return b.setErr
}

func receive(t *testing.T, ch *Channel) string {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	msg, err := ch.Receive(ctx)
	if err != nil {
		t.Fatalf("Receive: %v", err)
	}

	return msg.Content
}

// session-lifecycle 5.3: the Session commands are registered at start.
func TestChannel_RegistersSessionCommands(t *testing.T) {
	bot := newCommandBot(nil)
	ch := newChannel(bot, nil, zerolog.Nop())
	defer func() { _ = ch.Close() }()

	select {
	case <-bot.set:
	case <-time.After(time.Second):
		t.Fatal("setMyCommands was not called")
	}

	bot.mu.Lock()
	defer bot.mu.Unlock()

	if len(bot.commands) != 2 || bot.commands[0].Command != "new" || bot.commands[1].Command != "compact" {
		t.Errorf("registered commands = %+v", bot.commands)
	}
}

// A registration failure is a WARN and the channel keeps delivering.
func TestChannel_CommandRegistrationFailureIsNotFatal(t *testing.T) {
	var logs bytes.Buffer
	bot := newCommandBot(errors.New("forbidden"))
	ch := newChannel(bot, nil, zerolog.New(&logs))
	defer func() { _ = ch.Close() }()

	<-bot.set
	bot.updates <- textUpdate(1, 1, "alice", "ciao")

	if got := receive(t, ch); got != "ciao" {
		t.Fatalf("got %q", got)
	}

	if !strings.Contains(logs.String(), "failed to register the session commands") {
		t.Errorf("expected a WARN, got %s", logs.String())
	}
}

const newCommand = "/new"

func TestChannel_StripsBotMentionFromCommands(t *testing.T) {
	bot := newCommandBot(nil)
	ch := newChannel(bot, nil, zerolog.Nop())
	defer func() { _ = ch.Close() }()

	cases := map[string]string{
		"/new@ReadyBot":                   newCommand,
		"/compact@readybot keep the plan": "/compact keep the plan",
		"/new@OtherBot":                   "/new@OtherBot",
		"hello @ReadyBot":                 "hello @ReadyBot",
		newCommand:                        newCommand,
	}

	for in, want := range cases {
		bot.updates <- textUpdate(1, 1, "alice", in)

		if got := receive(t, ch); got != want {
			t.Errorf("%q → %q, want %q", in, got, want)
		}
	}
}
