//nolint:goconst
package twin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
	"github.com/marco-rozz/ready/pkg/hatwin"
	"github.com/rs/zerolog"
)

// commandToolName is the ha_command wire name (Twin owner, never prefixed).
const commandToolName = "ha_command"

// turnDeadlineMargin is the buffer kept from the remaining turn deadline when
// clamping the verify window (same discipline as the wait tool).
const turnDeadlineMargin = 5 * time.Second

// defaultVerifyTimeout / clamps come from config; they live in the adapter
// config struct built by BuildTools.

// commandToolConfig carries the operator knobs for the write path
// (home_assistant.commands.*).
type commandToolConfig struct {
	VerifyTimeout time.Duration
	MaxTargets    int
}

// CommandTool is the ha_command Eino adapter over pkg/hatwin's command
// machinery (twin-native-actuation D4/D7). The description is rendered at
// boot from the active vocabulary and is static for the process lifetime
// (prompt-cache safe).
type CommandTool struct {
	twin        *hatwin.Twin
	cfg         commandToolConfig
	description string
	log         zerolog.Logger
}

var _ tool.InvokableTool = (*CommandTool)(nil)

// NewCommandTool builds ha_command. The description is rendered once from the
// twin's active domains; an empty active set renders an explicit notice.
func NewCommandTool(t *hatwin.Twin, cfg commandToolConfig, logger zerolog.Logger) *CommandTool {
	return &CommandTool{
		twin:        t,
		cfg:         cfg,
		description: renderCommandDescription(t),
		log:         logger.With().Str("component", "ha_command").Logger(),
	}
}

// renderCommandDescription renders the static tool description from the
// active vocabulary (no MCP knowledge — fallback guidance is generic).
func renderCommandDescription(t *hatwin.Twin) string {
	var b strings.Builder
	b.WriteString("Execute a common home-automation command through the Home Assistant digital twin (real-time, verified). ")
	b.WriteString("NOT for locks or alarm panels (PIN codes must never be requested through this tool). ")
	b.WriteString(hatwin.RenderVocabularyDescription(t.ActiveCommandDomains()))
	b.WriteString("\n\nModes: sync (default) sends and verifies the effect (per-entity outcomes); " +
		"send returns immediately with a call_id for slow devices — verify later with mode=verify and that call_id; " +
		"verify reports the outcomes collected so far for a prior call_id.\n" +
		"Target with explicit entity_ids (never invent ids — read them from ha_state_query) or with a selector: " +
		"domain (mandatory) plus area / pattern / name. toggle requires exactly one target — use on/off otherwise. " +
		"Outcomes per entity: applied (verified, before → after), pending (transitional or verification in progress), " +
		"already (was already in the requested state), unchanged ([UNVERIFIED] — the effect could not be confirmed; " +
		"check state with ha_state_query before any retry), unavailable, error.")

	return b.String()
}

type commandArgs struct {
	Action    string         `json:"action"`
	Domain    string         `json:"domain,omitempty"`
	Area      string         `json:"area,omitempty"`
	Pattern   string         `json:"pattern,omitempty"`
	Name      string         `json:"name,omitempty"`
	EntityIDs entityIDList   `json:"entity_ids,omitempty"`
	Params    map[string]any `json:"params,omitempty"`
	Mode      string         `json:"mode,omitempty"`
	CallID    string         `json:"call_id,omitempty"`
}

// entityIDList decodes entity_ids from a JSON array — the declared shape —
// and also from the string forms models emit for array parameters: a
// JSON-encoded array ("[\"light.a\"]") or a single bare id ("light.a").
type entityIDList []string

func (l *entityIDList) UnmarshalJSON(data []byte) error {
	var ids []string
	if err := json.Unmarshal(data, &ids); err == nil {
		*l = ids

		return nil
	}

	var raw string
	if err := json.Unmarshal(data, &raw); err != nil {
		return fmt.Errorf("entity_ids must be an array of entity ids: %w", err)
	}

	raw = strings.TrimSpace(raw)
	switch {
	case raw == "":
		*l = nil
	case strings.HasPrefix(raw, "["):
		if err := json.Unmarshal([]byte(raw), &ids); err != nil {
			return fmt.Errorf("entity_ids must be an array of entity ids: %w", err)
		}
		*l = ids
	default:
		*l = []string{raw}
	}

	return nil
}

