// Package config handles loading and validating the application configuration from YAML.
package config

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Built-in profile names, synthesized by applyProfileDefaults when not
// overridden by the operator.
const (
	profileNameDefault = "default"
	profileNameEdit    = "edit"
)

// Provider and API name constants used across config and llm packages.
const (
	ProviderOllama      = "ollama"
	ProviderOpenRouter  = "openrouter"
	ProviderOpenCode    = "opencode"
	ProviderOpenAICodex = "openai-codex"

	APIChatCompletions = "chat_completions"
	APIAnthropic       = "anthropic"
	APIGoogle          = "google"
	APIResponses       = "responses"
	APIOllama          = "ollama"
)

// Config is the root configuration structure for the agent.
type Config struct {
	ConfigVersion int `yaml:"config_version"`
	// Agent is the agent's identity: an immutable ID (also the key of its
	// vault entry) and an editable display name.
	Agent AgentConfig `yaml:"agent"`
	// Secrets configures where the Secret store's key lives.
	Secrets SecretsConfig `yaml:"secrets"`
	// SecretValues resolves every Secret reference to its value. Runtime-only:
	// the daemon installs it after opening the Secret store; a nil source
	// resolves only `!secret_file` references.
	SecretValues  *SecretSource              `yaml:"-"`
	LLM           LLMConfig                  `yaml:"llm"`
	Channels      ChannelsConfig             `yaml:"channels"`
	MCPServers    map[string]MCPServerConfig `yaml:"mcp_servers"`
	Skills        SkillsConfig               `yaml:"skills"`
	Sessions      SessionsConfig             `yaml:"sessions"`
	Compaction    CompactionConfig           `yaml:"compaction"`
	Logging       LoggingConfig              `yaml:"logging"`
	HomeAssistant HomeAssistantConfig        `yaml:"home_assistant"`
	// DefaultProfile names the active MCP exposure profile resolved at boot. It
	// is the lowest-precedence source (overridden by the --profile flag and the
	// READY_DEFAULT_PROFILE env var). Defaults to "default" via applyDefaults.
	// The legacy mcp_profile YAML key is rejected by name (renamed selector).
	DefaultProfile string `yaml:"default_profile"`
	// Profiles maps a profile name to its cross-cutting injection rules. The
	// built-in "default" and "edit" profiles are synthesized by applyDefaults
	// when not overridden here. Per-owner tool gating (allow/deny) lives on
	// each policy owner's own config block instead — see OwnerProfileConfig.
	Profiles map[string]ProfileConfig `yaml:"profiles"`
	// BuiltinTools holds the gating profiles for READY's own internal tools
	// (wait, skills-load/unload, read-skill-asset, list-skill-assets,
	// and future internal tool packages that don't belong to an external
	// subsystem).
	BuiltinTools BuiltinToolsConfig `yaml:"builtin_tools"`
	// OAuth holds global credential-storage configuration shared by every MCP
	// server that opts into OAuth via its own MCPServerConfig.OAuth block.
	OAuth OAuthConfig `yaml:"oauth"`

	// ElectedPath is the config file this Config was loaded from, as
	// recorded by ApplyRuntimeHome during Load. Empty for Config values
	// constructed directly in Go (e.g. in tests).
	ElectedPath string `yaml:"-"`
	// Origin records how ElectedPath was elected (flag/local/global); it is
	// meaningful only when the caller went through config.SelectAgentDir.
	Origin OriginKind `yaml:"-"`
	// RuntimeHome is the directory containing the elected config file —
	// every runtime path of the run resolves against it:
	// sessions, log segments, the runtime skill copy. No env or flag
	// override exists for it (documented non-goal). Empty for direct
	// constructions until ApplyRuntimeHome runs.
	RuntimeHome string `yaml:"-"`

	// runtimeHomesApplied records that applyRuntimeHomePaths already ran
	// (ApplyRuntimeHome during Load), so applyDefaults does not re-resolve
	// the runtime paths against the (already doubled-in) runtime home.
	runtimeHomesApplied bool

	// legacyProfileFields records pre-refactor `profiles.<name>.{allow,deny,
	// edit_tools}` keys detected verbatim in the source YAML during Load, so
	// Validate can reject the old shape with a precise error. Not populated
	// when constructing a Config directly in Go (e.g. in tests).
	legacyProfileFields []string

	// removedFields records keys that were removed or renamed, detected
	// verbatim in the source YAML during Load (yaml.Unmarshal would silently
	// drop them), each rendered as an operator-facing message naming the
	// replacement. Not populated when constructing a Config directly in Go.
	removedFields []string
}

// ProfileConfig defines the cross-cutting concerns of an MCP profile that
// apply to the system prompt as a whole rather than to any one policy owner's
// tool surface.
type ProfileConfig struct {
	// InjectSkills lists skill names whose SKILL.md is injected into the initial
	// system prompt under this profile.
	InjectSkills []string `yaml:"inject_skills"`
	// DiscloseDeniedTools, when true, loads the tool names blocked by this
	// profile's per-owner gating rules into the agent's tool-name resolver as a
	// distinct blocked index: a model call naming a blocked tool (exact,
	// prefix-corrected, or bare-wire form, when unambiguous) receives a fixed
	// structured "policy wall" observation instead of a generic unregistered-tool
	// failure. Default false: denied tools stay invisible and hallucinated calls
	// fail exactly as they do today. Operator-chosen trade-off: enabling maps
	// part of the gated surface (the denied names) to the model.
	DiscloseDeniedTools bool `yaml:"disclose_denied_tools"`
}

// OwnerProfileConfig holds allow/deny glob rules for one policy owner (an MCP
// server, an internal Eino-tool package, or the built-in tools bucket) under
// one profile name. Allow/Deny are glob patterns (only `*` is significant)
// matched against the owner's own wire name (unprefixed); Deny takes
// precedence rules: allow and deny are mutually exclusive on one profile
// entry — an entry is either an allow-list or a deny-list, never both (an
// absent allow with a deny list means "allow everything except the denied
// names", so "all except X" is expressed with deny alone). An owner that
// does not declare the active profile name at
// all is fail-closed: it contributes zero tools under that profile.
type OwnerProfileConfig struct {
	Allow []string `yaml:"allow"`
	Deny  []string `yaml:"deny"`
}

// BuiltinToolsConfig holds the gating profiles for READY's own internal
// tools (not owned by any MCP server or Eino-tool package).
type BuiltinToolsConfig struct {
	Profiles map[string]OwnerProfileConfig `yaml:"profiles"`
}

// ProfileEnvName is the environment variable carrying the boot-time profile
// override (second precedence, below --profile and above default_profile).
// The removed READY_MCP_PROFILE name is never read — a set legacy env is
// silently ignored, never aliased.
const ProfileEnvName = "READY_DEFAULT_PROFILE"

// ConfigManagerToolKey is the builtin wire name of the config-manager tool.
// It lives here, next to the configuration definitions, because the
// synthesized builtin owner policies must reference it; the tools package
// imports the constant instead of redefining the name.
const ConfigManagerToolKey = "config-manager"

const configManagerToolKey = ConfigManagerToolKey

// LoggingConfig holds structured logging settings.
type LoggingConfig struct {
	// Level sets the minimum level of the log file and, when ConsoleLevel is
	// empty, of the console log: trace, debug, info, warn, error (default: info).
	Level string `yaml:"level"`
	// ConsoleLevel, when non-empty, sets the minimum level of the console
	// (stderr) log only, independently of Level. "off" silences the console
	// except for fatal records and log-file failure/recovery notices.
	ConsoleLevel string `yaml:"console_level"`
	// Format selects the stderr output format: console (default) or json.
	Format string `yaml:"format"`
	// FilePath, when non-empty, enables the JSON log file: a rotating series of
	// dated segments (<base>-YYYY-MM-DD[.<n>]<ext>) in the directory of
	// FilePath, which is created (0700) when missing. A path ending with a
	// separator, or naming an existing directory, means <dir>/ready.log.
	// See the architecture decision records.
	FilePath string `yaml:"file_path"`
	// FileRotation bounds the segment series; ignored when FilePath is empty.
	FileRotation FileRotationConfig `yaml:"file_rotation"`
	// ZMQPayloadsEnabled gates DEBUG-level payload events emitted by the
	// `zmq-listener` / `zmq-session` scopes (request_payload, response_payload,
	// auth_challenge_sent). Default false. Even with debug level enabled,
	// payload bodies and nonces are suppressed unless this flag is true.
	// The daemon's auth_token value is NEVER logged at any level regardless.
	ZMQPayloadsEnabled bool `yaml:"zmq_payloads_enabled"`
	// Thoughts, when true, emits ReAct thought/decision/observation records at
	// DEBUG level for every RunMessages invocation. Requires at least one sink
	// (the log file via level, or the console via console_level) at debug or
	// trace; otherwise a single boot-time WARN is emitted and no thought
	// records are written. Default: false.
	// See the ReAct thought-logging documentation for what each record carries.
	Thoughts bool `yaml:"thoughts"`
	// ProfileDecisions, when true, emits one DEBUG log entry per tool at boot
	// (and after each MCP reconnect) naming the owner, wire name, matched
	// pattern and effect (allow/deny/fail-closed). Default: false.
	ProfileDecisions bool `yaml:"profile_decisions"`
}

// ConsoleLevelOff is the logging.console_level value that silences the
// console log (fatal records and log-file failure notices still appear).
const ConsoleLevelOff = "off"

// Log-file rotation defaults (log-file-rotation capability).
const (
	DefaultLogMaxSegmentSize ByteSize = 10 << 20
	DefaultLogMaxTotalSize   ByteSize = 100 << 20
	DefaultLogRetentionDays           = 7
)

// FileRotationConfig configures log-file rotation and retention. Pointer
// fields distinguish an absent value (nil → default) from an explicit zero,
// which is rejected by ValidateLogging.
type FileRotationConfig struct {
	// MaxSegmentSize rotates the active segment before it would exceed this
	// size (default 10MB).
	MaxSegmentSize *ByteSize `yaml:"max_segment_size"`
	// RetentionDays keeps the segments of the last N local calendar days,
	// today included (default 7).
	RetentionDays *int `yaml:"retention_days"`
	// MaxTotalSize caps the total size of all segments (default 100MB).
	MaxTotalSize *ByteSize `yaml:"max_total_size"`
}

// EffectiveMaxSegmentSize returns MaxSegmentSize or its default.
func (r FileRotationConfig) EffectiveMaxSegmentSize() int64 {
	if r.MaxSegmentSize == nil {
		return int64(DefaultLogMaxSegmentSize)
	}

	return int64(*r.MaxSegmentSize)
}

