package usage

import (
	"context"
	"errors"
	"io"
	"sync"
	"testing"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
)

// fakeChatModel is a scripted ToolCallingChatModel whose Generate returns
// one message carrying the given usage.
type fakeChatModel struct {
	usage *schema.TokenUsage
}

func (f *fakeChatModel) Generate(_ context.Context, _ []*schema.Message, _ ...model.Option) (*schema.Message, error) {
	msg := &schema.Message{Role: schema.Assistant, Content: "ok"}
	if f.usage != nil {
		msg.ResponseMeta = &schema.ResponseMeta{Usage: f.usage}
	}

	return msg, nil
}

func (f *fakeChatModel) Stream(_ context.Context, _ []*schema.Message, _ ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	chunks := make([]chunkOrErr, 0, 3)
	if f.usage != nil {
		// Simulate the provider shape: intermediate chunks carry no usage,
		// the final chunk carries the full totals.
		chunks = append(chunks, chunkOrErr{chunk: &schema.Message{Content: "hel"}})
		chunks = append(chunks, chunkOrErr{chunk: &schema.Message{Content: "lo"}})
		chunks = append(chunks, chunkOrErr{chunk: &schema.Message{ResponseMeta: &schema.ResponseMeta{Usage: f.usage}}})
	} else {
		chunks = append(chunks, chunkOrErr{chunk: &schema.Message{Content: "no-usage"}})
	}

	sr, sw := schema.Pipe[*schema.Message](len(chunks))
	for _, c := range chunks {
		sw.Send(c.chunk, c.err)
	}
	sw.Close()

	return sr, nil
}

func (f *fakeChatModel) WithTools(_ []*schema.ToolInfo) (model.ToolCallingChatModel, error) {
	return f, nil
}

type chunkOrErr struct {
	chunk *schema.Message
	err   error
}

// fakeAgenticModel mirrors fakeChatModel for the AgenticModel interface.
type fakeAgenticModel struct {
	usage *schema.TokenUsage
}

func (f *fakeAgenticModel) Generate(_ context.Context, _ []*schema.AgenticMessage, _ ...model.Option) (*schema.AgenticMessage, error) {
	msg := &schema.AgenticMessage{Role: schema.AgenticRoleTypeAssistant}
	if f.usage != nil {
		msg.ResponseMeta = &schema.AgenticResponseMeta{TokenUsage: f.usage}
	}

	return msg, nil
}

func (f *fakeAgenticModel) Stream(_ context.Context, _ []*schema.AgenticMessage, _ ...model.Option) (*schema.StreamReader[*schema.AgenticMessage], error) {
	sr, sw := schema.Pipe[*schema.AgenticMessage](2)
	sw.Send(&schema.AgenticMessage{ContentBlocks: nil}, nil)
	if f.usage != nil {
		sw.Send(&schema.AgenticMessage{ResponseMeta: &schema.AgenticResponseMeta{TokenUsage: f.usage}}, nil)
	}
	sw.Close()

	return sr, nil
}

func tok(prompt, cached, completion int) schema.TokenUsage {
	return schema.TokenUsage{
		PromptTokens:     prompt,
		CompletionTokens: completion,
		TotalTokens:      prompt + completion,
		PromptTokenDetails: schema.PromptTokenDetails{
			CachedTokens: cached,
		},
	}
}

func TestAccumulatorAttribution(t *testing.T) {
	store := NewStore()
	acc := store.Accumulator("cli")
	ctx := WithAccumulator(context.Background(), acc)

	rec := NewRecordedChatModel(&fakeChatModel{usage: usagePtr(tok(100, 40, 50))})
	if _, err := rec.Generate(ctx, nil); err != nil {
		t.Fatalf("generate: %v", err)
	}

	got := acc.Snapshot()
	if got == nil {
		t.Fatal("expected totals after attributed call")
	}
	if got.ReadTokens != 100 || got.WrittenTokens != 50 || got.CachedTokens != 40 {
		t.Errorf("totals = %+v, want read=100 written=50 cached=40", got)
	}
	if !got.HasCached {
		t.Error("expected HasCached=true when cached tokens reported")
	}
	if got.LLMCalls != 1 || got.AuxCalls != 0 || got.TurnCount != 0 {
		t.Errorf("counters = %+v, want llm=1 aux=0 turns=0", got)
	}
	if got.LastMainPromptTokens != 100 {
		t.Errorf("LastMainPromptTokens = %d, want 100", got.LastMainPromptTokens)
	}
}