func (c *CommandTool) Info(_ context.Context) (*schema.ToolInfo, error) {
	return &schema.ToolInfo{
		Name: commandToolName,
		Desc: c.description,
		ParamsOneOf: schema.NewParamsOneOfByParams(map[string]*schema.ParameterInfo{
			"action": {
				Type:     schema.String,
				Desc:     "Intent action, from the vocabulary above (on, off, toggle, set, open, close, stop, play, pause, next, previous, activate, run, press).",
				Required: true,
			},
			"domain": {
				Type: schema.String,
				Desc: "HA domain of the target(s) — mandatory when using area/pattern/name, ignored when entity_ids is given.",
			},
			"area": {
				Type: schema.String,
				Desc: "Selector: area_id (exact, case-insensitive) or area name (substring).",
			},
			"pattern": {
				Type: schema.String,
				Desc: "Selector: glob (supports * and ?) or substring against entity_id.",
			},
			"name": {
				Type: schema.String,
				Desc: "Selector: friendly-name substring (case- and accent-insensitive).",
			},
			"entity_ids": {
				Type:     schema.Array,
				ElemInfo: &schema.ParameterInfo{Type: schema.String},
				Desc:     "Explicit entity ids (preferred when known from ha_state_query), e.g. [\"light.cucina\"]. Validated — unknown ids refuse the call; ids of several domains are split into one HA call per domain under one call_id.",
			},
			"params": {
				Type: schema.Object,
				Desc: "Parameters for the parametric 'set' action (e.g. {\"brightness\": 128}, {\"position\": 50}, {\"temperature\": 21.5}, {\"hvac_mode\": \"heat\"}).",
			},
			"mode": {
				Type: schema.String,
				Desc: "sync (default: send + verify), send (return call_id immediately — for slow devices like covers/thermostats), verify (outcomes so far for call_id).",
			},
			"call_id": {
				Type: schema.String,
				Desc: "For mode=verify: the call_id returned by a prior send.",
			},
		}),
	}, nil
}

func (c *CommandTool) InvokableRun(ctx context.Context, argumentsInJSON string, _ ...tool.Option) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", fmt.Errorf("ha_command: %w", err)
	}

	if c.twin == nil {
		return "", fmt.Errorf("%w: the Home Assistant digital twin is disabled", hatwin.ErrNotConnected)
	}

	var args commandArgs
	if err := json.Unmarshal([]byte(argumentsInJSON), &args); err != nil {
		return "", fmt.Errorf("ha_command: invalid arguments: %w", err)
	}

	switch args.Mode {
	case "", "sync", "send", "verify":
	default:
		return "", fmt.Errorf("%w: mode must be sync, send or verify (got %q)", hatwin.ErrInvalidParams, args.Mode)
	}

	if args.Mode == "verify" {
		if args.CallID == "" {
			return "", fmt.Errorf("%w: mode=verify requires the call_id of a prior send", hatwin.ErrInvalidParams)
		}
		rep, err := c.twin.VerifyCommand(args.CallID)
		if err != nil {
			return "", err // guided: check state with ha_state_query instead
		}

		return marshalCommandResponse(rep, commandResponseGuidance{}), nil
	}

	if args.Action == "" {
		return "", fmt.Errorf("%w: pass an action from the vocabulary", hatwin.ErrOutOfVocabulary)
	}

	spec := hatwin.CommandSpec{
		Action:     args.Action,
		Domain:     args.Domain,
		Area:       args.Area,
		Pattern:    args.Pattern,
		Name:       args.Name,
		EntityIDs:  args.EntityIDs,
		Params:     args.Params,
		MaxTargets: c.cfg.MaxTargets,
	}
	if args.Mode != "send" {
		spec.VerifyWindow = c.verifyWindow(ctx)
	}

	start := time.Now()
	rep, err := c.twin.ExecuteCommand(ctx, spec)
	if err != nil {
		return "", c.withFallbackGuidance(err)
	}

	c.logCommand(rep, start)

	guidance := commandResponseGuidance{}
	if needsStateCheck(rep) {
		guidance.StateCheck = "At least one effect is unverified. Read the current state with ha_state_query BEFORE any retry through any path — a sent command may still be in flight or may have succeeded without confirmation."
	}

	return marshalCommandResponse(rep, guidance), nil
}

// commandResponseGuidance carries the no-blind-retry instruction when some
// effect is unverified.
type commandResponseGuidance struct {
	StateCheck string `json:"state_check,omitempty"`
}

