// Readiness probing and boot evaluation (the design doc, tasks 4.1–4.6).
//
// Readiness is established by an isolated synthetic tool-call round trip: a
// transient runner built over the candidate's model, carrying ONLY a local
// no-op tool and a fixed probe prompt — no user history, no production tool
// definitions, no Home Assistant data, no side effects. Success requires a
// completed tool call (a valid call/result exchange), proving the model
// honours the agent's tool-calling contract; a model that can generate text
// but rejects the tool definition or the tool-call cycle is NOT ready.
//
// The model-catalog request is retained as a preflight (task 4.6): it can
// reject a candidate early (transport, auth, model-id absent), but catalog
// presence alone never establishes readiness.
package orchestrator

import (
	"context"
	"fmt"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
	"github.com/marco-rozz/ready/internal/agent"
	"github.com/marco-rozz/ready/internal/llm"
)

// probe constants. The probe exchange is intentionally minimal and fixed.
const (
	// probeToolName is the name of the synthetic no-op tool attached to the
	// readiness probe. It exists nowhere else — not in the production tool
	// catalog, not in any skill.
	probeToolName = "ready_probe_noop"

	// probeChannelName is the logical channel name the probe turn runs
	// under. It reaches no real channel; it only labels the runner call.
	probeChannelName = "orchestrator-probe"

	// probeMaxIterations bounds the probe's ReAct loop: tool call → tool
	// result → final answer needs two model calls.
	probeMaxIterations = 3
)

// probeSystemPrompt is the fixed system prompt of the readiness probe.
const probeSystemPrompt = "You are a connectivity probe. Follow the user's instruction exactly."

// probeUserMessage instructs the model to call the no-op tool. A model that
// honours the tool-calling contract calls it; a text-only model answers in
// prose instead, which fails the probe by design (text-only capability
// is insufficient).
const probeUserMessage = "Call the ready_probe_noop tool with value 1, then reply with the word done."

// probeNoopTool is the local no-op tool attached to readiness probes. It has
// no side effects: InvokableRun returns a constant without touching the
// filesystem, the network, or Home Assistant.
type probeNoopTool struct{}

// Guarantees probeNoopTool satisfies tool.InvokableTool.
var _ tool.InvokableTool = (*probeNoopTool)(nil)

// Info returns the synthetic tool definition presented to the model.
func (t *probeNoopTool) Info(_ context.Context) (*schema.ToolInfo, error) {
	return &schema.ToolInfo{
		Name: probeToolName,
		Desc: "A no-op connectivity probe tool. Calling it has no effect and always succeeds.",
		ParamsOneOf: schema.NewParamsOneOfByParams(map[string]*schema.ParameterInfo{
			"value": {
				Type:     schema.Number,
				Desc:     "Any number; ignored.",
				Required: true,
			},
		}),
	}, nil
}

// InvokableRun returns a constant result. No side effects by construction.
func (t *probeNoopTool) InvokableRun(_ context.Context, _ string, _ ...tool.Option) (string, error) {
	return "probe ok", nil
}

// probeMessages returns the fixed probe conversation: one user message, no
// history. The system prompt is injected by the probe runner's construction.
func probeMessages() []*schema.Message {
	return []*schema.Message{schema.UserMessage(probeUserMessage)}
}

// buildProbeRunner constructs the transient readiness-probe runner for a
// candidate: the same two construction paths as the production runner, but
// with only the local no-op tool and the fixed probe prompt. The probe runner
// is discarded after the probe; it never sees production tools or history.
func buildProbeRunner(ctx context.Context, m llm.ResolvedModel) (agent.Runner, error) {
	probeTools := []tool.BaseTool{&probeNoopTool{}}

	if m.IsAgentic() {
		return agent.NewAgentic(ctx, agent.AgenticConfig{
			Model:         m.Agentic,
			SystemPrompt:  probeSystemPrompt,
			Tools:         probeTools,
			MaxIterations: probeMaxIterations,
		}, nil)
	}

	return agent.New(ctx, agent.Config{
		ChatModel:     m.Chat,
		SystemPrompt:  probeSystemPrompt,
		Tools:         probeTools,
		MaxIterations: probeMaxIterations,
	}, nil)
}