func TestStreamRecordsLastNonNilUsage(t *testing.T) {
	store := NewStore()
	acc := store.Accumulator("cli")
	ctx := WithAccumulator(context.Background(), acc)

	rec := NewRecordedChatModel(&fakeChatModel{usage: usagePtr(tok(200, 0, 30))})
	sr, err := rec.Stream(ctx, nil)
	if err != nil {
		t.Fatalf("stream: %v", err)
	}

	for {
		if _, err := sr.Recv(); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			t.Fatalf("recv: %v", err)
		}
	}

	got := acc.Snapshot()
	if got == nil {
		t.Fatal("expected totals after stream")
	}
	if got.ReadTokens != 200 || got.WrittenTokens != 30 {
		t.Errorf("totals = %+v, want read=200 written=30", got)
	}
	if got.HasCached {
		t.Error("expected HasCached=false with zero cached tokens")
	}
	if got.LLMCalls != 1 {
		t.Errorf("LLMCalls = %d, want 1", got.LLMCalls)
	}
}

func TestAgenticAttribution(t *testing.T) {
	store := NewStore()
	acc := store.Accumulator("cli")
	ctx := WithAccumulator(context.Background(), acc)

	rec := NewRecordedAgenticModel(&fakeAgenticModel{usage: usagePtr(tok(300, 10, 20))})
	if _, err := rec.Generate(ctx, nil); err != nil {
		t.Fatalf("generate: %v", err)
	}

	sr, err := rec.Stream(ctx, nil)
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	for {
		if _, err := sr.Recv(); err != nil {
			break
		}
	}

	got := acc.Snapshot()
	if got == nil {
		t.Fatal("expected totals")
	}
	// Generate (300/20) + stream-final (300/20) = 600/40, cached 10+10=20.
	if got.ReadTokens != 600 || got.WrittenTokens != 40 || got.CachedTokens != 20 {
		t.Errorf("totals = %+v, want read=600 written=40 cached=20", got)
	}
	if got.LLMCalls != 2 {
		t.Errorf("LLMCalls = %d, want 2", got.LLMCalls)
	}
}

func TestProbeRecordsNothing(t *testing.T) {
	store := NewStore()
	_ = store.Accumulator("cli") // session exists but the probe ctx carries no accumulator

	rec := NewRecordedChatModel(&fakeChatModel{usage: usagePtr(tok(500, 0, 100))})
	if _, err := rec.Generate(context.Background(), nil); err != nil {
		t.Fatalf("probe generate: %v", err)
	}

	sr, err := rec.Stream(context.Background(), nil)
	if err != nil {
		t.Fatalf("probe stream: %v", err)
	}
	for {
		if _, err := sr.Recv(); err != nil {
			break
		}
	}

	if got := store.Snapshot("cli"); got != nil {
		t.Errorf("probe must record nothing, got %+v", got)
	}
}

func TestAuxAttribution(t *testing.T) {
	store := NewStore()
	acc := store.Accumulator("cli")
	ctx := WithAux(WithAccumulator(context.Background(), acc))

	rec := NewRecordedChatModel(&fakeChatModel{usage: usagePtr(tok(80, 0, 12))})
	if _, err := rec.Generate(ctx, nil); err != nil {
		t.Fatalf("aux generate: %v", err)
	}

	got := acc.Snapshot()
	if got.AuxCalls != 1 || got.LLMCalls != 0 {
		t.Errorf("counters = %+v, want aux=1 llm=0", got)
	}
	if got.ReadTokens != 80 {
		t.Errorf("read = %d, want 80 (aux tokens still accumulate)", got.ReadTokens)
	}
}

func TestFailoverRetryAddsUp(t *testing.T) {
	store := NewStore()
	acc := store.Accumulator("cli")

	// Two candidates, one turn, one accumulator: both attempts' usage adds.
	ctx1 := WithAccumulator(context.Background(), acc)
	acc2 := store.Accumulator("cli")
	if acc2 != acc {
		t.Fatal("accumulators for the same key must be the same handle")
	}
	ctx2 := WithAccumulator(context.Background(), acc2)

	r1 := NewRecordedChatModel(&fakeChatModel{usage: usagePtr(tok(1000, 0, 50))})
	r2 := NewRecordedAgenticModel(&fakeAgenticModel{usage: usagePtr(tok(1200, 0, 60))})
	_, _ = r1.Generate(ctx1, nil)
	_, _ = r2.Generate(ctx2, nil)

	acc.AddTurn()

	got := acc.Snapshot()
	if got.ReadTokens != 2200 || got.WrittenTokens != 110 {
		t.Errorf("totals = %+v, want read=2200 written=110", got)
	}
	if got.TurnCount != 1 || got.LLMCalls != 2 {
		t.Errorf("counters = %+v, want turns=1 llm=2", got)
	}
}

