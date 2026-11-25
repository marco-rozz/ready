package zmqcli

import (
	"context"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/marco-rozz/ready/internal/channel"
	"github.com/marco-rozz/ready/internal/protocol"
)

// TestIntegration_MessageOriginStamping pins that a conversational `message`
// is human-originated and an `exec` is scripted (turn-steering S1).
func TestIntegration_MessageOriginStamping(t *testing.T) {
	for _, tc := range []struct {
		name string
		typ  protocol.RequestType
		want channel.Origin
	}{
		{"message", protocol.RequestTypeMessage, channel.OriginHuman},
		{"exec", protocol.RequestTypeExec, channel.OriginScripted},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			ch, addr := newIntegrationChannel(ctx, t, "secret-origin", 0, zerolog.Nop())

			got := make(chan channel.Origin, 1)

			go func() {
				msg, err := ch.Receive(ctx)
				if err == nil {
					got <- msg.Origin
				}
			}()

			id := "origin-" + tc.name
			dealer := dialStatusDealer(ctx, t, addr, id)
			doHandshake(t, dealer, id, "secret-origin")
			sendDealerRequest(t, dealer, protocol.Request{
				ConnectionID: id,
				Type:         tc.typ,
				RequestID:    "req-" + tc.name,
				Content:      "hi",
				Timestamp:    testTimestamp,
			})

			select {
			case o := <-got:
				if o != tc.want {
					t.Errorf("origin = %s, want %s", o, tc.want)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("timed out waiting for the surfaced message")
			}
		})
	}
}
