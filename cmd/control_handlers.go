package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/marco-rozz/ready/internal/channel/zmqcli"
	"github.com/marco-rozz/ready/internal/compaction"
	"github.com/marco-rozz/ready/internal/orchestrator"
	"github.com/marco-rozz/ready/internal/persistence"
	"github.com/marco-rozz/ready/internal/protocol"
	"github.com/marco-rozz/ready/internal/session"
)

// buildControlRequestHandler returns the zmqcli.ControlRequestHandler serving
// session_info and context_usage for the `cli` address (the ZMQ channel's
// Conversation address). Neither handler invokes the LLM, mutates
// conversation history, or touches a channel formatter — both are answered
// from live daemon state.
func buildControlRequestHandler(ctx context.Context, rt *sessionRuntime) zmqcli.ControlRequestHandler {
	return func(reqType protocol.RequestType, connectionID string) string {
		switch reqType {
		case protocol.RequestTypeSessionInfo:
			return rt.sessionInfoMarkdown(ctx, connectionID)
		case protocol.RequestTypeContextUsage:
			return rt.contextUsageMarkdown(ctx)
		default:
			return "Unknown control request."
		}
	}
}

// cliAddress is the Conversation address served by the ZMQ channel.
const cliAddress = session.Address(zmqcli.ChannelName)

// sessionInfoMarkdown renders the /session block: Connection and Session
// identity (the effective Session — the pending one while a reset awaits
// application), the Session file, compactions, model, message/turn counts of
// the model's current view and token totals.
func (rt *sessionRuntime) sessionInfoMarkdown(ctx context.Context, connectionID string) string {
	var b strings.Builder
	b.WriteString("## Session\n\n")

	effectiveID, pending := rt.sessions.EffectiveID(ctx, cliAddress)
	cur, history := rt.sessions.History(ctx, cliAddress)

	fmt.Fprintf(&b, "**Session ID:** `%s`\n", effectiveID)
	fmt.Fprintf(&b, "**Address:** `%s`\n", cliAddress)
	fmt.Fprintf(&b, "**Connection ID:** `%s`\n", connectionID)

	if pending {
		b.WriteString("**Session file:** created with the next message (reset pending)\n")
	} else {
		// Privacy: the path may carry the home prefix — redact it the same
		// way as every user-visible path (privacy.FromHome).
		fmt.Fprintf(&b, "**Session file:** `%s`\n", rt.sanitizer.Redact(cur.Path))
		fmt.Fprintf(&b, "**Started:** %s\n", cur.StartedAt.Local().Format(time.DateTime))

		if n, last := history.CompactionCount(); n > 0 {
			fmt.Fprintf(&b, "**Compactions:** %d (last %s)\n", n, last.Local().Format(time.DateTime))
		} else {
			b.WriteString("**Compactions:** 0\n")
		}
	}

	if snap := rt.agentReg.orch.Snapshot(); snap.ActiveRef != "" {
		fmt.Fprintf(&b, "**Model:** `%s`", snap.ActiveRef)
		if snap.State == orchestrator.StateUnavailable {
			b.WriteString(" — LLM unavailable")
		}
		b.WriteString("\n")
	}

	// Reasoning effort is display-only when a configuration knob exists; no
	// knob exists today, so the line is omitted (the architecture decision records).

	if pending {
		b.WriteString("**Messages:** 0\n**Tokens:** none recorded in this session\n")

		return b.String()
	}

	total, user, assistant, toolResults, turnContexts := 0, 0, 0, 0, 0
	for _, m := range history.Messages() {
		total++
		switch {
		case m.Kind == persistence.KindTurnContext:
			turnContexts++
		case m.Role == persistence.RoleUser:
			user++
		case m.Role == persistence.RoleAssistant:
			assistant++
		case m.Role == persistence.RoleTool:
			toolResults++
		}
	}
	fmt.Fprintf(&b, "**Messages:** %d (user %d, assistant %d, tool results %d, turn context %d — model view)\n",
		total, user, assistant, toolResults, turnContexts)

	if totals := rt.usageStore.Snapshot(cur.ID); totals != nil {
		fmt.Fprintf(&b, "**Turns:** %s · **LLM calls:** %s · **Aux calls:** %s\n",
			formatTokens(totals.TurnCount), formatTokens(totals.LLMCalls), formatTokens(totals.AuxCalls))
		fmt.Fprintf(&b, "**Tokens:** read %s · written %s", formatTokens(totals.ReadTokens), formatTokens(totals.WrittenTokens))
		if totals.HasCached && totals.ReadTokens > 0 {
			fmt.Fprintf(&b, " · cached %s (%.1f%%)", formatTokens(totals.CachedTokens), 100*float64(totals.CachedTokens)/float64(totals.ReadTokens))
		}
		b.WriteString("\n")
	} else {
		b.WriteString("**Tokens:** none recorded in this session\n")
	}

	return b.String()
}

