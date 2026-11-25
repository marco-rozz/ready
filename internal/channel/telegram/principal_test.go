package telegram

import (
	"testing"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/rs/zerolog"

	"github.com/marco-rozz/ready/internal/channel"
)

const chatPrivate = "private"

func chatUpdate(chatID, userID int64, chatType, username, text string) tgbotapi.Update {
	return tgbotapi.Update{
		UpdateID: 1,
		Message: &tgbotapi.Message{
			Chat: &tgbotapi.Chat{ID: chatID, Type: chatType},
			From: &tgbotapi.User{ID: userID, UserName: username},
			Text: text,
		},
	}
}

func TestTranslate_ExposesUserIDAndChatType(t *testing.T) {
	ch := newChannel(newMockBot(), nil, zerolog.Nop())
	defer func() { _ = ch.Close() }()

	msg := ch.translate(chatUpdate(-100, 42, "supergroup", "alice", "hi"))
	if msg == nil || msg.Metadata[MetadataUserID] != "42" || msg.Metadata[MetadataChatType] != "supergroup" || msg.Metadata[MetadataChatID] != "-100" {
		t.Fatalf("metadata = %+v", msg)
	}

	noSender := chatUpdate(7, 0, chatPrivate, "", "hi")
	noSender.Message.From = nil
	msg = ch.translate(noSender)
	if msg == nil {
		t.Fatal("message without sender should still be translated when the whitelist is empty")
	}
	if _, ok := msg.Metadata[MetadataUserID]; ok {
		t.Fatalf("user_id must be absent without a sender: %+v", msg.Metadata)
	}
}

func TestVerifyPrincipal(t *testing.T) {
	cases := []struct {
		name  string
		allow []string
		md    map[string]string
		want  bool
	}{
		{"private chat, numeric user id listed", []string{"42"}, map[string]string{MetadataUserID: "42", MetadataChatType: chatPrivate}, true},
		{"empty allow_from never qualifies", nil, map[string]string{MetadataUserID: "42", MetadataChatType: chatPrivate}, false},
		{"whitelisted group chat id", []string{"-100"}, map[string]string{MetadataChatID: "-100", MetadataUserID: "42", MetadataChatType: "group"}, false},
		{"member of a group whose user id is listed", []string{"42"}, map[string]string{MetadataUserID: "42", MetadataChatType: "group"}, false},
		{"username only", []string{"@marco"}, map[string]string{MetadataUserID: "42", MetadataChatType: chatPrivate}, false},
		{"chat id equal but user id not listed", []string{"7"}, map[string]string{MetadataChatID: "7", MetadataUserID: "42", MetadataChatType: chatPrivate}, false},
		{"no sender", []string{"42"}, map[string]string{MetadataChatType: chatPrivate}, false},
		{"non numeric user id", []string{"abc"}, map[string]string{MetadataUserID: "abc", MetadataChatType: chatPrivate}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ch := newChannel(newMockBot(), c.allow, zerolog.Nop())
			defer func() { _ = ch.Close() }()
			if got := ch.VerifyPrincipal(c.md); got != c.want {
				t.Fatalf("VerifyPrincipal = %v, want %v", got, c.want)
			}
		})
	}
}

func TestTranslate_StampsHumanOrigin(t *testing.T) {
	ch := newChannel(newMockBot(), nil, zerolog.Nop())
	defer func() { _ = ch.Close() }()

	msg := ch.translate(chatUpdate(7, 42, chatPrivate, "alice", "hello there"))
	if msg == nil || msg.Origin != channel.OriginHuman {
		t.Fatalf("origin = %+v, want human", msg)
	}
}