// EffectiveMaxTotalSize returns MaxTotalSize or its default.
func (r FileRotationConfig) EffectiveMaxTotalSize() int64 {
	if r.MaxTotalSize == nil {
		return int64(DefaultLogMaxTotalSize)
	}

	return int64(*r.MaxTotalSize)
}

// EffectiveRetentionDays returns RetentionDays or its default.
func (r FileRotationConfig) EffectiveRetentionDays() int {
	if r.RetentionDays == nil {
		return DefaultLogRetentionDays
	}

	return *r.RetentionDays
}

// LLMConfig holds the providers-plus-references LLM registry configuration.
// Providers is a map of named provider connections (each holding the
// connection's provider type, wire api, credentials, and base_url once).
// Default names the active model at boot as a `<provider>::<model>` reference;
// Failover is an ordered fallback chain of the same reference form. Every
// provider name referenced by Default or Failover must exist as a key in
// Providers; the model-id portion is forwarded to the provider verbatim.
// FailoverAttemptTimeout, FailoverRetries, FailoverRetryBackoff, and Recovery
// configure the runtime failover orchestrator; all default fully when omitted
// (applyLLMDefaults), while explicit zero retains its documented disable
// semantics where supported.
type LLMConfig struct {
	Default   string                       `yaml:"default"`
	Failover  []string                     `yaml:"failover"`
	Providers map[string]LLMProviderConfig `yaml:"providers"`
	// OAuthPath is copied from the root oauth.path after defaults are applied.
	// It is runtime wiring, not a second YAML configuration surface.
	OAuthPath string `yaml:"-"`
	// Secrets resolves the providers' api_key references. Runtime-only: set
	// by the daemon after decoding; a nil source resolves only `!secret_file`.
	Secrets *SecretSource `yaml:"-"`

	// FailoverAttemptTimeout bounds the time until a streaming candidate
	// produces its first token or chunk. Nil means the 45s default; an explicit
	// zero disables the attempt budget and leaves only the outer turn deadline.
	FailoverAttemptTimeout *time.Duration `yaml:"failover_attempt_timeout"`

	// FailoverRetries is the number of same-candidate retries after a transient
	// failure. Nil means 2; an explicit zero disables in-turn retries. Values
	// above maxFailoverRetries are rejected to bound cost amplification.
	FailoverRetries *int `yaml:"failover_retries"`

	// FailoverRetryBackoff is the base exponential delay between retries. Nil
	// means 2s; it doubles per retry and is capped at 30s by the orchestrator.
	FailoverRetryBackoff *time.Duration `yaml:"failover_retry_backoff"`

	// Recovery configures the two cooldown ladders, the shared backoff
	// factor, the readiness-probe timeout, and the idle-probe window of the
	// failover orchestrator. Nil-safe: defaults are applied by
	// applyLLMDefaults, so a fully-omitted block behaves identically to an
	// explicit default one.
	Recovery *LLMRecoveryConfig `yaml:"recovery"`

	// legacyLLMFields records superseded `llm.{provider,model,models}` keys
	// detected verbatim in the source YAML during Load, so Validate can reject
	// the old shape with a migration error. Not populated when constructing a
	// Config directly in Go (e.g. in tests).
	legacyLLMFields []string
}

// LLMRecoveryConfig configures the runtime failover orchestrator's cooldown
// and recovery behaviour (change llm-failover-orchestrator, the design doc/D6).
// Every field defaults when omitted (applyLLMDefaults).
type LLMRecoveryConfig struct {
	// Soft is the cooldown ladder for transient failure classes (HTTP 429
	// rate limiting, 5xx / provider overload). Defaults: 30s initial, 5m max.
	Soft LadderConfig `yaml:"soft"`

	// Hard is the cooldown ladder for hard failure classes (auth, catalog,
	// quota, transport, attempt-budget expiry). Defaults: 5m initial, 1h max.
	Hard LadderConfig `yaml:"hard"`

	// BackoffFactor multiplies the previous cooldown on each successive
	// demotion of the same candidate, per ladder. Must exceed 1. Default 2.
	BackoffFactor float64 `yaml:"backoff_factor"`

	// ProbeTimeout bounds each readiness probe (boot and recovery). Default
	// 10s.
	ProbeTimeout time.Duration `yaml:"probe_timeout"`

	// IdleProbeWindow is how recently agent traffic must have occurred for
	// the background recovery worker to probe cooling candidates. Default
	// 15m.
	IdleProbeWindow time.Duration `yaml:"idle_probe_window"`
}

// LadderConfig is one cooldown ladder: an initial cooldown, doubled (per
// BackoffFactor) on each successive demotion, bounded by MaxCooldown, reset
// on the candidate's next success.
type LadderConfig struct {
	InitialCooldown time.Duration `yaml:"initial_cooldown"`
	MaxCooldown     time.Duration `yaml:"max_cooldown"`
}

// LLMProviderConfig holds the connection settings for a single named provider
// instance. The instance name (the YAML map key under llm.providers) is
// distinct from the Provider type, so one type MAY be configured more than
// once (e.g. two opencode gateways with different base_urls). The model-id is
// not part of this struct — it lives only in the `<provider>::<model>`
// reference string in llm.default / llm.failover.
type LLMProviderConfig struct {
	// Name is backfilled from the YAML map key by applyLLMDefaults; it is a
	// diagnostic label only and is not parsed from YAML.
	Name     string `yaml:"-"`
	Provider string `yaml:"provider"`
	API      string `yaml:"api"`
	// APIKey references the provider key in the Secret store
	// (`!secret <name>`) or an external file (`!secret_file <path>`).
	APIKey  SecretRef `yaml:"api_key"`
	BaseURL string    `yaml:"base_url"`

	// KeepAlive is an Ollama-only knob controlling how long the model stays
	// loaded in memory after a request (e.g. "30m"). A nil value (field
	// absent) preserves Ollama's own default; an explicit zero ("0s") requests
	// unload after every call. Setting this on a non-ollama provider is a boot
	// error — see Validate. The value applies to every model referenced
	// through this provider instance.
	KeepAlive *time.Duration `yaml:"keep_alive"`

	// Options is an Ollama-only block of generation-time tuning knobs
	// (temperature, num_predict, num_ctx, …). A nil value (block absent)
	// preserves Ollama's defaults. Setting this on a non-ollama provider is a
	// boot error — see Validate. The value applies to every model referenced
	// through this provider instance.
	Options *OllamaOptions `yaml:"options"`

	// ContextWindow is the model's context-window size in tokens, when the
	// operator declares it (tui-usage-observability). It is the denominator
	// for the TUI's context-occupancy percentage and the /context free-space
	// estimate; when absent, both are absent rather than estimated from a
	// guessed default. Optional on every provider type; must be a positive
	// integer when set. Ollama operators must keep options.num_ctx consistent
	// with this value or Ollama's silent truncation invalidates the
	// percentage (boot WARN on mismatch).
	ContextWindow int `yaml:"context_window"`
}

// OllamaOptions mirrors the subset of Ollama's api.Options relevant to chat
// generation. Every field is optional; a nil pointer means "not set" and is
// not forwarded to Ollama, preserving its default. Pointer fields give a
// clean "omitted vs explicitly zero" distinction (which matters for
// num_predict and seed).
type OllamaOptions struct {
	Temperature   *float32 `yaml:"temperature"`
	TopP          *float32 `yaml:"top_p"`
	TopK          *int     `yaml:"top_k"`
	NumPredict    *int     `yaml:"num_predict"`
	NumCtx        *int     `yaml:"num_ctx"`
	Seed          *int     `yaml:"seed"`
	RepeatPenalty *float32 `yaml:"repeat_penalty"`
	NumThread     *int     `yaml:"num_thread"`
}

// modelRefSeparator is the separator between a provider instance name and a
// model-id in a `<provider>::<model>` reference. Chosen because real model IDs
// contain single `:` (e.g. `gemma4:31b`) but never `::` — see the design doc.
const modelRefSeparator = "::"

// ParseModelReference splits a `<provider>::<model>` reference on the first
// `::` occurrence. The provider name is the part before the separator; the
// model-id is the remainder verbatim (so a model-id containing a single `:`,
// e.g. `gemma4:31b`, round-trips unchanged). It returns an error if the
// separator is missing, the provider name is empty, or the model-id is empty.
func ParseModelReference(ref string) (providerName, modelID string, err error) {
	idx := strings.Index(ref, modelRefSeparator)
	if idx < 0 {
		return "", "", fmt.Errorf("model reference %q is missing the '::' separator (expected <provider>::<model>)", ref) // user-facing config error
	}

	providerName = ref[:idx]
	modelID = ref[idx+len(modelRefSeparator):]

	if providerName == "" {
		return "", "", fmt.Errorf("model reference %q has an empty provider name", ref) // user-facing config error
	}

	if modelID == "" {
		return "", "", fmt.Errorf("model reference %q has an empty model-id", ref) // user-facing config error
	}

	return providerName, modelID, nil
}

// ChannelsConfig groups configuration for all supported channels.
type ChannelsConfig struct {
	CLI      CLIChannelConfig      `yaml:"cli"`
	ZMQCLI   ZMQCLIChannelConfig   `yaml:"zmqcli"`
	Serial   SerialChannelConfig   `yaml:"serial"`
	Telegram TelegramChannelConfig `yaml:"telegram"`

	// zmqCLIEnabledSet records whether the operator explicitly wrote
	// channels.zmqcli.enabled in the YAML (set by UnmarshalYAML). applyDefaults
	// uses it so the zmq-first default never overrides an explicit
	// `enabled: false`. Zero for direct (in-memory) constructions.
	zmqCLIEnabledSet bool
}

// UnmarshalYAML decodes the channels block and records whether the zmqcli
// section carried an explicit `enabled` key, so the zmq-first default
// applied by applyDefaults (zmqcli on when a token is configured, legacy
// cli off) never overrides an explicit operator choice.
func (c *ChannelsConfig) UnmarshalYAML(value *yaml.Node) error {
	type rawChannelsConfig ChannelsConfig // strips the UnmarshalYAML method
	var raw rawChannelsConfig
	// value.Decode uses a fresh, non-strict decoder, so the channels subtree
	// would escape the strict decode of decodeConfigTarget: re-marshal the
	// node and decode it with the same unknown-fields-are-errors rule.
	channelData, err := yaml.Marshal(value)
	if err != nil {
		return fmt.Errorf("failed to decode the channels block: %w", err)
	}
	if err := strictDecode(channelData, &raw); err != nil {
		return fmt.Errorf("channels: %w", err)
	}

	*c = ChannelsConfig(raw)
	c.zmqCLIEnabledSet = mappingKeyPresent(value, "zmqcli", "enabled")

	return nil
}

