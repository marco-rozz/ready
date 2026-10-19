// Package session owns the conversation Session lifecycle
// (the session-lifecycle, the architecture decision records): Conversation addresses,
// the per-Session file layout and current-Session resolution, Session end
// with its closing record, reset rules, and retention of ended Sessions.
//
// Vocabulary (CONTEXT.md): an ADDRESS (`cli`, `telegram-<chat_id>`) is the
// stable conversation endpoint; it owns one current SESSION (UUIDv7) plus
// ended ones; a CONNECTION (a ZMQ handshake) is orthogonal and its expiry
// never ends a Session.
package session

import (
	"strings"

	"github.com/marco-rozz/ready/internal/channel"
	"github.com/marco-rozz/ready/internal/channel/telegram"
)

// Address is a Conversation address: the bare channel name for single-tenant
// channels (`cli`, `serial`) and `<channel>-<peer>` for multi-tenant ones
// (`telegram-<chat_id>`). The stdin CLI and the ZMQ CLI share `cli`.
type Address string

// AddressOf maps an incoming message to its Conversation address. Each
// multi-tenant channel contributes its peer key through the metadata it
// already sets (Telegram: the chat id).
func AddressOf(msg channel.Message) Address {
	if msg.ChannelName == telegram.ChannelName {
		if id := msg.Metadata[telegram.MetadataChatID]; id != "" {
			return Address(msg.ChannelName + "-" + id)
		}
	}

	return Address(msg.ChannelName)
}

// Route recovers the channel name and delivery metadata of an address, for
// outbound messages not triggered by an incoming one.
func Route(a Address) (channelName string, metadata map[string]string) {
	if chatID, ok := strings.CutPrefix(string(a), telegram.ChannelName+"-"); ok && chatID != "" {
		return telegram.ChannelName, map[string]string{telegram.MetadataChatID: chatID}
	}

	return string(a), nil
}

// DirName returns the address sanitised for use as a directory name.
func (a Address) DirName() string {
	return strings.NewReplacer(
		"/", "_",
		"\\", "_",
		"..", "_",
		" ", "_",
	).Replace(string(a))
}
