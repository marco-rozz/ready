package main

import (
	"context"
	"strings"
	"testing"

	"github.com/marco-rozz/ready/internal/channel/zmqcli"
	"github.com/marco-rozz/ready/internal/compaction"
	"github.com/marco-rozz/ready/internal/llm"
	"github.com/marco-rozz/ready/internal/orchestrator"
	"github.com/marco-rozz/ready/internal/persistence"
	"github.com/rs/zerolog"
)

func TestContextUsageReportsUnavailableAndDoesNotMutateHistory(t *testing.T) {
	env := newTermEnv(t, channelFormatterCfg{}, &zmqcli.Channel{})
	orch := orchestrator.New(orchestrator.Config{Candidates: []orchestrator.Candidate{{CandidateDescriptor: llm.CandidateDescriptor{Ref: "p::m"}}, {CandidateDescriptor: llm.CandidateDescriptor{Ref: "provider::model-with-long-name"}}}}, zerolog.Nop())
	env.agentReg = &agentRegistry{orch: orch}
	rt := env.runtime()
	rt.activeProfileName = rejectedChatProfileName
	_, history := rt.sessions.History(context.Background(), cliAddress)
	before := len(history.Messages())
	cur, _ := rt.sessions.History(context.Background(), cliAddress)
	parts := rt.collectPromptParts(context.Background(), cur.ID, "", zmqcli.ChannelName)
	if !strings.Contains(parts.modelEstimate, "provider::model-with-long-name") {
		t.Fatalf("not conservative: %q", parts.modelEstimate)
	}
	report := rt.contextUsageMarkdown(context.Background())
	for _, want := range []string{"**Active profile:** `admin`", "**Channel:** `zmqcli`", "**Global model:** unavailable", "channel", "conservative candidate estimate"} {
		if !strings.Contains(report, want) {
			t.Errorf("missing %q in %s", want, report)
		}
	}
	if got := len(history.Messages()); got != before {
		t.Fatalf("history changed from %d to %d", before, got)
	}
}

func TestRuntimeContextOverheadAffectsGuardAndCompaction(t *testing.T) {
	env := newTermEnv(t, channelFormatterCfg{})
	rt := env.runtime()
	cur, history := rt.sessions.History(context.Background(), cliAddress)
	history.Add(persistence.HistoryMessage{Role: persistence.RoleUser, Content: strings.Repeat("a", 40)})
	history.Add(persistence.HistoryMessage{Role: persistence.RoleAssistant, Content: "answer"})
	history.Add(persistence.HistoryMessage{Role: persistence.RoleUser, Content: "following turn"})
	base := promptParts{}
	augmented := promptParts{channel: runtimePromptContext("current_channel", "zmqcli"), modelEstimate: runtimePromptContext("current_llm_model", "provider::long-model")}
	threshold := estimatePromptTokens(base, "", history.Messages())
	rt.trigger = compaction.Trigger{Window: threshold}
	if omit, _ := rt.turnStartGuard(context.Background(), cur, history, base, "", "cli"); omit != 0 {
		t.Fatalf("base omitted %d turns", omit)
	}
	before := len(history.Messages())
	if omit, _ := rt.turnStartGuard(context.Background(), cur, history, augmented, "", "cli"); omit == 0 {
		t.Fatal("runtime overhead did not trigger projection")
	}
	if len(history.Messages()) != before {
		t.Fatal("guard mutated history")
	}
	estimate := estimatePromptTokens(augmented, "", history.Messages())
	if !rt.trigger.Due(compaction.Compared(0, false, 0, estimate), 0) {
		t.Fatal("runtime overhead omitted from compaction decision")
	}
}