// mappingKeyPresent reports whether the mapping node carries a `section` key
// whose value is a mapping containing a `key` entry (e.g. channels.zmqcli.enabled).
func mappingKeyPresent(node *yaml.Node, section, key string) bool {
	if node.Kind != yaml.MappingNode {
		return false
	}

	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Value != section {
			continue
		}

		sub := node.Content[i+1]
		if sub.Kind != yaml.MappingNode {
			return false
		}

		for j := 0; j+1 < len(sub.Content); j += 2 {
			if sub.Content[j].Value == key {
				return true
			}
		}

		return false
	}

	return false
}

// ZMQCLIChannelConfig holds settings for the ZMQ-attached CLI channel. When
// enabled, this channel replaces the legacy stdin-based CLI: the daemon binds
// a ROUTER socket at Bind and accepts authenticated REQ clients (the `chat`
// subcommand). The channel still reports Name() == "cli" so per-channel
// agent / formatter wiring is unchanged.
type ZMQCLIChannelConfig struct {
	Enabled bool `yaml:"enabled"`
	// Bind is the ZMQ endpoint to bind the ROUTER socket to.
	// Default: tcp://127.0.0.1:7600. Use tcp://127.0.0.1:0 for an ephemeral
	// port (primarily useful in tests; Listener.Addr() reports the resolved
	// address).
	Bind string `yaml:"bind"`
	// AuthToken references the shared secret used to derive HMAC-SHA256 over
	// the per-connection challenge nonce. A reference is required when Enabled
	// is true; the value lives in the Secret store (or a `!secret_file`).
	AuthToken SecretRef `yaml:"auth_token"`
	// ConnectionIdleTimeout bounds the per-session idle window. After this duration
	// without a message from the active client, the daemon releases the
	// session lock and emits Response{Status:"error", ErrorCode:"session_timeout"}.
	// Default: 15m.
	ConnectionIdleTimeout time.Duration `yaml:"connection_idle_timeout"`
	// ReplyTimeout is a safety net, separate from SessionTimeout: it bounds
	// how long a session may sit with a reply pending (the agent is actively
	// computing) before the watchdog gives up on it and releases the lock —
	// covering a dead client / crashed dispatcher that never calls Send.
	// It must stay comfortably above the daemon's own agent-run budget
	// (agentRunTimeout + formatterTimeout in cmd/serve.go) or the watchdog
	// will kill legitimately long-running requests before the agent replies.
	// buildChannelManager clamps this up automatically if it is too low, so
	// this is mostly a ceiling for "give up sooner" tuning. Default: 400s.
	ReplyTimeout time.Duration `yaml:"reply_timeout"`
	// LivenessTimeout (change zmq-session-liveness) bounds how long the
	// daemon waits for a client heartbeat before releasing the session lock:
	// the TUI sends a ping every LivenessTimeout/3, and a session whose
	// pings stop (killed client, closed terminal window, laptop sleep) is
	// released with reason client_unresponsive. A session that never pinged
	// (older clients, --exec one-shots) is exempt — the existing
	// connection_idle_timeout / reply_timeout watchdogs cover it. Default: 45s.
	LivenessTimeout time.Duration `yaml:"liveness_timeout"`
	// FormatterSkill names a skill whose SKILL.md is used as the system prompt
	// for a post-step LLM call that reformats the agent's response for this
	// channel. Leave empty to skip formatting. Always post-step.
	FormatterSkill string `yaml:"formatter_skill"`
	// FormatterPostStep selects the formatting mode:
	// true  — invoke the formatter as a separate LLM call after the agent response.
	// false (default) — inject the formatter SKILL.md into the agent's system prompt.
	FormatterPostStep bool `yaml:"formatter_post_step"`
	// Bootstrap lists skill names to preload into every session on this channel.
	// Their SKILL.md and static assets are injected as a system message before
	// each LLM call. A skill in Bootstrap must not also be the FormatterSkill.
	Bootstrap []string `yaml:"bootstrap"`
	// SessionReset optionally overrides fields of sessions.reset for this
	// channel. Evaluated only when a new Connection begins (connection-
	// oriented channel).
	SessionReset *SessionResetConfig `yaml:"session_reset"`
}

// CLIChannelConfig holds CLI-specific settings.
type CLIChannelConfig struct {
	Enabled           bool                `yaml:"enabled"`
	FormatterSkill    string              `yaml:"formatter_skill"`
	FormatterPostStep bool                `yaml:"formatter_post_step"`
	Bootstrap         []string            `yaml:"bootstrap"`
	SessionReset      *SessionResetConfig `yaml:"session_reset"`
}

// SerialChannelConfig holds serial/PETSCII-specific settings.
type SerialChannelConfig struct {
	Enabled           bool                `yaml:"enabled"`
	Port              string              `yaml:"port"`
	BaudRate          int                 `yaml:"baud_rate"`
	FormatterSkill    string              `yaml:"formatter_skill"`
	FormatterPostStep bool                `yaml:"formatter_post_step"`
	Bootstrap         []string            `yaml:"bootstrap"`
	SessionReset      *SessionResetConfig `yaml:"session_reset"`
}

// TelegramChannelConfig holds Telegram bot settings.
type TelegramChannelConfig struct {
	Enabled           bool                `yaml:"enabled"`
	Token             SecretRef           `yaml:"token"`
	AllowFrom         []string            `yaml:"allow_from"`
	FormatterSkill    string              `yaml:"formatter_skill"`
	FormatterPostStep bool                `yaml:"formatter_post_step"`
	Bootstrap         []string            `yaml:"bootstrap"`
	SessionReset      *SessionResetConfig `yaml:"session_reset"`
}

// HomeAssistantConfig holds settings for the HA WebSocket digital twin and
// autonomic alerting. Set enabled: true and configure websocket to activate.
// The twin starts non-blocking: on timeout or unreachability it continues in
// a degraded state and recovers automatically on reconnect.
type HomeAssistantConfig struct {
	// Enabled gates all HA WebSocket functionality. Default: false.
	Enabled    bool               `yaml:"enabled"`
	WebSocket  HAWebSocketConfig  `yaml:"websocket"`
	Monitoring HAMonitoringConfig `yaml:"monitoring"`
	// Profiles gates the Twin's own tools (ha_state_query) per profile name,
	// keyed by wire name, same shape and fail-closed semantics as
	// MCPServerConfig.Profiles. The Twin and the home_assistant MCP server
	// (if configured) are independent policy owners.
	Profiles map[string]OwnerProfileConfig `yaml:"profiles"`
	// StaleThresholdStates is how long since the last WS activity before the
	// home-state-query payload sets stale: true (entity data still returned).
	// Default: 60s.
	StaleThresholdStates time.Duration `yaml:"stale_threshold_states"`
	// HardStaleThreshold is how long since the last WS activity before
	// home-state-query refuses to serve data and returns ErrTwinStale.
	// Default: 300s.
	HardStaleThreshold time.Duration `yaml:"hard_stale_threshold"`
	// OutstandingPingsThreshold is the number of unacknowledged pings that
	// triggers a force-reconnect of the WS (zombie-connection detection).
	// Default: 2.
	OutstandingPingsThreshold int `yaml:"outstanding_pings_threshold"`
	// Commands configures the twin's write path (ha_command tool,
	// twin-native-actuation D12). All fields optional with sensible defaults.
	Commands HACommandsConfig `yaml:"commands"`
}

// HACommandsConfig configures the ha_command write path.
// home_assistant.commands.{domains, verify_timeout, max_targets}.
type HACommandsConfig struct {
	// Domains selects the active intent domains (D5). Absent (nil) ⇒ every
	// vocabulary domain except siren; an explicit list is taken literally
	// (an empty list activates none); a listed domain unknown to the
	// vocabulary produces a boot WARN and is ignored. lock and
	// alarm_control_panel are NEVER part of the vocabulary, whatever this
	// list says.
	Domains []string `yaml:"domains"`
	// VerifyTimeout bounds the sync-mode verification window. Default: 5s,
	// clamped to 1s–30s. It is further bounded at call time by the remaining
	// turn deadline.
	VerifyTimeout time.Duration `yaml:"verify_timeout"`
	// MaxTargets caps how many entities one command may target. Default: 20.
	MaxTargets int `yaml:"max_targets"`
}

// HAWebSocketConfig holds HA WS endpoint and authentication.
// A Token reference is required when enabled is true.
type HAWebSocketConfig struct {
	// URL is the HA WebSocket endpoint, e.g. "ws://homeassistant.local:8123/api/websocket".
	// Secret-capable plain field: a literal is kept in clear, a `!secret`
	// reference is resolved at boot (dynamic per instance).
	URL SecretString `yaml:"url"`
	// Token references the Long-Lived Access Token.
	Token SecretRef `yaml:"token"`
}

// HAMonitoringConfig holds twin operational parameters.
type HAMonitoringConfig struct {
	// SyncTimeout is how long to wait at boot for the initial sync before
	// continuing degraded. Default: 15s.
	SyncTimeout time.Duration `yaml:"sync_timeout"`
	// PingInterval controls WS keep-alive pings. Default: 30s.
	PingInterval time.Duration `yaml:"ping_interval"`
	// TopologyDebounce is the wait before emitting a topology-change signal
	// after a registry-update event. Default: 500ms.
	TopologyDebounce time.Duration `yaml:"topology_debounce"`
	// Reconnect policy for the WS connection.
	InitialBackoff time.Duration `yaml:"initial_backoff"`
	MaxBackoff     time.Duration `yaml:"max_backoff"`
	BackoffFactor  float64       `yaml:"backoff_factor"`
	// MaxRetries caps reconnect attempts. 0 = infinite.
	MaxRetries int `yaml:"max_retries"`
}

