package main

import (
	"context"
	"testing"

	"github.com/cloudwego/eino/schema"
	"github.com/marco-rozz/ready/internal/activity"
	"github.com/marco-rozz/ready/internal/agent"
	"github.com/marco-rozz/ready/internal/channel"
	"github.com/marco-rozz/ready/internal/channel/telegram"
	"github.com/marco-rozz/ready/internal/llm"
)

type activityInspectRunner struct{ enabled bool }

func (r *activityInspectRunner) RunMessagesStreaming(ctx context.Context, _ string, _ []*schema.Message, _ func(string)) (agent.TurnOutcome, error) {
	r.enabled = activity.Enabled(ctx)

	return agent.TurnOutcome{Reply: "ok"}, nil
}

func TestTelegramTurnDoesNotCarryActivityTracker(t *testing.T) {
	tg := &termTGChannel{}
	env := newTermEnv(t, channelFormatterCfg{}, tg)
	runner := &activityInspectRunner{}
	env.agentReg = &agentRegistry{exec: &plainRunnerExec{runner: runner, layout: llm.LayoutTailDynamic}, layout: llm.LayoutTailDynamic}
	env.run(channel.Message{ChannelName: telegram.ChannelName, Content: "telegram activity isolation", Metadata: map[string]string{telegram.MetadataChatID: "42"}})
	if runner.enabled {
		t.Fatal("Telegram turn carried a ZMQ activity tracker")
	}
}