// needsStateCheck reports whether any outcome should steer the model away
// from a blind retry (unchanged, pending, or partial batches).
func needsStateCheck(rep hatwin.CommandReport) bool {
	for _, o := range rep.Outcomes {
		if o.Outcome == hatwin.OutcomeUnchanged || o.Outcome == hatwin.OutcomePending {
			return true
		}
	}

	return false
}

// withFallbackGuidance turns refusal errors into the guided text required
// by twin-native-actuation D7, WITHOUT naming or knowing MCP: barrier
// failures (transient) tell the model the write path is down and that an
// equivalent catalog tool may serve the request; out-of-vocabulary requests
// (permanent) do the same. Target/parameter refusals already carry their own
// guidance and pass through unchanged.
func (c *CommandTool) withFallbackGuidance(err error) error {
	fallback := "; if an equivalent tool is present in your catalog you may use it, otherwise report the action as unavailable"
	switch {
	case errors.Is(err, hatwin.ErrNotConnected),
		errors.Is(err, hatwin.ErrSessionNotReady),
		errors.Is(err, hatwin.ErrStale):
		return fmt.Errorf("%w — nothing was sent: the Home Assistant write path is temporarily unavailable%s; retry shortly", err, fallback)
	case errors.Is(err, hatwin.ErrOutOfVocabulary):
		return fmt.Errorf("%w%s", err, fallback)
	}

	return err
}

// verifyWindow returns the verification budget for a sync command: the
// configured verify_timeout clamped by the remaining turn deadline minus a
// margin (like the wait tool), never below a minimal observation slice.
func (c *CommandTool) verifyWindow(ctx context.Context) time.Duration {
	window := c.cfg.VerifyTimeout
	if deadline, ok := ctx.Deadline(); ok {
		remaining := time.Until(deadline) - turnDeadlineMargin
		if remaining <= 0 {
			// No budget to verify: degrade to send semantics (call_id
			// returned, verify later). Still honest, never an error.
			return 0
		}
		if window > remaining {
			window = remaining
		}
	}

	return window
}

// logCommand emits the structured command record (D12): one INFO aggregate
// (call_id, per-call domain/service, target count, outcomes, duration) and
// one DEBUG line per entity. Never prompt, response, or credential content.
func (c *CommandTool) logCommand(rep hatwin.CommandReport, start time.Time) {
	counts := map[hatwin.Outcome]int{}
	for _, o := range rep.Outcomes {
		counts[o.Outcome]++
	}
	services := make([]string, 0, len(rep.Calls))
	for _, call := range rep.Calls {
		services = append(services, call.Domain+"/"+call.Service)
	}

	ev := c.log.Info().
		Str("call_id", rep.CallID).
		Strs("services", services).
		Int("targets", len(rep.Outcomes)).
		Dur("duration", time.Since(start))
	for _, o := range []hatwin.Outcome{
		hatwin.OutcomeApplied, hatwin.OutcomePending, hatwin.OutcomeAlready,
		hatwin.OutcomeUnchanged, hatwin.OutcomeUnavailable, hatwin.OutcomeError,
	} {
		if counts[o] > 0 {
			ev = ev.Int(string(o), counts[o])
		}
	}
	ev.Msg("command executed")

	for _, o := range rep.Outcomes {
		c.log.Debug().
			Str("call_id", rep.CallID).
			Str("entity_id", o.EntityID).
			Str("outcome", string(o.Outcome)).
			Str("before", o.Before).
			Str("after", o.After).
			Str("evidence", o.Evidence).
			Msg("command entity outcome")
	}
}

// marshalCommandResponse renders the tool's JSON response. `unchanged`
// outcomes carry the [UNVERIFIED] marker in their note (D4).
func marshalCommandResponse(rep hatwin.CommandReport, guidance commandResponseGuidance) string {
	for i := range rep.Outcomes {
		if rep.Outcomes[i].Outcome == hatwin.OutcomeUnchanged {
			rep.Outcomes[i].Note = "[UNVERIFIED] " + rep.Outcomes[i].Note
		}
	}
	payload := struct {
		hatwin.CommandReport
		StateCheck string `json:"state_check,omitempty"`
	}{CommandReport: rep, StateCheck: guidance.StateCheck}

	b, err := json.Marshal(payload)
	if err != nil {
		// Never fail after a send: degrade to the minimal honest payload.
		return fmt.Sprintf(`{"call_id":%q,"sent":true,"outcomes":"unavailable (marshal failure)"}`, rep.CallID)
	}

	return string(b)
}