// MCPServerConfig holds connection info for an MCP server.
type MCPServerConfig struct {
	// URL is the server endpoint. Secret-capable plain field: a literal is
	// kept in clear (endpoints without credentials), a `!secret` reference
	// is resolved at boot — dynamic per instance (some deployments embed a
	// token in the query string, others are plain).
	URL SecretString `yaml:"url"`
	// Headers maps a header name to a reference to its value: every header
	// value is treated as a Secret.
	Headers map[string]SecretRef `yaml:"headers"`
	// ResolvedHeaders holds the header values resolved from Headers' Secret
	// references. Runtime-only and in-memory: filled by ResolveHeaders after
	// decoding, never marshalled, never logged.
	ResolvedHeaders map[string]string `yaml:"-"`
	// Profiles gates this server's tools per profile name, keyed by wire name
	// (unprefixed). An owner that does not declare the active profile is
	// fail-closed: it contributes zero tools under that profile.
	Profiles map[string]OwnerProfileConfig `yaml:"profiles"`
	// ToolPrefix, when non-nil, overrides the server key used as a prefix in
	// tool names exposed to the LLM. The full display name is built as
	// "<prefix>__<wire_name>" using "__" as the separator.
	//
	// Three modes:
	//   nil (absent in YAML) → use the server's map key as the prefix
	//   ""  (empty string)   → no prefix; tool names are exposed as-is
	//   "ha" (non-empty)     → use "ha" as the prefix
	//
	// The field is a pointer so that the absent/zero-value distinction is
	// preserved after YAML unmarshalling.
	ToolPrefix *string `yaml:"tool_prefix,omitempty"`
	// Transport selects the MCP transport protocol.
	// Supported values: "streamable_http" (default, required by Home Assistant)
	// and "sse" (legacy, for older MCP servers).
	Transport string `yaml:"transport"`
	// Timeout bounds any single MCP request (Initialize, ListTools, CallTool).
	// Zero means use the package default (30s).
	Timeout time.Duration `yaml:"timeout"`
	// ExpectedTools is an optional list of tool names that must be present
	// after discovery. Missing tools are logged as warnings — they never block
	// startup, because HA MCP exposes a different set of tools per version.
	ExpectedTools []string `yaml:"expected_tools"`
	// Reconnect configures the background reconnection policy.
	Reconnect MCPReconnectConfig `yaml:"reconnect"`
	// HealthCheck configures the periodic connectivity check.
	HealthCheck MCPHealthCheckConfig `yaml:"health_check"`
	// OAuth, when non-nil, declares that this server requires OAuth 2.0
	// authorization. A server without this block behaves exactly as before
	// (static headers only, or no authentication).
	OAuth *MCPOAuthConfig `yaml:"oauth,omitempty"`
}

// Accepted MCPOAuthConfig.Grant values.
const (
	grantAuto              = "auto"
	grantAuthorizationCode = "authorization_code"
	grantClientCredentials = "client_credentials"
)

// isValidGrant reports whether grant is one of the accepted
// MCPOAuthConfig.Grant values.
func isValidGrant(grant string) bool {
	switch grant {
	case grantAuto, grantAuthorizationCode, grantClientCredentials:
		return true
	default:
		return false
	}
}

// MCPReconnectConfig configures exponential-backoff reconnection.
type MCPReconnectConfig struct {
	// MaxRetries caps the number of reconnect attempts. 0 means infinite.
	MaxRetries int `yaml:"max_retries"`
	// InitialBackoff is the delay before the first retry.
	InitialBackoff time.Duration `yaml:"initial_backoff"`
	// MaxBackoff caps the delay between retries.
	MaxBackoff time.Duration `yaml:"max_backoff"`
	// BackoffFactor is the exponential multiplier applied to InitialBackoff.
	BackoffFactor float64 `yaml:"backoff_factor"`
}

// MCPHealthCheckConfig configures the background health-check loop.
type MCPHealthCheckConfig struct {
	Enabled  bool          `yaml:"enabled"`
	Interval time.Duration `yaml:"interval"`
}

// OAuthConfig holds global OAuth credential-storage settings shared by every
// MCP server that declares its own oauth block. token_store currently
// accepts only "file" (D6 in the mcp-oauth-authorization); any
// other value, including "keychain", is rejected explicitly by Validate
// rather than silently falling back.
type OAuthConfig struct {
	// Path is the directory the file backend stores one JSON record per
	// server under. It is runtime-derived state, never author config: the
	// oauth.path YAML key is rejected and the directory is always
	// <agent-dir>/oauth (filled in by ApplyRuntimeHome; empty for a Config
	// constructed directly in Go until the caller resolves it).
	Path string `yaml:"-"`
	// RedirectPort is the fixed loopback port `ready login` binds for the
	// authorization-code callback. Frozen into the persisted client
	// registration, so it must stay stable across logins. Default: 7601.
	RedirectPort int `yaml:"redirect_port"`
}

// MCPOAuthConfig declares that a single MCP server requires OAuth 2.0
// authorization and how its client credentials are supplied. There is
// deliberately no scopes field: scopes are determined by the authorization
// server, not by configuration.
type MCPOAuthConfig struct {
	// Grant selects the OAuth grant: "auto" (default), "authorization_code",
	// or "client_credentials". Under "auto" the client_credentials grant is
	// used only when ClientID and a secret are configured and the
	// authorization server advertises it; otherwise authorization_code.
	Grant string `yaml:"grant"`
	// ClientID is used for the client_credentials grant, or as a
	// pre-registered client for authorization_code (skipping Dynamic Client
	// Registration). Optional for authorization_code servers that support DCR.
	ClientID string `yaml:"client_id"`
	// ClientSecret references the client secret. A reference is required for
	// the client_credentials grant.
	ClientSecret SecretRef `yaml:"client_secret"`
}

// SkillsConfig holds skill-related settings.
type SkillsConfig struct {
	Path string `yaml:"path"`
	// Sync is the library-sync policy: auto (default) applies the updates of
	// untouched runtime files; notify only reports them; off skips the sync
	// entirely. Any other value fails boot.
	Sync string `yaml:"sync"`
	// LibraryPath is the shipped skill library the runtime copy is synced
	// from (default ~/.local/share/ready/library/skills/; absolute as-is, relative
	// resolved against the runtime home). The installer owns that directory
	// and replaces it on every install; the daemon never writes into it.
	LibraryPath string `yaml:"library_path"`
	HotReload   bool   `yaml:"hot_reload"`
}

// SyncMode returns the effective library-sync mode: "auto" when unset.
func (s SkillsConfig) SyncMode() string {
	if s.Sync == "" {
		return SkillsSyncAuto
	}

	return s.Sync
}

// validateProfileNamed checks that the configured mcp_profile (when present)
// is declared under profiles.
func (c *Config) validateProfileNamed() error {
	if c.DefaultProfile != "" {
		if _, ok := c.Profiles[c.DefaultProfile]; !ok {
			return fmt.Errorf("default_profile %q is not declared under profiles (built-ins: default, edit)", c.DefaultProfile) // user-facing config error
		}
	}

	return nil
}

// validateSkills boot-time gate of the skills block: the library-sync mode
// value must be one of the recognised ones (empty = auto).
func (c *Config) validateSkills() error {
	if !validSkillsSync(c.Skills.Sync) {
		return fmt.Errorf("skills.sync %q is invalid (must be auto, notify, or off; empty means auto)", c.Skills.Sync) // user-facing config error
	}

	return nil
}

// validSkillsSync reports whether v is a recognised skills.sync value.
func validSkillsSync(v string) bool {
	switch v {
	case "", SkillsSyncAuto, SkillsSyncNotify, SkillsSyncOff:
		return true
	}

	return false
}

// Skills-sync modes: auto applies the updates of untouched
// runtime files; notify only reports them; off skips the sync entirely.
const (
	SkillsSyncAuto   = "auto"
	SkillsSyncNotify = "notify"
	SkillsSyncOff    = "off"
)

// Load reads and parses a YAML configuration file at the given path.
func Load(ctx context.Context, path string) (*Config, error) {
	select {
	case <-ctx.Done():
		return nil, fmt.Errorf("failed to load config: %w", ctx.Err())
	default:
	}

	// A linked config leaf relaxes the directory boundary; reject it before
	// reading.
	if err := RejectLinkedConfig(path); err != nil {
		return nil, err
	}

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("failed to read config file %s: %w (the sole accepted filename is %s)", path, err, ConfigFileName)
		}

		return nil, fmt.Errorf("failed to read config file: %w", err)
	}

	return decodeConfig(ctx, data, path, TargetConfigVersion)
}

// decodeConfig keeps runtime-only defaults and resolved secrets out of the YAML document.
func decodeConfig(ctx context.Context, data []byte, path string, target int) (*Config, error) {
	return decodeConfigTarget(ctx, data, path, target)
}

// decodeConfigTarget decodes and defaults a document at the given schema
// target. Sensitive fields decode only as Secret references (SecretRef), so
// a literal credential is refused here, before any runtime or config write.
func decodeConfigTarget(ctx context.Context, data []byte, path string, target int) (*Config, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("failed to decode config %s: %w", path, err)
	}
	_, version, err := parseVersion(data, path)
	if err != nil {
		return nil, err
	}
	if err = checkVersion(path, version, target); err != nil {
		return nil, err
	}
	var cfg Config
	// Superseded shapes are probed on the raw document BEFORE the strict
	// decode: their keys are no longer struct fields, so the strict decoder
	// would reject them with a generic unknown-field message instead of the
	// operator-facing migration reason. The probes read only the raw YAML.
	// Historical targets (migration source decoding) keep the lenient decode:
	// their keyset is the document's own and the migration steps own its
	// reshaping — only the current target rejects unknown keys.
	if target == TargetConfigVersion {
		superseded := Config{
			legacyProfileFields: detectLegacyProfileFields(data),
			removedFields:       detectRemovedFields(data),
		}
		superseded.LLM.legacyLLMFields = detectLegacyLLMFields(data)
		if err = superseded.validateSupersededShapes(); err != nil {
			return nil, err
		}

		// Strict decode: unknown or misspelled keys are a decoding error, never
		// a silent drop (e.g. `home_assistant.url` instead of the nested
		// `home_assistant.websocket.url` would otherwise disable the twin and
		// vanish without a trace).
		if err = strictDecode(data, &cfg); err != nil {
			if literals := LiteralSecretPaths(data); len(literals) > 0 {
				return nil, fmt.Errorf("%w: %s hold a literal value; a sensitive field takes only `!secret <name>` or `!secret_file <absolute path>` (set the value with `ready secrets set`)",
					ErrInvalidSecretRef, strings.Join(literals, ", "))
			}

			if detail := safeUnknownFieldDetail(err); detail != "" {
				return nil, fmt.Errorf("failed to parse config %s: %w: unknown or misspelled key: %s", path, ErrInvalidConfigDocument, detail)
			}

			return nil, fmt.Errorf("failed to parse config %s (unknown or misspelled keys are rejected — check field names and types): %w: %w", path, ErrInvalidConfigDocument, &privateConfigError{message: "YAML field decoding failed", cause: err})
		}
	} else {
		if err = yaml.Unmarshal(data, &cfg); err != nil {
			if literals := LiteralSecretPaths(data); len(literals) > 0 {
				return nil, fmt.Errorf("%w: %s hold a literal value; a sensitive field takes only `!secret <name>` or `!secret_file <absolute path>` (set the value with `ready secrets set`)",
					ErrInvalidSecretRef, strings.Join(literals, ", "))
			}

			return nil, fmt.Errorf("failed to parse config %s (check field types): %w: %w", path, ErrInvalidConfigDocument, &privateConfigError{message: "YAML field decoding failed", cause: err})
		}

		cfg.legacyProfileFields = detectLegacyProfileFields(data)
		cfg.LLM.legacyLLMFields = detectLegacyLLMFields(data)
		cfg.removedFields = detectRemovedFields(data)
	}

	if err = cfg.ApplyRuntimeHome(path); err != nil {
		return nil, err
	}

	cfg.applyDefaults()

	return &cfg, nil
}

