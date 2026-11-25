package tools

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/marco-rozz/ready/internal/channel"
)

func runSay(ctx context.Context, t *testing.T, text string) (string, error) {
	t.Helper()

	args := `{"text":` + jsonString(text) + `}`

	return NewSayTool().InvokableRun(ctx, args)
}

func jsonString(s string) string {
	b, _ := json.Marshal(s)

	return string(b)
}

func deliverer(res channel.NoticeResult, err error, got *string) NoticeDeliverer {
	return func(_ context.Context, text string) (channel.NoticeResult, error) {
		if got != nil {
			*got = text
		}

		return res, err
	}
}

func TestSay_Outcomes(t *testing.T) {
	cases := []struct {
		name string
		res  channel.NoticeResult
		want string
	}{
		{"delivered", channel.NoticeResult{Outcome: channel.NoticeDelivered}, "delivered:"},
		{"partial", channel.NoticeResult{Outcome: channel.NoticePartial, Parts: 2, DeliveredParts: 1}, "partial: 1 of 2"},
		{"no_connection", channel.NoticeResult{Outcome: channel.NoticeNotShown, Reason: channel.NoticeReasonNoConnection}, "not_shown (no_connection)"},
		{"client", channel.NoticeResult{Outcome: channel.NoticeNotShown, Reason: channel.NoticeReasonClientDoesNotRender}, "not_shown (client_does_not_render)"},
		{"unsupported", channel.NoticeResult{Outcome: channel.NoticeNotShown, Reason: channel.NoticeReasonChannelUnsupported}, "not_shown (channel_unsupported)"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var got string
			ctx := WithNoticeDeliverer(context.Background(), deliverer(tc.res, nil, &got))

			out, err := runSay(ctx, t, "  <b>hi</b>\n")
			if err != nil || !strings.HasPrefix(out, tc.want) {
				t.Fatalf("out=%q err=%v, want prefix %q", out, err, tc.want)
			}

			if got != "  <b>hi</b>\n" {
				t.Fatalf("text must reach the deliverer unmodified, got %q", got)
			}
		})
	}
}

func TestSay_NoDeliverer(t *testing.T) {
	out, err := runSay(context.Background(), t, "x")
	if err != nil || !strings.HasPrefix(out, "not_shown (no_channel)") {
		t.Fatalf("out=%q err=%v", out, err)
	}
}

func TestSay_RefusalsNeverReachDeliverer(t *testing.T) {
	called := false
	ctx := WithNoticeDeliverer(context.Background(), func(context.Context, string) (channel.NoticeResult, error) {
		called = true

		return channel.NoticeResult{Outcome: channel.NoticeDelivered}, nil
	})

	for name, text := range map[string]string{
		"empty":              "",
		"whitespace":         " \n\t ",
		"over cap":           strings.Repeat("a", MaxNoticeChars+1),
		"over cap multibyte": strings.Repeat("è", MaxNoticeChars+1),
	} {
		if _, err := runSay(ctx, t, text); !errors.Is(err, ErrNoticeRefused) {
			t.Errorf("%s: want ErrNoticeRefused, got %v", name, err)
		}
	}

	if called {
		t.Fatal("a refused text reached the deliverer")
	}

	// Exactly at the cap (in code points, not bytes) is accepted.
	if out, err := runSay(ctx, t, strings.Repeat("è", MaxNoticeChars)); err != nil || !strings.HasPrefix(out, "delivered") {
		t.Fatalf("cap boundary: out=%q err=%v", out, err)
	}
}

func TestSay_DeliveryFailure(t *testing.T) {
	ctx := WithNoticeDeliverer(context.Background(), deliverer(channel.NoticeResult{}, errors.New("secret-token-in-url"), nil))

	out, err := runSay(ctx, t, "x")
	if !errors.Is(err, ErrNoticeDeliveryFailed) || out != "" {
		t.Fatalf("out=%q err=%v", out, err)
	}

	if strings.Contains(err.Error(), "secret-token-in-url") {
		t.Fatalf("transport cause must not reach the model: %v", err)
	}
}

func TestSay_Stats(t *testing.T) {
	stats := &NoticeStats{}
	ctx := WithNoticeStats(WithNoticeDeliverer(context.Background(), deliverer(channel.NoticeResult{Outcome: channel.NoticeDelivered}, nil, nil)), stats)

	_, _ = runSay(ctx, t, "a")
	_, _ = runSay(ctx, t, "b")
	_, _ = runSay(ctx, t, "")

	got := stats.Counts()
	if got["delivered"] != 2 || got[NoticeStatRefused] != 1 {
		t.Fatalf("counts = %v", got)
	}
}

func TestSay_DescriptionRules(t *testing.T) {
	info, err := NewSayTool().Info(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	for _, want := range []string{"NEVER shown", "final answer", "3500", "not_shown", "partial", "delivered", "retry at most once"} {
		if !strings.Contains(info.Desc, want) {
			t.Errorf("description lacks %q", want)
		}
	}
}
