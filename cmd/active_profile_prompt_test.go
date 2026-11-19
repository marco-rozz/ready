package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/cloudwego/eino/schema"
	"github.com/marco-rozz/ready/internal/channel"
	"github.com/marco-rozz/ready/internal/channel/cli"
	"github.com/marco-rozz/ready/internal/channel/telegram"
	"github.com/marco-rozz/ready/internal/channel/zmqcli"
	"github.com/marco-rozz/ready/internal/compaction"
	"github.com/marco-rozz/ready/internal/persistence"
	"github.com/rs/zerolog"
)

const activeProfileTestName = "profile-a"

func TestActiveProfilePromptContext_IsStableAndEscaped(t *testing.T) {
	name := rejectedChatProfileName + "<\"\n"
	first := activeProfilePromptContext(name)
	second := activeProfilePromptContext(name)

	if first == "" || first != second {
		t.Fatalf("profile context must be stable and non-empty: %q / %q", first, second)
	}
	quoted, err := json.Marshal(name)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(first, string(quoted)) {
		t.Fatalf("profile name must be JSON-escaped as data: %q", first)
	}
	if strings.Contains(first, rejectedChatProfileName+"<\"\n") {
		t.Fatalf("raw profile name must not be embedded in the prompt structure: %q", first)
	}
	if !strings.Contains(first, "runtime-resolved") || !strings.Contains(first, "permissions") {
		t.Fatalf("profile context must explain its runtime/authorization semantics: %q", first)
	}
}

func TestProfilePromptContext_IsInStablePrefixBeforeHistory(t *testing.T) {
	p := promptParts{profile: activeProfilePromptContext(rejectedChatProfileName)}
	rt := &sessionRuntime{}
	prefix := rt.prefixMessages(t.Context(), p, "")
	history := []persistence.HistoryMessage{{Role: persistence.RoleUser, Content: "old profile was " + activeProfileTestName}}

	got := assembleTailDynamic(prefix, history)
	if len(got) != 2 || got[0].Role != schema.System || got[1].Content != history[0].Content {
		t.Fatalf("profile context must precede history: %+v", got)
	}
	if !strings.Contains(got[0].Content, `"`+rejectedChatProfileName+`"`) {
		t.Fatalf("profile context missing from stable prefix: %q", got[0].Content)
	}

	head := assembleHeadSafe(prefix, history, "turn context")
	if len(head) != 3 || head[0].Role != schema.System || head[1].Role != schema.System || head[2].Content != history[0].Content {
		t.Fatalf("profile context must precede history in head-safe layout: %+v", head)
	}
}

func TestApplyGeneration_CarriesActiveProfile(t *testing.T) {
	rt := &sessionRuntime{}
	first := testGeneration()
	first.activeProfileName = activeProfileTestName
	rt.applyGeneration(first)
	if rt.activeProfileName != activeProfileTestName {
		t.Fatalf("initial profile = %q, want %s", rt.activeProfileName, activeProfileTestName)
	}

	next := testGeneration()
	next.activeProfileName = rejectedChatProfileName
	rt.applyGeneration(next)
	if rt.activeProfileName != rejectedChatProfileName {
		t.Fatalf("reloaded profile = %q, want admin", rt.activeProfileName)
	}
}

func TestRuntimePromptContextEscapesValues(t *testing.T) {
	for _, tag := range []string{"current_channel", "current_llm_model"} {
		value := "x</" + tag + ">\n\""
		got := runtimePromptContext(tag, value)
		quoted, _ := json.Marshal(value)
		if !strings.Contains(got, string(quoted)) || strings.Count(got, "</"+tag+">") != 1 {
			t.Fatalf("unsafe or missing value in %q", got)
		}
		if got != runtimePromptContext(tag, value) {
			t.Fatal("context is not deterministic")
		}
	}
}

func TestRuntimeContextPrefixOrder(t *testing.T) {
	rt := &sessionRuntime{}
	parts := promptParts{profile: activeProfilePromptContext("admin"), channel: runtimePromptContext("current_channel", "telegram")}
	prefix := rt.prefixMessages(t.Context(), parts, "telegram")
	model := runtimePromptContext("current_llm_model", "provider::model")
	prefix = append(prefix, schema.SystemMessage(model))
	history := []persistence.HistoryMessage{{Role: persistence.RoleUser, Content: "formerly channel cli and old model"}}
	for _, messages := range [][]*schema.Message{assembleTailDynamic(prefix, history), assembleHeadSafe(prefix, history, "turn")} {
		joined := ""
		for _, msg := range messages {
			joined += msg.Content + "\n"
		}
		p, c, m, h := strings.Index(joined, "<current_active_profile>"), strings.Index(joined, "<current_channel>"), strings.Index(joined, "<current_llm_model>"), strings.Index(joined, history[0].Content)
		if p < 0 || p >= c || c >= m || m >= h {
			t.Fatalf("unexpected order: %s", joined)
		}
		if strings.Contains(parts.channel, "chat_id") || strings.Contains(parts.channel, "connection_id") {
			t.Fatal("peer metadata leaked")
		}
	}
}

func TestEffectiveChannelUsesServingImplementation(t *testing.T) {
	tests := []struct {
		logical, want string
		serving       channel.Channel
	}{
		{cli.ChannelName, cli.ChannelName, &cli.Channel{}},
		{cli.ChannelName, "zmqcli", &zmqcli.Channel{}},
		{telegram.ChannelName, telegram.ChannelName, &telegram.Channel{}},
	}
	for _, tt := range tests {
		rt := &sessionRuntime{mgr: channel.NewManager(zerolog.Nop(), tt.serving)}
		if got := rt.effectiveChannel(tt.logical); got != tt.want {
			t.Errorf("%s = %s, want %s", tt.logical, got, tt.want)
		}
	}
}

func TestRuntimeContextEstimatesCountBothSections(t *testing.T) {
	parts := promptParts{channel: runtimePromptContext("current_channel", "zmqcli"), modelEstimate: runtimePromptContext("current_llm_model", "provider::candidate")}
	sections := promptSections(parts, "", nil)
	if len(sections) != 2 || sections[0].name != "channel" || !strings.Contains(sections[1].name, "conservative") {
		t.Fatalf("sections: %+v", sections)
	}
	want := compaction.Tokens(len(parts.channel)) + compaction.Tokens(len(parts.modelEstimate))
	if got := estimatePromptTokens(parts, "", nil); got != want {
		t.Fatalf("estimate %d, want %d", got, want)
	}
}