// safeUnknownFieldDetail extracts, from a yaml TypeError, only the entries
// matching "field X not found in type Y": those are generated from the schema
// and by construction carry field and type names (never document values), so
// they are safe to render. Every other entry (type mismatches can embed
// document values) is dropped — the privateConfigError policy holds.
func safeUnknownFieldDetail(err error) string {
	var typeErr *yaml.TypeError
	if !errors.As(err, &typeErr) {
		return ""
	}

	re := regexp.MustCompile(`line \d+: (?:[a-z_]+: )*field \S+ not found in type \S+`)
	var keep []string
	for _, entry := range typeErr.Errors {
		if m := re.FindString(entry); m != "" {
			keep = append(keep, m)
		}
	}

	return strings.Join(keep, "; ")
}

// strictDecode decodes one YAML document refusing fields that do not exist
// in the destination struct: a typo or a misplaced key is an error, never a
// silent drop. Custom UnmarshalYAML implementations re-enter this helper so
// their subtree keeps the same strictness.
func strictDecode(data []byte, out any) error {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)

	return dec.Decode(out)
}

// detectLegacyProfileFields scans the raw YAML for pre-refactor
// `profiles.<name>.{allow,deny,edit_tools}` keys, which are no longer part of
// ProfileConfig and would otherwise be silently dropped by yaml.Unmarshal.
// Returns the sorted list of offending dotted field paths (empty if none).
func detectLegacyProfileFields(data []byte) []string {
	var probe struct {
		Profiles map[string]map[string]yaml.Node `yaml:"profiles"`
	}

	if err := yaml.Unmarshal(data, &probe); err != nil {
		return nil
	}

	var fields []string

	for name, block := range probe.Profiles {
		for _, key := range []string{"allow", "deny", "edit_tools"} {
			if _, ok := block[key]; ok {
				fields = append(fields, fmt.Sprintf("profiles.%s.%s", name, key))
			}
		}
	}

	sort.Strings(fields)

	return fields
}

// removedKeys maps each removed or renamed dotted config key to the
// operator-facing reason naming its replacement (or "removed").
var removedKeys = []struct { //nolint:gochecknoglobals // immutable lookup table of removed config keys
	path   string
	reason string
}{
	{"persistence.plans_path", "removed: the Plan subsystem no longer exists — delete the key and the legacy plans directory (the pre-.ready era)"},
	{"persistence.plan_ttl", "removed: the Plan subsystem no longer exists — delete the key"},
	{"channels.zmqcli.session_timeout", "renamed: use channels.zmqcli.connection_idle_timeout"},
	{"persistence.checkpoints_path", "replaced: use sessions.path — delete the key and the legacy checkpoints directory (no migration)"},
	{"mcp_profile", "renamed: use default_profile — rename the key in the config file (no historical alias)"},
	{"persistence.max_history", "replaced: use compaction.max_turns (turn-count compaction trigger when no context_window is declared)"},
	{"oauth.path", "removed: the OAuth credential store is fixed at <agent-dir>/oauth — delete the key"},
	{"oauth.token_store", "removed: credentials live in the agent's Secret store — delete the key"},
	{"llm.providers.*.api_key_file", "removed: use `api_key: !secret_file /absolute/path` or `api_key: !secret <name>` (see `ready secrets set`)"},
	{"channels.zmqcli.auth_token_file", "removed: use `auth_token: !secret_file /absolute/path` or `auth_token: !secret <name>`"},
	{"home_assistant.websocket.token_file", "removed: use `token: !secret_file /absolute/path` or `token: !secret <name>`"},
	{"mcp_servers.*.oauth.client_secret_file", "removed: use `client_secret: !secret_file /absolute/path` or `client_secret: !secret <name>`"},
}

// detectRemovedFields scans the raw YAML for removed or renamed keys and
// returns one message per key found, in removedKeys order.
func detectRemovedFields(data []byte) []string {
	var root yaml.Node
	if err := yaml.Unmarshal(data, &root); err != nil {
		return nil
	}

	var found []string

	for _, k := range removedKeys {
		if yamlPathExists(&root, strings.Split(k.path, ".")) {
			found = append(found, k.path+" "+k.reason)
		}
	}

	return found
}

// yamlPathExists reports whether the mapping-key path exists under node. A
// "*" segment matches any key of the mapping at that level.
func yamlPathExists(node *yaml.Node, path []string) bool {
	if node.Kind == yaml.DocumentNode && len(node.Content) > 0 {
		node = node.Content[0]
	}

	if len(path) == 0 {
		return true
	}

	if node.Kind != yaml.MappingNode {
		return false
	}

	for i := 0; i+1 < len(node.Content); i += 2 {
		if path[0] == "*" || node.Content[i].Value == path[0] {
			if yamlPathExists(node.Content[i+1], path[1:]) {
				return true
			}
		}
	}

	return false
}

// detectLegacyLLMFields scans the raw YAML for superseded `llm` shapes that
// would be silently dropped by yaml.Unmarshal after the LLMConfig reshape:
// the pre-registry `llm.{provider,model}` keys and the prior registry
// `llm.models` map. Returns the sorted list of offending dotted field paths
// (empty if none).
func detectLegacyLLMFields(data []byte) []string {
	var probe struct {
		LLM map[string]yaml.Node `yaml:"llm"`
	}

	if err := yaml.Unmarshal(data, &probe); err != nil {
		return nil
	}

	var fields []string

	for _, key := range []string{"provider", "model", "models"} {
		if _, ok := probe.LLM[key]; ok {
			fields = append(fields, fmt.Sprintf("llm.%s", key))
		}
	}

	sort.Strings(fields)

	return fields
}

// validateSupersededShapes rejects the legacy and removed keys detected at
// Load, each with a message naming its replacement.
func (c *Config) validateSupersededShapes() error {
	if len(c.legacyProfileFields) > 0 {
		return fmt.Errorf( // user-facing config error
			"config uses the pre-refactor profile shape (found: %s); allow/deny/edit_tools now live under each owner's own profiles block (mcp_servers.<key>.profiles, home_assistant.profiles, builtin_tools.profiles) — see the profile-ownership-refactor/MIGRATION.md",
			strings.Join(c.legacyProfileFields, ", "),
		)
	}

	if len(c.LLM.legacyLLMFields) > 0 {
		return fmt.Errorf( // user-facing config error
			"config uses a superseded llm shape (found: %s); migrate to the providers-plus-references shape: llm.providers (map of named provider connections holding provider/api/api_key/base_url once each), llm.default (active model as <provider>::<model>), optional llm.failover (ordered <provider>::<model> fallback chain)",
			strings.Join(c.LLM.legacyLLMFields, ", "),
		)
	}

	if len(c.removedFields) > 0 {
		return fmt.Errorf("config uses removed keys: %s", strings.Join(c.removedFields, "; ")) // user-facing config error
	}

	return nil
}

// Validate checks that the configuration is semantically correct. It is
// purely structural with respect to secrets: it reads no Secret store, no
// Store key and no secret file. Value rules apply when a value is written.
func (c *Config) Validate() error {
	return c.validate()
}

// validate is the full boot-time gatekeeper.
func (c *Config) validate() error {
	if supErr := c.validateSupersededShapes(); supErr != nil {
		return supErr
	}

	if err := c.validateAgentAndSecrets(); err != nil {
		return err
	}

	if err := c.validateSkills(); err != nil {
		return err
	}

	if err := c.validateLLM(); err != nil {
		return err
	}

	if err := c.validateSessions(); err != nil {
		return err
	}

	if err := c.validateProfileNamed(); err != nil {
		return err
	}

	if err := c.validateOwnerProfiles(); err != nil {
		return err
	}

	if err := c.validateChannelsAndHA(); err != nil {
		return err
	}

	if err := c.validateOAuth(); err != nil {
		return err
	}

	// Agent-directory confinement (per-user-install-agent-isolation): every
	// writable runtime destination must stay inside the agent directory
	// after link resolution — identical rules for boot loading and the
	// config-manager candidate validation. The read-only skill library
	// override (skills.library_path) is the deliberate program-content
	// exception.
	if err := c.validateStateConfined(); err != nil {
		return err
	}

	// A skill used as formatter_skill must not also appear in that channel's
	// bootstrap list — the two mechanisms are mutually exclusive.
	type channelCheck struct {
		name           string
		formatterSkill string
		bootstrap      []string
	}

	channelChecks := []channelCheck{
		{"cli", c.Channels.CLI.FormatterSkill, c.Channels.CLI.Bootstrap},
		{"zmqcli", c.Channels.ZMQCLI.FormatterSkill, c.Channels.ZMQCLI.Bootstrap},
		{"serial", c.Channels.Serial.FormatterSkill, c.Channels.Serial.Bootstrap},
		{"telegram", c.Channels.Telegram.FormatterSkill, c.Channels.Telegram.Bootstrap},
	}

	for _, ch := range channelChecks {
		if ch.formatterSkill == "" {
			continue
		}

		for _, skill := range ch.bootstrap {
			if skill == ch.formatterSkill {
				return fmt.Errorf("channels.%s: formatter_skill %q must not also appear in bootstrap (use one or the other)", ch.name, ch.formatterSkill)
			}
		}
	}

	return nil
}

