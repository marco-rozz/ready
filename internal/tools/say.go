package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"

	"github.com/marco-rozz/ready/internal/channel"
)

// MaxNoticeChars is the length cap of one Mid-turn Notice in Unicode code
// points. It leaves headroom under Telegram's 4096-character message limit
// after entity escaping, so a Notice is one message in practice.
const MaxNoticeChars = 3500

var (
	// ErrNoticeRefused is returned when the text is empty or over the cap
	// (classified Permanent by the error digester).
	ErrNoticeRefused = errors.New("notice refused")
	// ErrNoticeDeliveryFailed is returned when the transport failed before
	// any part was accepted, or the delivery timed out (classified
	// Transient).
	ErrNoticeDeliveryFailed = errors.New("notice delivery failed")
)

// Compile-time assertion that SayTool implements InvokableTool.
var _ tool.InvokableTool = (*SayTool)(nil)

// NoticeDeliverer shows one Notice on the channel of the current Turn. It is
// carried in the Turn context (WithNoticeDeliverer); the daemon wires the
// redaction, per-address serialisation, detached timeout and channel lookup
// behind it. A non-nil error means nothing was shown (transport failure or
// timeout).
type NoticeDeliverer func(ctx context.Context, text string) (channel.NoticeResult, error)

type noticeDelivererKey struct{}

// WithNoticeDeliverer returns a context carrying d for the say tool.
func WithNoticeDeliverer(ctx context.Context, d NoticeDeliverer) context.Context {
	return context.WithValue(ctx, noticeDelivererKey{}, d)
}

// NoticeStats counts the say calls of one Turn by outcome (DEBUG
// observability). Safe for concurrent use: one tool-call group runs its
// calls concurrently.
type NoticeStats struct {
	mu     sync.Mutex
	counts map[string]int
}

// Counts returns a copy of the per-outcome counters.
func (s *NoticeStats) Counts() map[string]int {
	s.mu.Lock()
	defer s.mu.Unlock()

	out := make(map[string]int, len(s.counts))
	for k, v := range s.counts {
		out[k] = v
	}

	return out
}

func (s *NoticeStats) add(outcome string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.counts == nil {
		s.counts = make(map[string]int)
	}

	s.counts[outcome]++
}

type noticeStatsKey struct{}

// WithNoticeStats returns a context whose say calls are counted into s.
func WithNoticeStats(ctx context.Context, s *NoticeStats) context.Context {
	return context.WithValue(ctx, noticeStatsKey{}, s)
}

// Outcome keys recorded in NoticeStats.
const (
	NoticeStatRefused = "refused"
	NoticeStatFailed  = "failed"
)

// sayArgs is the input structure of the say tool.
type sayArgs struct {
	Text string `json:"text"`
}

// SayTool implements the `say` builtin: it shows its text argument to the
// user immediately, mid-Turn, without ending the Turn (a Mid-turn Notice).
type SayTool struct{}

// NewSayTool creates the say tool.
func NewSayTool() *SayTool {
	return &SayTool{}
}

// Info returns the tool metadata for the LLM.
func (t *SayTool) Info(_ context.Context) (*schema.ToolInfo, error) {
	return &schema.ToolInfo{
		Name: "say",
		Desc: "Show a text to the user right now, in the middle of your work, without ending the turn. " +
			"Text you write between tool calls is NEVER shown to the user: only this tool and your final answer reach them, " +
			"so use `say` whenever the user should see something before you finish (for example each subagent report as it arrives). " +
			"You must still end the turn with a final answer: do not repeat in it what a delivered notice already showed verbatim, " +
			"and include in it anything whose notice was not shown. " +
			fmt.Sprintf("Use one `say` per step, under %d characters, written in the channel's reply format. ", MaxNoticeChars) +
			"The result tells you what happened: `delivered` (shown; do not repeat it verbatim), " +
			"`partial` (only some parts shown; do not resend them, put the rest in the final answer), " +
			"`not_shown` (no client could show it; put it in the final answer if it matters), " +
			"or an error (refused text: shorten it or use the final answer; delivery failure: nothing was shown, retry at most once).",
		ParamsOneOf: schema.NewParamsOneOfByParams(map[string]*schema.ParameterInfo{
			"text": {
				Type:     schema.String,
				Desc:     fmt.Sprintf("The text to show to the user now (non-empty, at most %d characters).", MaxNoticeChars),
				Required: true,
			},
		}),
	}, nil
}

// InvokableRun validates the text, delivers it through the context's
// deliverer and returns the outcome text.
func (t *SayTool) InvokableRun(ctx context.Context, argumentsInJSON string, _ ...tool.Option) (string, error) {
	stats, _ := ctx.Value(noticeStatsKey{}).(*NoticeStats)
	record := func(outcome string) {
		if stats != nil {
			stats.add(outcome)
		}
	}

	var args sayArgs
	if err := json.Unmarshal([]byte(argumentsInJSON), &args); err != nil {
		record(NoticeStatRefused)

		return "", fmt.Errorf("%w: arguments must be a JSON object with a text string; put the content in your final answer instead", ErrNoticeRefused)
	}

	if strings.TrimSpace(args.Text) == "" {
		record(NoticeStatRefused)

		return "", fmt.Errorf("%w: text is empty; nothing was shown. Provide the text to show, or put the content in your final answer", ErrNoticeRefused)
	}

	if n := utf8.RuneCountInString(args.Text); n > MaxNoticeChars {
		record(NoticeStatRefused)

		return "", fmt.Errorf("%w: text is %d characters, over the %d-character limit; nothing was shown. Shorten it, split it across several `say` calls, or put the content in your final answer", ErrNoticeRefused, n, MaxNoticeChars)
	}

	deliver, _ := ctx.Value(noticeDelivererKey{}).(NoticeDeliverer)
	if deliver == nil {
		record(string(channel.NoticeNotShown))

		return notShownText(channel.NoticeReasonNoChannel), nil
	}

	res, err := deliver(ctx, args.Text)
	if err != nil {
		record(NoticeStatFailed)

		return "", fmt.Errorf("%w: nothing was shown to the user. Retry at most once; otherwise put the content in your final answer", ErrNoticeDeliveryFailed)
	}

	record(string(res.Outcome))

	switch res.Outcome {
	case channel.NoticeDelivered:
		return "delivered: the notice was shown to the user. Do not repeat it verbatim in your final answer.", nil
	case channel.NoticePartial:
		return fmt.Sprintf("partial: %d of %d parts of the notice were shown to the user. Do not resend the delivered parts; include the remainder in your final answer.", res.DeliveredParts, res.Parts), nil
	case channel.NoticeNotShown:
		return notShownText(res.Reason), nil
	default:
		return notShownText(channel.NoticeReasonNoChannel), nil
	}
}

func notShownText(reason string) string {
	return fmt.Sprintf("not_shown (%s): nothing was shown to the user because no client can display it. Include the content in your final answer if it matters.", reason)
}