// contextSection is one estimated entry of the /context breakdown.
type contextSection struct {
	name  string
	chars int
}

// contextUsageMarkdown renders the /context block: an estimated per-section
// breakdown mirroring the assembled prompt (stable prefix, tool catalog,
// compaction summary, history view) — the same estimator the compaction
// trigger uses — anchored to the last main-agent call's real prompt tokens,
// with occupancy and free space only when the active provider declares a
// context window. Every number is labelled as an estimate.
func (rt *sessionRuntime) contextUsageMarkdown(ctx context.Context) string {
	snap := rt.agentReg.orch.Snapshot()
	layout := rt.agentReg.PromptLayout()

	var b strings.Builder
	b.WriteString("## Context usage (estimated)\n\n")

	if snap.ActiveRef != "" {
		fmt.Fprintf(&b, "**Model:** `%s`\n", snap.ActiveRef)
	}
	fmt.Fprintf(&b, "**Layout:** %s — per-section numbers are characters÷4 estimates\n", layout)

	cur, history := rt.sessions.History(ctx, cliAddress)
	sid := rt.agentSkillsSession(ctx, cur, history, zmqcli.ChannelName)
	parts := rt.collectPromptParts(ctx, cur.ID, sid, zmqcli.ChannelName)
	lead := rt.leadMessage(ctx, history, nil)

	estimated := 0
	for _, s := range promptSections(parts, lead, history.Messages()) {
		fmt.Fprintf(&b, "- **%s** — ~%s tokens\n", s.name, formatTokens(compaction.Tokens(s.chars)))
		estimated += compaction.Tokens(s.chars)
	}
	fmt.Fprintf(&b, "**Total (estimated):** ~%s tokens\n", formatTokens(estimated))

	if summary, preserved, ok := history.Summary(); ok {
		n, last := history.CompactionCount()
		fmt.Fprintf(&b, "**Compaction summary:** ~%s tokens · %d compaction(s), last %s",
			formatTokens(compaction.Tokens(len(summary))), n, last.Local().Format(time.DateTime))
		if len(preserved) > 0 {
			fmt.Fprintf(&b, " · pinned skills: %s", strings.Join(preserved, ", "))
		}
		b.WriteString("\n")
	}

	totals := rt.usageStore.Snapshot(cur.ID)
	if totals != nil && totals.LastMainPromptTokens > 0 {
		fmt.Fprintf(&b, "**Last main call (real):** %s prompt tokens\n", formatTokens(totals.LastMainPromptTokens))
	}

	if window := snap.ActiveContextWindow; window > 0 {
		// The occupancy anchor is the real prompt-token count of the last
		// main-agent call when available, the estimate otherwise.
		base := estimated
		if totals != nil && totals.LastMainPromptTokens > 0 {
			base = totals.LastMainPromptTokens
		}

		free := max(window-base, 0)
		fmt.Fprintf(&b, "**Context window:** %s · **Occupancy:** %.1f%% · **Free:** ~%s tokens\n",
			formatTokens(window), 100*float64(base)/float64(window), formatTokens(free))
	} else {
		b.WriteString("**Context window:** not configured — occupancy and free space unavailable\n")
	}

	return b.String()
}

// formatTokens renders an integer with thousands separators.
func formatTokens(n int) string {
	s := fmt.Sprintf("%d", n)
	var out []string
	for len(s) > 3 {
		out = append([]string{s[len(s)-3:]}, out...)
		s = s[:len(s)-3]
	}
	out = append([]string{s}, out...)

	return strings.Join(out, ",")
}