// validateOwnerProfiles refuses allow and deny on the same profile entry:
// the two lists are mutually exclusive — an entry expresses either an
// allow-list or a deny-list (an absent allow with a deny list means
// "allow everything except the denied names"). Every policy owner (each
// MCP server, the HA digital twin, builtin_tools) follows the same rule.
func (c *Config) validateOwnerProfiles() error {
	owners := make([]struct {
		owner    string
		profiles map[string]OwnerProfileConfig
	}, 0, 2+len(c.MCPServers))
	owners = append(owners,
		struct {
			owner    string
			profiles map[string]OwnerProfileConfig
		}{"home_assistant", c.HomeAssistant.Profiles},
		struct {
			owner    string
			profiles map[string]OwnerProfileConfig
		}{"builtin_tools", c.BuiltinTools.Profiles},
	)
	for _, key := range slices.Sorted(maps.Keys(c.MCPServers)) {
		owners = append(owners, struct {
			owner    string
			profiles map[string]OwnerProfileConfig
		}{"mcp_servers." + key, c.MCPServers[key].Profiles})
	}

	for _, o := range owners {
		for _, name := range slices.Sorted(maps.Keys(o.profiles)) {
			p := o.profiles[name]
			if len(p.Allow) > 0 && len(p.Deny) > 0 {
				return fmt.Errorf("%s.profiles.%s: allow and deny are mutually exclusive — declare one list only (an absent allow with a deny list means \"allow everything except the denied names\")", o.owner, name)
			}
		}
	}

	return nil
}

// validateChannelsAndHA runs the channel/HA enablement and presence rules.
// A sensitive field is present when it carries a reference; the value behind
// it is judged when it is written (secretstore.CheckValue), never here.
func (c *Config) validateChannelsAndHA() error {
	if c.HomeAssistant.Enabled {
		if c.HomeAssistant.WebSocket.URL.Value == "" && !c.HomeAssistant.WebSocket.URL.IsReference() {
			return errors.New("home_assistant.websocket.url is required when home_assistant.enabled is true")
		}
		if !c.HomeAssistant.WebSocket.Token.IsSet() {
			return errors.New("home_assistant.websocket.token (a `!secret` reference) is required when home_assistant.enabled is true")
		}
	}

	if c.Channels.Telegram.Enabled && !c.Channels.Telegram.Token.IsSet() {
		return errors.New("channels.telegram.token (a `!secret` reference) is required when telegram is enabled")
	}

	if c.Channels.Telegram.Enabled && len(c.Channels.Telegram.AllowFrom) == 0 {
		return errors.New("channels.telegram.allow_from is required when telegram is enabled")
	}

	if c.Channels.Serial.Enabled && c.Channels.Serial.Port == "" {
		return errors.New("channels.serial.port is required when serial is enabled")
	}

	if c.Channels.ZMQCLI.Enabled {
		if c.Channels.CLI.Enabled {
			return errors.New("channels.cli and channels.zmqcli cannot both be enabled (zmqcli replaces the stdin CLI under the same logical channel name)")
		}

		if !c.Channels.ZMQCLI.AuthToken.IsSet() {
			return errors.New("channels.zmqcli.auth_token (a `!secret` reference) is required when channels.zmqcli.enabled is true")
		}

		if c.Channels.ZMQCLI.Bind == "" {
			return errors.New("channels.zmqcli.bind is required when channels.zmqcli.enabled is true")
		}
	}

	return nil
}

// validateLLM validates the providers-plus-references LLM registry.
// Reference parsing, provider existence, non-empty model-id, and uniqueness
// across default+failover are checked before any per-provider validation, and
// per-provider validation (provider type, api, credentials, ollama-only
// fields) runs inside the providers loop.
func (c *Config) validateLLM() error {
	if c.LLM.Default == "" {
		return errors.New("llm.default is required")
	}

	if len(c.LLM.Providers) == 0 {
		return errors.New("llm.providers must contain at least one entry")
	}

	// Provider instance names MUST NOT contain '::' so a reference is
	// unambiguous (provider identifiers).
	for name := range c.LLM.Providers {
		if strings.Contains(name, modelRefSeparator) {
			return fmt.Errorf("llm.providers.%s: provider instance name must not contain '::'", name) // user-facing config error
		}
	}

	// Validate default + failover references: parse, ensure provider exists,
	// reject duplicates across the whole chain.
	refs := make([]string, 0, 1+len(c.LLM.Failover))
	refs = append(refs, c.LLM.Default)
	refs = append(refs, c.LLM.Failover...)

	seen := make(map[string]bool, len(refs))
	for _, ref := range refs {
		providerName, modelID, err := ParseModelReference(ref)
		if err != nil {
			return fmt.Errorf("llm reference %q is invalid: %w", ref, err)
		}

		if _, ok := c.LLM.Providers[providerName]; !ok {
			return fmt.Errorf("llm reference %q names provider %q which is not declared under llm.providers", ref, providerName) // user-facing config error
		}

		if modelID == "" {
			return fmt.Errorf("llm reference %q has an empty model-id", ref) // user-facing config error
		}

		if seen[ref] {
			return fmt.Errorf("duplicate llm reference %q across default/failover", ref) // user-facing config error
		}

		seen[ref] = true
	}

	validProviders := map[string]bool{
		ProviderOllama:      true,
		ProviderOpenRouter:  true,
		ProviderOpenCode:    true,
		ProviderOpenAICodex: true,
	}

	// validProviderAPIs is the per-provider wire-protocol allow-list (design
	// Wire protocol selection: `api` is required on every
	// instance and only the documented values are accepted per provider type.
	// The same `api` value on different provider types selects different
	// eino components (opencode chat_completions → eino-ext/openai,
	// openrouter chat_completions → eino-ext/openrouter), which is why the
	// `provider` field remains distinct.
	validProviderAPIs := map[string]map[string]string{
		ProviderOllama:     {APIOllama: "the native Ollama client"},
		ProviderOpenRouter: {APIChatCompletions: "the OpenRouter component"},
		ProviderOpenCode: {
			APIChatCompletions: "the OpenAI Chat Completions component",
			APIAnthropic:       "the Anthropic Messages component",
			APIGoogle:          "the Google generateContent component",
			APIResponses:       "the OpenAI Responses component",
		},
		ProviderOpenAICodex: {APIResponses: "the ChatGPT Codex Responses component"},
	}

	for name, entry := range c.LLM.Providers {
		if entry.Provider == "" {
			return fmt.Errorf("llm.providers.%s.provider is required", name) // user-facing config error
		}

		if !validProviders[entry.Provider] {
			return fmt.Errorf("llm.providers.%s.provider %q is not supported (valid: ollama, openrouter, opencode, openai-codex)", name, entry.Provider) // user-facing config error
		}

		if entry.API == "" {
			return fmt.Errorf("llm.providers.%s.api is required (declare the wire protocol explicitly; valid for %q: %s)", name, entry.Provider, strings.Join(validAPINames(validProviderAPIs[entry.Provider]), ", ")) // user-facing config error
		}

		if _, ok := validProviderAPIs[entry.Provider][entry.API]; !ok {
			return fmt.Errorf("llm.providers.%s.api %q is not supported for provider %q (valid: %s)", name, entry.API, entry.Provider, strings.Join(validAPINames(validProviderAPIs[entry.Provider]), ", ")) // user-facing config error
		}

		if entry.Provider != ProviderOllama && (entry.KeepAlive != nil || entry.Options != nil) {
			return fmt.Errorf("llm.providers.%s.keep_alive and options are ollama-only fields (provider %q)", name, entry.Provider) // user-facing config error
		}

		// context_window (tui-usage-observability): optional positive integer
		// on every provider type; a non-positive value fails boot naming the
		// field.
		if entry.ContextWindow < 0 {
			return fmt.Errorf("llm.providers.%s.context_window must be a positive integer (tokens), got %d", name, entry.ContextWindow) // user-facing config error
		}

		hasKey := entry.APIKey.IsSet()

		if entry.Provider == ProviderOpenAICodex && hasKey {
			return fmt.Errorf("llm.providers.%s: api_key is not supported for provider %q; run `ready login %s`", name, entry.Provider, name) // user-facing config error
		}

		if !hasKey && entry.Provider != ProviderOllama && entry.Provider != ProviderOpenAICodex {
			return fmt.Errorf("llm.providers.%s.api_key (a `!secret` reference) is required for provider %q", name, entry.Provider) // user-facing config error
		}
	}

	return c.validateLLMRecovery()
}

// validAPINames renders the sorted allow-list of wire protocols for a
// provider type, for use in validation error messages.
func validAPINames(apis map[string]string) []string {
	names := make([]string, 0, len(apis))
	for apiName := range apis {
		names = append(names, apiName)
	}
	slices.Sort(names)

	return names
}

// validateLLMRecovery validates llm.failover_attempt_timeout and
// llm.recovery (change llm-failover-orchestrator). Validation runs on the
// effective (defaults-merged) values so an omitted field can never be
// invalid, while an explicitly wrong value is rejected naming the offending
// field.
func (c *Config) validateLLMRecovery() error {
	attemptTimeout := c.LLM.ResolvedFailoverAttemptTimeout()
	if attemptTimeout < 0 {
		return fmt.Errorf("llm.failover_attempt_timeout must be a non-negative duration, got %s", attemptTimeout) // user-facing config error
	}

	retries := c.LLM.ResolvedFailoverRetries()
	if retries < 0 || retries > maxFailoverRetries {
		return fmt.Errorf("llm.failover_retries must be between 0 and %d, got %d", maxFailoverRetries, retries) // user-facing config error
	}

	retryBackoff := c.LLM.ResolvedFailoverRetryBackoff()
	if retryBackoff <= 0 {
		return fmt.Errorf("llm.failover_retry_backoff must be a positive duration, got %s", retryBackoff) // user-facing config error
	}

	r := c.LLM.ResolvedRecovery()

	if r.Soft.InitialCooldown < 0 {
		return fmt.Errorf("llm.recovery.soft.initial_cooldown must be a positive duration, got %s", r.Soft.InitialCooldown) // user-facing config error
	}
	if r.Soft.MaxCooldown < 0 {
		return fmt.Errorf("llm.recovery.soft.max_cooldown must be a positive duration, got %s", r.Soft.MaxCooldown) // user-facing config error
	}
	if r.Soft.MaxCooldown < r.Soft.InitialCooldown {
		return fmt.Errorf("llm.recovery.soft.max_cooldown (%s) must not be shorter than soft.initial_cooldown (%s)", r.Soft.MaxCooldown, r.Soft.InitialCooldown) // user-facing config error
	}
	if r.Hard.InitialCooldown < 0 {
		return fmt.Errorf("llm.recovery.hard.initial_cooldown must be a positive duration, got %s", r.Hard.InitialCooldown) // user-facing config error
	}
	if r.Hard.MaxCooldown < 0 {
		return fmt.Errorf("llm.recovery.hard.max_cooldown must be a positive duration, got %s", r.Hard.MaxCooldown) // user-facing config error
	}
	if r.Hard.MaxCooldown < r.Hard.InitialCooldown {
		return fmt.Errorf("llm.recovery.hard.max_cooldown (%s) must not be shorter than hard.initial_cooldown (%s)", r.Hard.MaxCooldown, r.Hard.InitialCooldown) // user-facing config error
	}
	if r.BackoffFactor < 0 || r.BackoffFactor <= 1 {
		return fmt.Errorf("llm.recovery.backoff_factor must be greater than 1, got %v", c.LLM.Recovery.BackoffFactor) // user-facing config error
	}
	if r.ProbeTimeout < 0 {
		return fmt.Errorf("llm.recovery.probe_timeout must be a positive duration, got %s", r.ProbeTimeout) // user-facing config error
	}
	if r.IdleProbeWindow < 0 {
		return fmt.Errorf("llm.recovery.idle_probe_window must be a positive duration, got %s", r.IdleProbeWindow) // user-facing config error
	}

	return nil
}

