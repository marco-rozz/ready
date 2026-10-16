//nolint:goconst
package toolnames

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/rs/zerolog"
)

// fakeModel is a minimal ToolCallingChatModel returning a canned Generate
// response. Only the fields exercised by the tests are populated.
type fakeModel struct {
	reply *schema.Message
}

func (f *fakeModel) Generate(_ context.Context, _ []*schema.Message, _ ...model.Option) (*schema.Message, error) {
	// Return a fresh copy per call so callers can mutate safely.
	cp := *f.reply
	cp.ToolCalls = append([]schema.ToolCall(nil), f.reply.ToolCalls...)

	return &cp, nil
}

func (f *fakeModel) Stream(_ context.Context, _ []*schema.Message, _ ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	return schema.StreamReaderFromArray([]*schema.Message{f.reply}), nil
}

func (f *fakeModel) WithTools(_ []*schema.ToolInfo) (model.ToolCallingChatModel, error) {
	return f, nil
}

func TestNormalizingChatModel_Generate_RewritesHallucinatedName(t *testing.T) {
	base := &fakeModel{reply: &schema.Message{
		Role: schema.Assistant,
		ToolCalls: []schema.ToolCall{
			{ID: "1", Function: schema.FunctionCall{Name: "home_assistant__ha_state_query", Arguments: `{}`}},
		},
	}}

	r := NewResolver([]string{"ha_state_query", "home_assistant__light/turn_on"})
	nm := NewNormalizingChatModel(base, r, zerolog.Nop())

	msg, err := nm.Generate(context.Background(), nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := msg.ToolCalls[0].Function.Name; got != "ha_state_query" {
		t.Fatalf("expected rewrite to ha_state_query, got %q", got)
	}
}

func TestNormalizingChatModel_Generate_PassesThroughKnownName(t *testing.T) {
	base := &fakeModel{reply: &schema.Message{
		Role: schema.Assistant,
		ToolCalls: []schema.ToolCall{
			{ID: "1", Function: schema.FunctionCall{Name: "home_assistant__light/turn_on"}},
		},
	}}

	r := NewResolver([]string{"ha_state_query", "home_assistant__light/turn_on"})
	nm := NewNormalizingChatModel(base, r, zerolog.Nop())

	msg, err := nm.Generate(context.Background(), nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := msg.ToolCalls[0].Function.Name; got != "home_assistant__light/turn_on" {
		t.Fatalf("known name must be preserved, got %q", got)
	}
}

func TestNormalizingChatModel_NilResolver_PassThrough(t *testing.T) {
	base := &fakeModel{reply: &schema.Message{
		Role: schema.Assistant,
		ToolCalls: []schema.ToolCall{
			{ID: "1", Function: schema.FunctionCall{Name: "whatever"}},
		},
	}}

	nm := NewNormalizingChatModel(base, nil, zerolog.Nop())

	msg, err := nm.Generate(context.Background(), nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := msg.ToolCalls[0].Function.Name; got != "whatever" {
		t.Fatalf("nil resolver must pass through, got %q", got)
	}
}

func TestNormalizingChatModel_Generate_RewritesBlockedCall(t *testing.T) {
	base := &fakeModel{reply: &schema.Message{
		Role: schema.Assistant,
		ToolCalls: []schema.ToolCall{
			{ID: "42", Function: schema.FunctionCall{Name: "home_assistant__ha_get_history", Arguments: `{}`}},
		},
	}}

	r := NewResolver([]string{"home_assistant__light/turn_on"})
	r.LoadBlocked([]BlockedTool{{Name: "home_assistant__ha_get_history", Owner: "home_assistant"}})

	nm := NewDisclosingChatModel(base, r, zerolog.Nop(), "default")

	msg, err := nm.Generate(context.Background(), nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	tc := msg.ToolCalls[0]
	if tc.Function.Name != PolicyBlockToolName {
		t.Fatalf("expected blocked call rewritten to %s, got %q", PolicyBlockToolName, tc.Function.Name)
	}

	var args policyBlockArgs
	if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
		t.Fatalf("sentinel arguments must be valid JSON: %v", err)
	}
	if args.Tool != "home_assistant__ha_get_history" || args.Owner != "home_assistant" || args.Profile != "default" {
		t.Errorf("unexpected sentinel arguments: %+v", args)
	}
	if tc.ID != "42" {
		t.Errorf("rewrite must preserve the call id, got %q", tc.ID)
	}
}

func TestNormalizingChatModel_KnobOff_BlockedCallPassesThrough(t *testing.T) {
	base := &fakeModel{reply: &schema.Message{
		Role: schema.Assistant,
		ToolCalls: []schema.ToolCall{
			{ID: "1", Function: schema.FunctionCall{Name: "home_assistant__ha_get_history"}},
		},
	}}

	// Same ghost index, but disclosure disabled (NewNormalizingChatModel).
	r := NewResolver([]string{"home_assistant__light/turn_on"})
	r.LoadBlocked([]BlockedTool{{Name: "home_assistant__ha_get_history", Owner: "home_assistant"}})

	nm := NewNormalizingChatModel(base, r, zerolog.Nop())

	msg, err := nm.Generate(context.Background(), nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := msg.ToolCalls[0].Function.Name; got != "home_assistant__ha_get_history" {
		t.Fatalf("knob-off must leave the blocked call untouched, got %q", got)
	}
}

func TestPolicyBlockTool_DirectCallInformational(t *testing.T) {
	p := NewPolicyBlockTool()

	info, err := p.Info(context.Background())
	if err != nil {
		t.Fatalf("Info: %v", err)
	}
	if info.Name != PolicyBlockToolName {
		t.Errorf("unexpected sentinel name %q", info.Name)
	}

	out, err := p.InvokableRun(context.Background(), `{"unrelated":"args"}`)
	if err != nil {
		t.Fatalf("direct call must not error: %v", err)
	}
	if !strings.Contains(out, "invoked automatically") {
		t.Errorf("direct call must return the informational text, got %q", out)
	}

	out, err = p.InvokableRun(context.Background(), `not-json`)
	if err != nil {
		t.Fatalf("unparseable direct call must not error: %v", err)
	}
	if !strings.Contains(out, "invoked automatically") {
		t.Errorf("unparseable direct call must return the informational text, got %q", out)
	}
}

func TestPolicyBlockTool_RewrittenCallDeliversDisclosure(t *testing.T) {
	p := NewPolicyBlockTool()

	out, err := p.InvokableRun(context.Background(),
		`{"tool":"home_assistant__ha_get_history","owner":"home_assistant","profile":"default"}`)
	if err != nil {
		t.Fatalf("rewritten call must not error: %v", err)
	}
	for _, want := range []string{"home_assistant__ha_get_history", "home_assistant", "default", "Do not seek alternative routes", "unreachable"} {
		if !strings.Contains(out, want) {
			t.Errorf("disclosure message missing %q: %s", want, out)
		}
	}
}