func TestReActMultiIterationTotals(t *testing.T) {
	store := NewStore()
	acc := store.Accumulator("cli")
	ctx := WithAccumulator(context.Background(), acc)

	rec := NewRecordedChatModel(&fakeChatModel{})
	rec.base = &scriptedModel{usages: []schema.TokenUsage{tok(100, 0, 10), tok(300, 0, 10), tok(400, 0, 50)}}

	for range 3 {
		if _, err := rec.Generate(ctx, nil); err != nil {
			t.Fatalf("iteration generate: %v", err)
		}
	}
	acc.AddTurn()

	got := acc.Snapshot()
	if got.ReadTokens != 800 || got.WrittenTokens != 70 {
		t.Errorf("totals = %+v, want read=800 written=70", got)
	}
	if got.LLMCalls != 3 || got.TurnCount != 1 {
		t.Errorf("counters = %+v, want llm=3 turns=1", got)
	}
	if got.LastMainPromptTokens != 400 {
		t.Errorf("LastMainPromptTokens = %d, want 400 (last turn's real prompt)", got.LastMainPromptTokens)
	}
}

// scriptedModel serves a fixed sequence of usage observations, one per call.
type scriptedModel struct {
	usages []schema.TokenUsage
	n      int
	mu     sync.Mutex
}

func (s *scriptedModel) Generate(_ context.Context, _ []*schema.Message, _ ...model.Option) (*schema.Message, error) {
	s.mu.Lock()
	u := s.usages[min(s.n, len(s.usages)-1)]
	s.n++
	s.mu.Unlock()

	return &schema.Message{Role: schema.Assistant, ResponseMeta: &schema.ResponseMeta{Usage: &u}}, nil
}

func (s *scriptedModel) Stream(ctx context.Context, input []*schema.Message, opts ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	msg, _ := s.Generate(ctx, input, opts...)
	sr, sw := schema.Pipe[*schema.Message](1)
	sw.Send(msg, nil)
	sw.Close()

	return sr, nil
}

func (s *scriptedModel) WithTools(_ []*schema.ToolInfo) (model.ToolCallingChatModel, error) {
	return s, nil
}

func TestSnapshotNilWhenFresh(t *testing.T) {
	store := NewStore()
	if got := store.Snapshot("never-seen"); got != nil {
		t.Errorf("fresh key must snapshot nil, got %+v", got)
	}

	acc := store.Accumulator("fresh")
	if got := acc.Snapshot(); got != nil {
		t.Errorf("accumulator with no data must snapshot nil, got %+v", got)
	}
}

func TestReconnectContinuesTotalsAndRestartResets(t *testing.T) {
	store := NewStore()
	acc := store.Accumulator("cli")
	acc.AddTurn()

	if got := store.Snapshot("cli"); got == nil || got.TurnCount != 1 {
		t.Fatalf("pre-reconnect totals = %+v, want turns=1", got)
	}

	// Reconnect: the same store handle yields the same accumulator.
	acc2 := store.Accumulator("cli")
	acc2.AddTurn()
	if got := store.Snapshot("cli"); got == nil || got.TurnCount != 2 {
		t.Fatalf("post-reconnect totals = %+v, want turns=2", got)
	}

	// Daemon restart: a new store starts from zero.
	if got := NewStore().Snapshot("cli"); got != nil {
		t.Errorf("restarted store must be empty, got %+v", got)
	}
}

func TestWithToolsPreservesRecorder(t *testing.T) {
	store := NewStore()
	acc := store.Accumulator("cli")
	ctx := WithAccumulator(context.Background(), acc)

	base := &fakeChatModel{usage: usagePtr(tok(10, 0, 5))}
	rec := NewRecordedChatModel(base)

	derived, err := rec.WithTools(nil)
	if err != nil {
		t.Fatalf("with-tools: %v", err)
	}

	// The ADK calls WithTools(...).Stream(...) — usage must still record.
	sr, err := derived.Stream(ctx, nil)
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	for {
		if _, err := sr.Recv(); err != nil {
			break
		}
	}

	if got := acc.Snapshot(); got == nil || got.ReadTokens != 10 {
		t.Errorf("totals = %+v, want read=10 through WithTools wrapper", got)
	}
}

func usagePtr(u schema.TokenUsage) *schema.TokenUsage { return &u }