// ResolvedFailoverAttemptTimeout returns the configured streaming liveness
// budget. It is nil-safe for directly constructed configs; explicit zero is
// preserved because zero disables the budget.
func (c LLMConfig) ResolvedFailoverAttemptTimeout() time.Duration {
	if c.FailoverAttemptTimeout == nil {
		return defaultFailoverAttemptTimeout
	}

	return *c.FailoverAttemptTimeout
}

// ResolvedFailoverRetries returns the configured number of same-candidate
// retries. Explicit zero is preserved.
func (c LLMConfig) ResolvedFailoverRetries() int {
	if c.FailoverRetries == nil {
		return defaultFailoverRetries
	}

	return *c.FailoverRetries
}

// ResolvedFailoverRetryBackoff returns the base retry delay.
func (c LLMConfig) ResolvedFailoverRetryBackoff() time.Duration {
	if c.FailoverRetryBackoff == nil {
		return defaultFailoverRetryBackoff
	}

	return *c.FailoverRetryBackoff
}

// ResolvedRecovery returns the effective failover-recovery configuration:
// the operator-provided llm.recovery block with every omitted (zero) field
// replaced by its documented default. Safe on a nil Recovery block and on a
// directly-constructed LLMConfig that never went through Load — the
// orchestrator consumes the result and never sees a zero value.
func (c LLMConfig) ResolvedRecovery() LLMRecoveryConfig {
	r := LLMRecoveryConfig{
		Soft:            LadderConfig{InitialCooldown: defaultSoftInitialCooldown, MaxCooldown: defaultSoftMaxCooldown},
		Hard:            LadderConfig{InitialCooldown: defaultHardInitialCooldown, MaxCooldown: defaultHardMaxCooldown},
		BackoffFactor:   defaultRecoveryBackoffFactor,
		ProbeTimeout:    defaultProbeTimeout,
		IdleProbeWindow: defaultIdleProbeWindow,
	}

	if c.Recovery == nil {
		return r
	}

	if c.Recovery.Soft.InitialCooldown != 0 {
		r.Soft.InitialCooldown = c.Recovery.Soft.InitialCooldown
	}
	if c.Recovery.Soft.MaxCooldown != 0 {
		r.Soft.MaxCooldown = c.Recovery.Soft.MaxCooldown
	}
	if c.Recovery.Hard.InitialCooldown != 0 {
		r.Hard.InitialCooldown = c.Recovery.Hard.InitialCooldown
	}
	if c.Recovery.Hard.MaxCooldown != 0 {
		r.Hard.MaxCooldown = c.Recovery.Hard.MaxCooldown
	}
	if c.Recovery.BackoffFactor != 0 {
		r.BackoffFactor = c.Recovery.BackoffFactor
	}
	if c.Recovery.ProbeTimeout != 0 {
		r.ProbeTimeout = c.Recovery.ProbeTimeout
	}
	if c.Recovery.IdleProbeWindow != 0 {
		r.IdleProbeWindow = c.Recovery.IdleProbeWindow
	}

	return r
}

// validateOAuth checks the root OAuth block and every per-server oauth block.
// Errors here are ordinary validation failures (config.Validate's contract);
// the non-https warning is deliberately not an error — see OAuthBootWarnings.
func (c *Config) validateOAuth() error {
	if c.OAuth.RedirectPort < 0 || c.OAuth.RedirectPort > 65535 {
		return fmt.Errorf("oauth.redirect_port %d is out of range (must be between 1 and 65535)", c.OAuth.RedirectPort) // user-facing config error
	}

	for name, srv := range c.MCPServers {
		if srv.OAuth == nil {
			continue
		}

		if err := validateServerOAuth(name, srv); err != nil {
			return err
		}
	}

	return nil
}

// validateServerOAuth validates a single server's oauth block.
func validateServerOAuth(name string, srv MCPServerConfig) error {
	oa := srv.OAuth

	if !isValidGrant(oa.Grant) {
		return fmt.Errorf("mcp_servers.%s.oauth.grant %q is invalid (must be auto, authorization_code, or client_credentials)", name, oa.Grant) // user-facing config error
	}

	if _, ok := srv.Headers["Authorization"]; ok {
		return fmt.Errorf("mcp_servers.%s: oauth and headers.Authorization are mutually exclusive on the same server", name) // user-facing config error
	}

	if srv.Transport == "sse" {
		return fmt.Errorf("mcp_servers.%s: oauth is not supported over transport: sse (SSEClientTransport has no OAuthHandler)", name) // user-facing config error
	}

	return nil
}

// OAuthBootWarnings returns one warning message per MCP server that declares
// an oauth block on a non-https URL. This is deliberately a boot-time
// warning rather than a Validate error: deciding "is this
// local?" from the URL scheme alone would misjudge split-horizon DNS, VPNs,
// Tailscale and IPv6 ULA topologies that are common on a home LAN.
func (c *Config) OAuthBootWarnings() []string {
	var warnings []string

	names := make([]string, 0, len(c.MCPServers))
	for name := range c.MCPServers {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		srv := c.MCPServers[name]
		if srv.OAuth == nil {
			continue
		}

		if srv.URL.Value != "" && !strings.HasPrefix(strings.ToLower(srv.URL.Value), "https://") {
			warnings = append(warnings, fmt.Sprintf(
				"mcp_servers.%s declares oauth on a non-https URL (%s): tokens and authorization codes will travel in plaintext",
				name, srv.URL.LogValue(),
			))
		}
	}

	return warnings
}

// ValidateLogging checks the logging block. It returns a fatal error for an
// invalid format or invalid file-rotation values (only when file_path is
// set), and non-fatal boot warnings for unrecognised levels (the caller logs
// them; the logging package falls back to info / the file level) and for a
// total cap too small to retain history.
func (c *Config) ValidateLogging() (warnings []string, err error) {
	lc := c.Logging

	if lc.Format != "" && lc.Format != "console" && lc.Format != "json" {
		return nil, fmt.Errorf("logging.format %q is invalid: must be console or json", lc.Format)
	}

	if !validLogLevel(lc.Level) {
		warnings = append(warnings, fmt.Sprintf("logging.level %q is not recognised (valid: trace|debug|info|warn|error); falling back to info", lc.Level))
	}

	if lc.ConsoleLevel != ConsoleLevelOff && !validLogLevel(lc.ConsoleLevel) {
		warnings = append(warnings, fmt.Sprintf("logging.console_level %q is not recognised (valid: trace|debug|info|warn|error|off); the console log uses logging.level", lc.ConsoleLevel))
	}

	if lc.FilePath == "" {
		if lc.ConsoleLevel == ConsoleLevelOff {
			warnings = append(warnings, "logging.console_level is off and logging.file_path is empty: only fatal records will be logged anywhere")
		}

		return warnings, nil
	}

	r := lc.FileRotation
	if r.MaxSegmentSize != nil && *r.MaxSegmentSize <= 0 {
		return nil, fmt.Errorf("logging.file_rotation.max_segment_size must be positive, got %d", int64(*r.MaxSegmentSize))
	}

	if r.MaxTotalSize != nil && *r.MaxTotalSize <= 0 {
		return nil, fmt.Errorf("logging.file_rotation.max_total_size must be positive, got %d", int64(*r.MaxTotalSize))
	}

	if r.RetentionDays != nil && *r.RetentionDays <= 0 {
		return nil, fmt.Errorf("logging.file_rotation.retention_days must be positive, got %d", *r.RetentionDays)
	}

	seg, total := r.EffectiveMaxSegmentSize(), r.EffectiveMaxTotalSize()
	if total < seg {
		return nil, fmt.Errorf("logging.file_rotation.max_total_size (%s) must not be smaller than logging.file_rotation.max_segment_size (%s)", ByteSize(total), ByteSize(seg))
	}

	if total/2 < seg {
		warnings = append(warnings, fmt.Sprintf("logging.file_rotation.max_total_size (%s) is less than twice logging.file_rotation.max_segment_size (%s); almost no log history will be retained", ByteSize(total), ByteSize(seg)))
	}

	return warnings, nil
}

// validLogLevel reports whether name is empty or a recognised level.
func validLogLevel(name string) bool {
	switch name {
	case "", "trace", "debug", "info", "warn", "error":
		return true
	default:
		return false
	}
}

// applyDefaults sets default values for optional fields.
func (c *Config) applyDefaults() {
	// Direct constructions (in-memory Configs in tests and embedders) resolve
	// their runtime paths here against the working directory; configs loaded
	// through Load were already resolved by ApplyRuntimeHome (the
	// runtimeHomesApplied guard makes this a no-op for them).
	c.applyRuntimeHomePaths()

	if c.Channels.Serial.BaudRate == 0 {
		c.Channels.Serial.BaudRate = 9600
	}

	// Channel default (zmq-first): cli and zmqcli are mutually exclusive and
	// share the logical channel name "cli". When the operator does not set
	// channels.zmqcli.enabled explicitly, the daemon enables the ZMQ CLI
	// channel (the `ready chat` interface) and keeps the legacy stdin CLI off
	// as soon as a shared-secret reference is configured (auth_token) —
	// configuring the reference is the opt-in signal, so a
	// minimal config (e.g. for `ready login`, which never touches channels)
	// still boots without a ZMQ secret. An explicit channels.cli.enabled:
	// true suppresses the default; an explicit channels.zmqcli.enabled:
	// false is always honoured.
	if !c.Channels.zmqCLIEnabledSet && !c.Channels.CLI.Enabled &&
		c.Channels.ZMQCLI.AuthToken.IsSet() {
		c.Channels.ZMQCLI.Enabled = true
	}

	if c.Channels.ZMQCLI.Bind == "" {
		c.Channels.ZMQCLI.Bind = "tcp://127.0.0.1:7600"
	}

	if c.Channels.ZMQCLI.ConnectionIdleTimeout == 0 {
		c.Channels.ZMQCLI.ConnectionIdleTimeout = 15 * time.Minute
	}

	if c.Channels.ZMQCLI.ReplyTimeout == 0 {
		c.Channels.ZMQCLI.ReplyTimeout = 400 * time.Second
	}

	if c.Channels.ZMQCLI.LivenessTimeout == 0 {
		c.Channels.ZMQCLI.LivenessTimeout = 45 * time.Second
	}

	applyLLMDefaults(&c.LLM)

	for name, srv := range c.MCPServers {
		applyMCPDefaults(&srv)
		c.MCPServers[name] = srv
	}

	applyHADefaults(&c.HomeAssistant)

	if c.Logging.Level == "" {
		c.Logging.Level = "info"
	}

	if c.Logging.Format == "" {
		c.Logging.Format = "console"
	}

	applyLogRotationDefaults(&c.Logging)

	if c.DefaultProfile == "" {
		c.DefaultProfile = profileNameDefault
	}

	applyOAuthDefaults(&c.OAuth)
	c.LLM.OAuthPath = c.OAuth.Path

	for name, srv := range c.MCPServers {
		if srv.OAuth != nil && srv.OAuth.Grant == "" {
			srv.OAuth.Grant = grantAuto
			c.MCPServers[name] = srv
		}
	}

	c.applyProfileDefaults()
}

