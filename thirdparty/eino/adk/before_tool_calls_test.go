/*
 * READY patch: tests for WithBeforeToolCallsHook (see thirdparty/README.md).
 */

package adk

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"go.uber.org/mock/gomock"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/compose"
	mockModel "github.com/cloudwego/eino/internal/mock/components/model"
	"github.com/cloudwego/eino/schema"
)

type countingTool struct {
	name string
	runs *int32
}

func (t *countingTool) Info(_ context.Context) (*schema.ToolInfo, error) {
	return &schema.ToolInfo{Name: t.name, Desc: t.name}, nil
}

func (t *countingTool) InvokableRun(_ context.Context, _ string, _ ...tool.Option) (string, error) {
	atomic.AddInt32(t.runs, 1)
	return "ran", nil
}

func newBeforeHookAgent(t *testing.T, runs *int32, finalCalls *int32) *ChatModelAgent {
	t.Helper()

	ctx := context.Background()
	ctrl := gomock.NewController(t)
	cm := mockModel.NewMockToolCallingChatModel(ctrl)
	cm.EXPECT().WithTools(gomock.Any()).Return(cm, nil).AnyTimes()

	first := true
	cm.EXPECT().Generate(gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(func(context.Context, []*schema.Message, ...interface{}) (*schema.Message, error) {
			if first {
				first = false
				return schema.AssistantMessage("calling", []schema.ToolCall{
					{ID: "c1", Function: schema.FunctionCall{Name: "act", Arguments: "{}"}},
				}), nil
			}
			atomic.AddInt32(finalCalls, 1)
			return schema.AssistantMessage("done", nil), nil
		}).AnyTimes()

	agent, err := NewChatModelAgent(ctx, &ChatModelAgentConfig{
		Name:        "TestAgent",
		Description: "Test agent",
		Model:       cm,
		ToolsConfig: ToolsConfig{ToolsNodeConfig: compose.ToolsNodeConfig{Tools: []tool.BaseTool{&countingTool{name: "act", runs: runs}}}},
	})
	assert.NoError(t, err)

	return agent
}

func drain(iter *AsyncIterator[*AgentEvent]) (lastErr error) {
	for {
		ev, ok := iter.Next()
		if !ok {
			return lastErr
		}
		if ev.Err != nil {
			lastErr = ev.Err
		}
	}
}

func TestBeforeToolCallsHook(t *testing.T) {
	veto := errors.New("veto")

	t.Run("ErrorAbortsBeforeAnyToolRuns", func(t *testing.T) {
		var runs, finals int32
		agent := newBeforeHookAgent(t, &runs, &finals)

		var fired int32
		iter := agent.Run(context.Background(), &AgentInput{Messages: []Message{schema.UserMessage("go")}},
			WithBeforeToolCallsHook(func(context.Context) error {
				atomic.AddInt32(&fired, 1)
				return veto
			}))

		err := drain(iter)
		assert.ErrorIs(t, err, veto)
		assert.EqualValues(t, 1, atomic.LoadInt32(&fired))
		assert.EqualValues(t, 0, atomic.LoadInt32(&runs), "no tool may execute after a veto")
		assert.EqualValues(t, 0, atomic.LoadInt32(&finals), "no further model call after a veto")
	})

	t.Run("NilErrorLetsToolsRun", func(t *testing.T) {
		var runs, finals int32
		agent := newBeforeHookAgent(t, &runs, &finals)

		var fired int32
		iter := agent.Run(context.Background(), &AgentInput{Messages: []Message{schema.UserMessage("go")}},
			WithBeforeToolCallsHook(func(context.Context) error {
				atomic.AddInt32(&fired, 1)
				return nil
			}))

		assert.NoError(t, drain(iter))
		assert.EqualValues(t, 1, atomic.LoadInt32(&fired))
		assert.EqualValues(t, 1, atomic.LoadInt32(&runs))
		assert.EqualValues(t, 1, atomic.LoadInt32(&finals))
	})

	t.Run("NotFiredWithoutToolCalls", func(t *testing.T) {
		ctx := context.Background()
		ctrl := gomock.NewController(t)
		cm := mockModel.NewMockToolCallingChatModel(ctrl)
		cm.EXPECT().WithTools(gomock.Any()).Return(cm, nil).AnyTimes()
		cm.EXPECT().Generate(gomock.Any(), gomock.Any(), gomock.Any()).
			Return(schema.AssistantMessage("hi", nil), nil).Times(1)

		var runs int32
		agent, err := NewChatModelAgent(ctx, &ChatModelAgentConfig{
			Name: "A", Description: "a", Model: cm,
			ToolsConfig: ToolsConfig{ToolsNodeConfig: compose.ToolsNodeConfig{Tools: []tool.BaseTool{&countingTool{name: "act", runs: &runs}}}},
		})
		assert.NoError(t, err)

		var fired int32
		iter := agent.Run(ctx, &AgentInput{Messages: []Message{schema.UserMessage("go")}},
			WithBeforeToolCallsHook(func(context.Context) error { atomic.AddInt32(&fired, 1); return veto }))

		assert.NoError(t, drain(iter))
		assert.EqualValues(t, 0, atomic.LoadInt32(&fired))
	})
}