// probeCandidate runs the synthetic tool-call readiness probe for one
// candidate under the configured probe timeout. It returns nil when the
// candidate completed a valid tool call/result exchange. All inputs are
// synthetic; the probe performs no I/O besides the candidate's own model
// call and has no side effects.
func (o *Orchestrator) probeCandidate(ctx context.Context, cand Candidate) error {
	probeCtx, cancel := context.WithTimeout(ctx, o.cfg.Recovery.ProbeTimeout)
	defer cancel()

	runner, err := buildProbeRunner(probeCtx, cand.Model)
	if err != nil {
		return fmt.Errorf("failed to build probe runner: %w", err)
	}

	outcome, err := runner.RunMessagesStreaming(probeCtx, probeChannelName, probeMessages(), nil)
	if err != nil {
		return fmt.Errorf("readiness probe call failed: %w", err)
	}

	if !outcome.ToolCallCompleted {
		return fmt.Errorf("readiness probe failed: model did not complete the synthetic tool-call exchange (text-capable but tool-incompatible?)") // probe-local diagnostic
	}

	return nil
}

// Boot performs the lazy readiness evaluation (tasks 4.6, 6.1): candidates
// are evaluated in declared order only until the first ready one — catalog
// preflight first (an accelerator: transport/auth/model-id rejections skip
// the probe), then the synthetic tool-call probe. The first ready candidate
// becomes the boot-active reference; every later candidate is left unproven
// (never probed, attemptable on demand, verified by its first real use).
//
// A per-candidate construction, credential, preflight, or readiness failure
// logs a WARN and marks only that candidate unhealthy (hard cooldown); the
// daemon still starts. When no otherwise-valid candidate is ready, the
// orchestrator enters the unavailable state and the background recovery
// worker takes over — an unreachable default no longer fails boot.
func (o *Orchestrator) Boot(ctx context.Context) {
	for i := range o.cands {
		if ctx.Err() != nil {
			return
		}

		cand := o.candidateCopy(i)

		// Per-candidate construction failure (task 4.7): logged as WARN,
		// candidate unhealthy, boot continues.
		if cand.Runner == nil {
			o.logger.Warn().
				Str("model", cand.Ref).
				Str("api", cand.API).
				Err(cand.ConstructErr).
				Msg("LLM candidate construction failed; candidate unhealthy")

			o.markBootUnhealthy(i)

			continue
		}

		// Catalog preflight (task 4.6): connectivity/auth/model-id check.
		// An inconclusive catalog proceeds to the real probe.
		if err := llm.PreflightCatalog(ctx, o.cfg.Registry, cand.Ref, o.logger); err != nil {
			o.logger.Warn().
				Str("model", cand.Ref).
				Str("api", cand.API).
				Err(err).
				Msg("LLM candidate catalog preflight failed; candidate unhealthy")

			o.markBootUnhealthy(i)

			continue
		}

		// Readiness probe: the synthetic tool-call exchange.
		if err := o.probeCandidate(ctx, cand); err != nil {
			o.logger.Warn().
				Str("model", cand.Ref).
				Str("api", cand.API).
				Err(err).
				Msg("LLM candidate readiness probe failed; candidate unhealthy")

			o.markBootUnhealthy(i)

			continue
		}

		// First ready candidate: boot-active. Later candidates stay
		// unproven.
		o.recordSuccess(i, true, "boot-readiness")

		o.logger.Info().
			Str("model", cand.Ref).
			Str("api", cand.API).
			Str("layout", cand.Layout.String()).
			Msg("LLM boot-active candidate ready")

		return
	}

	// No candidate ready: start unavailable (task 6.1).
	o.mu.Lock()
	prev := o.snapshotLocked()
	o.refreshStateLocked()
	state := o.state
	o.mu.Unlock()

	if state == StateUnavailable {
		o.logger.Warn().
			Msg("no LLM candidate ready at boot; starting in unavailable state (recovery probing will run when agent traffic occurs)")

		o.emitTransition(prev, "boot-unavailable")
	}
}

// markBootUnhealthy marks candidate i unhealthy at boot with the hard
// ladder's initial cooldown, so the recovery worker probes it once the
// cooldown expires and traffic resumes.
func (o *Orchestrator) markBootUnhealthy(i int) {
	o.mu.Lock()

	prev := o.snapshotLocked()

	o.cands[i].health.cooling = true
	o.cands[i].health.ladder = llm.LadderHard
	o.cands[i].health.demotions = 1
	o.cands[i].health.cooldownUntil = o.clock().Add(o.cfg.Recovery.Hard.InitialCooldown)

	o.refreshStateLocked()
	stateChanged := prev.State != o.state
	o.mu.Unlock()

	if stateChanged {
		o.emitTransition(prev, "boot-unhealthy")
	}
}