// applyOAuthDefaults sets sensible defaults for the root OAuth block.
func applyOAuthDefaults(o *OAuthConfig) {
	if o.RedirectPort == 0 {
		o.RedirectPort = 7601
	}
}

// Default LLM recovery values (change llm-failover-orchestrator, design
// D4/D6/D9). Applied by applyLLMDefaults when the operator omits the field.
const (
	defaultFailoverAttemptTimeout = 45 * time.Second
	defaultFailoverRetries        = 2
	defaultFailoverRetryBackoff   = 2 * time.Second
	maxFailoverRetries            = 5
	defaultSoftInitialCooldown    = 30 * time.Second
	defaultSoftMaxCooldown        = 5 * time.Minute
	defaultHardInitialCooldown    = 5 * time.Minute
	defaultHardMaxCooldown        = 1 * time.Hour
	defaultRecoveryBackoffFactor  = 2
	defaultProbeTimeout           = 10 * time.Second
	defaultIdleProbeWindow        = 15 * time.Minute
)

// applyLLMDefaults sets defaults for the LLM provider registry: it backfills
// each provider instance's Name from its YAML map key and normalises the
// failover-attempt timeout and recovery block to their documented defaults
// when omitted. `api` has NO implicit default — it is required on every
// provider instance and validated per provider type by validateLLM.
func applyLLMDefaults(llm *LLMConfig) {
	if llm.FailoverAttemptTimeout == nil {
		v := defaultFailoverAttemptTimeout
		llm.FailoverAttemptTimeout = &v
	}
	if llm.FailoverRetries == nil {
		v := defaultFailoverRetries
		llm.FailoverRetries = &v
	}
	if llm.FailoverRetryBackoff == nil {
		v := defaultFailoverRetryBackoff
		llm.FailoverRetryBackoff = &v
	}
	// Explicit zero/negative values are preserved so resolution retains the
	// disable semantic and validation can reject invalid values by field name.

	if llm.Recovery == nil {
		llm.Recovery = &LLMRecoveryConfig{}
	}

	r := llm.Recovery
	if r.Soft.InitialCooldown == 0 {
		r.Soft.InitialCooldown = defaultSoftInitialCooldown
	}
	if r.Soft.MaxCooldown == 0 {
		r.Soft.MaxCooldown = defaultSoftMaxCooldown
	}
	if r.Hard.InitialCooldown == 0 {
		r.Hard.InitialCooldown = defaultHardInitialCooldown
	}
	if r.Hard.MaxCooldown == 0 {
		r.Hard.MaxCooldown = defaultHardMaxCooldown
	}
	if r.BackoffFactor == 0 {
		r.BackoffFactor = defaultRecoveryBackoffFactor
	}
	if r.ProbeTimeout == 0 {
		r.ProbeTimeout = defaultProbeTimeout
	}
	if r.IdleProbeWindow == 0 {
		r.IdleProbeWindow = defaultIdleProbeWindow
	}

	if llm.Providers == nil {
		return
	}

	for name, entry := range llm.Providers {
		entry.Name = name
		if entry.Provider == ProviderOpenAICodex && entry.BaseURL == "" {
			entry.BaseURL = "https://chatgpt.com/backend-api"
		}
		llm.Providers[name] = entry
	}
}

// applyProfileDefaults synthesizes the built-in "default" and "edit" profiles
// when an operator has not overridden them in the profiles map. Operators may
// override either built-in by declaring a profile with the same name.
func (c *Config) applyProfileDefaults() {
	if c.Profiles == nil {
		c.Profiles = make(map[string]ProfileConfig)
	}

	if _, ok := c.Profiles[profileNameDefault]; !ok {
		c.Profiles[profileNameDefault] = ProfileConfig{}
	}

	if _, ok := c.Profiles[profileNameEdit]; !ok {
		c.Profiles[profileNameEdit] = ProfileConfig{
			InjectSkills: []string{"home-provisioning"},
		}
	}

	// When builtin_tools.profiles is entirely absent, synthesize default
	// and edit owner rules that DENY config-manager while retaining the
	// other builtins (mcp-profiles delta): editing the elected configuration
	// is an explicit opt-in via a named profile's owner policy, never an
	// implicit default. Explicit owner declarations keep the established
	// semantics (empty allow = all owner tools, deny precedence, missing
	// profile fail-closed).
	if c.BuiltinTools.Profiles == nil {
		c.BuiltinTools.Profiles = map[string]OwnerProfileConfig{
			profileNameDefault: {Deny: []string{configManagerToolKey}},
			profileNameEdit:    {Deny: []string{configManagerToolKey}},
		}
	}
}

// ResolveProfile selects the active MCP profile by precedence
// flagVal > envVal > c.DefaultProfile (the config value, defaulted to "default" by
// applyDefaults). It returns the resolved name, its ProfileConfig, and an error
// if the resolved name is not declared in the profiles map.
func (c *Config) ResolveProfile(flagVal, envVal string) (string, ProfileConfig, error) {
	name := flagVal
	if name == "" {
		name = envVal
	}

	if name == "" {
		name = c.DefaultProfile
	}

	if name == "" {
		name = profileNameDefault
	}

	prof, ok := c.Profiles[name]
	if !ok {
		return name, ProfileConfig{}, fmt.Errorf("unknown mcp profile %q (declare it under profiles or use a built-in: default, edit)", name) // user-facing config error
	}

	return name, prof, nil
}

// applyMCPDefaults sets sensible defaults for an MCP server entry.
func applyMCPDefaults(srv *MCPServerConfig) {
	if srv.Transport == "" {
		srv.Transport = "streamable_http"
	}

	if srv.Timeout == 0 {
		srv.Timeout = 30 * time.Second
	}

	if srv.Reconnect.InitialBackoff == 0 {
		srv.Reconnect.InitialBackoff = time.Second
	}

	if srv.Reconnect.MaxBackoff == 0 {
		srv.Reconnect.MaxBackoff = 60 * time.Second
	}

	if srv.Reconnect.BackoffFactor == 0 {
		srv.Reconnect.BackoffFactor = 2.0
	}

	if srv.HealthCheck.Interval == 0 {
		srv.HealthCheck.Interval = 30 * time.Second
	}
}

// applyHADefaults sets sensible defaults for the HomeAssistant section.
func applyHADefaults(ha *HomeAssistantConfig) {
	if ha.Monitoring.SyncTimeout <= 0 {
		ha.Monitoring.SyncTimeout = 15 * time.Second
	}
	if ha.Monitoring.PingInterval <= 0 {
		ha.Monitoring.PingInterval = 30 * time.Second
	}
	if ha.Monitoring.TopologyDebounce <= 0 {
		ha.Monitoring.TopologyDebounce = 500 * time.Millisecond
	}
	if ha.Monitoring.InitialBackoff <= 0 {
		ha.Monitoring.InitialBackoff = time.Second
	}
	if ha.Monitoring.MaxBackoff <= 0 {
		ha.Monitoring.MaxBackoff = 60 * time.Second
	}
	if ha.Monitoring.BackoffFactor <= 1 {
		ha.Monitoring.BackoffFactor = 2.0
	}
	if ha.StaleThresholdStates <= 0 {
		ha.StaleThresholdStates = 60 * time.Second
	}
	if ha.HardStaleThreshold <= 0 {
		ha.HardStaleThreshold = 300 * time.Second
	}
	if ha.OutstandingPingsThreshold <= 0 {
		ha.OutstandingPingsThreshold = 2
	}
	if ha.Commands.VerifyTimeout <= 0 {
		ha.Commands.VerifyTimeout = 5 * time.Second
	}
	if ha.Commands.VerifyTimeout < time.Second {
		ha.Commands.VerifyTimeout = time.Second
	}
	if ha.Commands.VerifyTimeout > 30*time.Second {
		ha.Commands.VerifyTimeout = 30 * time.Second
	}
	if ha.Commands.MaxTargets <= 0 {
		ha.Commands.MaxTargets = 20
	}
}

// EffectiveToolPrefix returns the prefix to prepend to tool names exposed to
// the LLM for this server. The prefix and tool name are joined with "__".
//
// Resolution order:
//
//  1. nil (field absent)   → use serverKey as prefix
//  2. "" (explicit empty)  → no prefix; wire name used as-is
//  3. non-empty string     → use the provided value
func (m MCPServerConfig) EffectiveToolPrefix(serverKey string) string {
	if m.ToolPrefix == nil {
		return serverKey
	}

	return *m.ToolPrefix
}

// HasEnabledChannels returns true if at least one channel is enabled.
func (c *Config) HasEnabledChannels() bool {
	return c.Channels.CLI.Enabled ||
		c.Channels.ZMQCLI.Enabled ||
		c.Channels.Serial.Enabled ||
		c.Channels.Telegram.Enabled
}

// applyLogRotationDefaults fills absent file_rotation fields (nil pointers)
// when file_path is set. Explicit values — including an invalid explicit zero,
// rejected by ValidateLogging — are never overwritten.
func applyLogRotationDefaults(lc *LoggingConfig) {
	if lc.FilePath == "" {
		return
	}

	r := &lc.FileRotation
	if r.MaxSegmentSize == nil {
		v := DefaultLogMaxSegmentSize
		r.MaxSegmentSize = &v
	}

	if r.MaxTotalSize == nil {
		v := DefaultLogMaxTotalSize
		r.MaxTotalSize = &v
	}

	if r.RetentionDays == nil {
		v := DefaultLogRetentionDays
		r.RetentionDays = &v
	}
}
