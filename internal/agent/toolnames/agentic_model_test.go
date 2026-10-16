//nolint:goconst
package toolnames

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/rs/zerolog"
)

// fakeAgenticModel is a minimal AgenticModel returning a canned Generate
// response with function tool-call blocks.
type fakeAgenticModel struct {
	reply *schema.AgenticMessage
}

func (f *fakeAgenticModel) Generate(_ context.Context, _ []*schema.AgenticMessage, _ ...model.Option) (*schema.AgenticMessage, error) {
	cp := *f.reply
	cp.ContentBlocks = append([]*schema.ContentBlock(nil), f.reply.ContentBlocks...)

	return &cp, nil
}

func (f *fakeAgenticModel) Stream(_ context.Context, _ []*schema.AgenticMessage, _ ...model.Option) (*schema.StreamReader[*schema.AgenticMessage], error) {
	return schema.StreamReaderFromArray([]*schema.AgenticMessage{f.reply}), nil
}

func blockedCallMsg(name string) *schema.AgenticMessage {
	return &schema.AgenticMessage{
		Role: schema.AgenticRoleTypeAssistant,
		ContentBlocks: []*schema.ContentBlock{
			{
				Type: schema.ContentBlockTypeFunctionToolCall,
				FunctionToolCall: &schema.FunctionToolCall{
					CallID:    "call_7",
					Name:      name,
					Arguments: `{}`,
				},
			},
		},
	}
}

func firstToolCall(msg *schema.AgenticMessage) *schema.FunctionToolCall {
	for _, b := range msg.ContentBlocks {
		if b.Type == schema.ContentBlockTypeFunctionToolCall && b.FunctionToolCall != nil {
			return b.FunctionToolCall
		}
	}

	return nil
}

func TestNormalizingAgenticModel_RewritesBlockedCall(t *testing.T) {
	base := &fakeAgenticModel{reply: blockedCallMsg("home_assistant__ha_get_history")}

	r := NewResolver([]string{"home_assistant__light/turn_on"})
	r.LoadBlocked([]BlockedTool{{Name: "home_assistant__ha_get_history", Owner: "home_assistant"}})

	nm := NewDisclosingAgenticModel(base, r, zerolog.Nop(), "default")

	msg, err := nm.Generate(context.Background(), nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	call := firstToolCall(msg)
	if call == nil {
		t.Fatal("expected a function tool-call block")
	}
	if call.Name != PolicyBlockToolName {
		t.Fatalf("expected rewrite to %s, got %q", PolicyBlockToolName, call.Name)
	}

	var args policyBlockArgs
	if err := json.Unmarshal([]byte(call.Arguments), &args); err != nil {
		t.Fatalf("sentinel arguments must be valid JSON: %v", err)
	}
	if args.Tool != "home_assistant__ha_get_history" || args.Owner != "home_assistant" || args.Profile != "default" {
		t.Errorf("unexpected sentinel arguments: %+v", args)
	}
	if call.CallID != "call_7" {
		t.Errorf("rewrite must preserve the call id, got %q", call.CallID)
	}
}

func TestNormalizingAgenticModel_KnobOff_PassThrough(t *testing.T) {
	base := &fakeAgenticModel{reply: blockedCallMsg("home_assistant__ha_get_history")}

	r := NewResolver([]string{"home_assistant__light/turn_on"})
	r.LoadBlocked([]BlockedTool{{Name: "home_assistant__ha_get_history", Owner: "home_assistant"}})

	nm := NewNormalizingAgenticModel(base, r, zerolog.Nop())

	msg, err := nm.Generate(context.Background(), nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := firstToolCall(msg).Name; got != "home_assistant__ha_get_history" {
		t.Fatalf("knob-off must leave the blocked call untouched, got %q", got)
	}
}

func TestNormalizingAgenticModel_UnknownNamePassesThrough(t *testing.T) {
	base := &fakeAgenticModel{reply: blockedCallMsg("totally_unknown")}

	r := NewResolver(nil)
	r.LoadBlocked([]BlockedTool{{Name: "home_assistant__ha_get_history", Owner: "home_assistant"}})

	nm := NewDisclosingAgenticModel(base, r, zerolog.Nop(), "default")

	msg, err := nm.Generate(context.Background(), nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := firstToolCall(msg).Name; got != "totally_unknown" {
		t.Fatalf("unknown name must pass through untouched, got %q", got)
	}
}

func TestNormalizingAgenticModel_StreamRewrites(t *testing.T) {
	base := &fakeAgenticModel{reply: blockedCallMsg("home_assistant__ha_get_history")}

	r := NewResolver([]string{"home_assistant__light/turn_on"})
	r.LoadBlocked([]BlockedTool{{Name: "home_assistant__ha_get_history", Owner: "home_assistant"}})

	nm := NewDisclosingAgenticModel(base, r, zerolog.Nop(), "default")

	sr, err := nm.Stream(context.Background(), nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer sr.Close()

	for {
		chunk, recvErr := sr.Recv()
		if recvErr != nil {
			t.Fatalf("stream ended with error: %v", recvErr)
		}
		if got := firstToolCall(chunk).Name; got == PolicyBlockToolName {
			return // rewrite observed on the streamed chunk
		}
	}
}
