package main

// Degraded-boot helpers (change secret-store-and-references): the daemon
// DERIVES which Secrets are pending from the configuration's references and the
// Secret store, instead of reading a marker written by the model. The degraded
// view is a shallow config copy with the pending-affected subsystems
// presenting as off — Telegram not started, the Twin treated as disabled,
// owning MCP servers skipped. The LLM registry is NOT degraded: candidates on a
// pending provider become per-candidate construction failures (existing
// orchestrator path), so all-pending resolves to the documented unavailable
// state.

import (
	"context"
	"errors"
	"fmt"

	"github.com/marco-rozz/ready/internal/config"
	"github.com/marco-rozz/ready/internal/secretstore"
	"github.com/rs/zerolog"
)

// secretsSetCommand renders the offline command that sets one Secret for this
// agent (POSIX/PowerShell quoting shared with every remediation).
func secretsSetCommand(agentDir, name string) string {
	cmd := "ready secrets set " + name
	if agentDir != "" && !config.IsDefaultAgentDir(agentDir) {
		cmd += " --agent-dir " + config.QuoteAgentDir(agentDir)
	}

	return cmd
}

// bootSecretState is the boot-time view of the Secret store.
type bootSecretState struct {
	Store   *secretstore.Store
	Report  secretstore.Report
	Pending map[string]bool // dotted field → pending
}

// prepareBootSecrets opens the agent's Secret store, derives the pending set
// and, when the ZMQ shared secret is merely absent, generates it. A ZMQ secret
// that is present but non-conforming, or an external file that is missing,
// refuses boot: the entry channel could not authenticate, and READY never
// overwrites a value it did not just see empty.
func prepareBootSecrets(ctx context.Context, cfg *config.Config, logger zerolog.Logger, agentDir string) (*bootSecretState, error) {
	store, err := cfg.OpenSecretStore(logger, false)
	if err != nil {
		return nil, fmt.Errorf("cannot open the secret store: %w", err)
	}
	cfg.SecretValues = config.NewSecretSource(store)
	cfg.LLM.Secrets = cfg.SecretValues

	state := &bootSecretState{Store: store}
	if err = state.derive(ctx, cfg); err != nil {
		return nil, err
	}

	if cfg.Channels.ZMQCLI.Enabled {
		if err = state.ensureZMQ(ctx, cfg, logger, agentDir); err != nil {
			return nil, err
		}
	}

	return state, nil
}

func (b *bootSecretState) derive(ctx context.Context, cfg *config.Config) error {
	payload, err := b.Store.View(ctx)
	if errors.Is(err, secretstore.ErrNoStore) {
		payload = nil
	} else if err != nil {
		return fmt.Errorf("cannot read the secret store: %w (restore the key with `ready secrets import-key`, or see docs/agent-directories.md)", err)
	}
	b.Report = secretstore.Derive(payload, cfg.SecretStoreReferences(), nil)
	b.Pending = make(map[string]bool)
	for _, f := range b.Report.PendingFields() {
		b.Pending[f] = true
	}

	return nil
}

func (b *bootSecretState) ensureZMQ(ctx context.Context, cfg *config.Config, logger zerolog.Logger, agentDir string) error {
	ref := cfg.Channels.ZMQCLI.AuthToken
	const field = secretstore.FieldZMQAuthToken
	switch ref.Kind {
	case config.SecretRefFile:
		if b.Pending[field] {
			return fmt.Errorf("channels.zmqcli.auth_token points at %s, which is missing or empty; create it, or use a `!secret` reference and run `%s`", ref.Path, secretsSetCommand(agentDir, "zmqcli_auth_token"))
		}
	case config.SecretRefName:
		for _, st := range b.Report.Secrets {
			if st.Name != ref.Name || st.State != secretstore.StatePending {
				continue
			}
			if st.Rule != "" {
				return fmt.Errorf("channels.zmqcli.auth_token: the secret %q is present but breaks the value rule %q; set a valid one with `%s`", ref.Name, st.Rule, secretsSetCommand(agentDir, ref.Name))
			}

			return b.generateZMQ(ctx, cfg, logger, agentDir, ref.Name)
		}
	}

	return nil
}

// generateZMQ writes a fresh 32-byte (64 hex) ZMQ shared secret under the
// store lock, only while the name is still absent.
func (b *bootSecretState) generateZMQ(ctx context.Context, cfg *config.Config, logger zerolog.Logger, agentDir, name string) error {
	value, err := randomHex(32)
	if err != nil {
		return err
	}
	written := false
	err = b.Store.Update(ctx, func(p *secretstore.Payload) error {
		if _, exists := p.Secrets[name]; exists {
			return nil // a concurrent writer got there first: never overwrite
		}
		p.Secrets[name] = secretstore.Secret{Value: value, UpdatedAt: nowUTC()}
		written = true

		return nil
	})
	if err != nil {
		return fmt.Errorf("failed to generate the ZMQ shared secret: %w", err)
	}
	if written {
		logger.Info().Str("secret", name).
			Msgf("generated the chat (ZMQ) shared secret; `ready chat` on this machine reads it by itself, or copy it with `%s`", revealCommand(agentDir, name))
	}

	return b.derive(ctx, cfg)
}

// revealCommand renders the command that writes one Secret to a file.
func revealCommand(agentDir, name string) string {
	cmd := "ready secrets reveal " + name + " --to-file <path>"
	if agentDir != "" && !config.IsDefaultAgentDir(agentDir) {
		cmd += " --agent-dir " + config.QuoteAgentDir(agentDir)
	}

	return cmd
}

// degradedBootView builds the shallow boot-time view with pending-affected
// subsystems off. The original configuration is never mutated; the copy
// enables the existing disabled-subsystem wiring everywhere downstream.
func degradedBootView(cfg *config.Config, pendingPaths map[string]bool) *config.Config {
	if len(pendingPaths) == 0 {
		return cfg
	}

	view := *cfg // shallow copy: every map mutation below writes its own map

	// Telegram token pending → the channel does not start this boot.
	if pendingPaths["channels.telegram.token"] {
		view.Channels.Telegram.Enabled = false
	}

	// HA websocket token or URL pending → the twin presents as disabled (no
	// twin tools, no home-integration injection, no snapshotter).
	if pendingPaths["home_assistant.websocket.token"] || pendingPaths["home_assistant.websocket.url"] {
		view.HomeAssistant.Enabled = false
	}

	// An MCP server owning any pending credential (its oauth client secret
	// or any header value) is skipped this boot, exactly like a best-effort
	// boot failure.
	if len(cfg.MCPServers) > 0 {
		servers := make(map[string]config.MCPServerConfig, len(cfg.MCPServers))
		for key, srv := range cfg.MCPServers {
			if mcpServerPending(key, srv, pendingPaths) {
				continue
			}
			servers[key] = srv
		}
		view.MCPServers = servers
	}

	return &view
}

// mcpServerPending reports whether one server config owns a pending reference
// (a referenced URL, oauth client secret or any configured header).
func mcpServerPending(key string, srv config.MCPServerConfig, pendingPaths map[string]bool) bool {
	if srv.URL.IsReference() && pendingPaths["mcp_servers."+key+".url"] {
		return true
	}
	if srv.OAuth != nil && pendingPaths["mcp_servers."+key+".oauth.client_secret"] {
		return true
	}
	for h := range srv.Headers {
		if pendingPaths["mcp_servers."+key+".headers."+h] {
			return true
		}
	}

	return false
}

// logPendingBootEffects emits the per-boot degraded-boot WARNs after the
// logger exists: one WARN per pending field naming what stays off this boot,
// plus the manual guidance when ZMQ direct entry is unavailable. Metadata
// only — never a value.
func logPendingBootEffects(logger zerolog.Logger, report secretstore.Report, zmqcliEnabled bool, agentDir string) {
	for _, st := range report.Secrets {
		if st.State != secretstore.StatePending {
			continue
		}
		for _, field := range st.Fields {
			ev := logger.Warn().Str("field", field).Str("secret", st.Name)
			if st.Rule != "" {
				ev = ev.Str("rule", st.Rule)
			}
			if zmqcliEnabled {
				ev.Msg("pending credential: the component behind this field starts off this boot; enter the value in the admin TUI secret editor or with `ready secrets set`, then restart")
			} else {
				ev.Msgf("pending credential: the component behind this field starts off this boot; run `%s`, then restart", secretsSetCommand(agentDir, st.Name))
			}
		}
	}
	for _, ext := range report.External {
		if ext.State == secretstore.StatePendingExternal {
			logger.Warn().Str("field", ext.Field).Str("file", ext.Path).
				Msg("pending external credential: the referenced file is missing or empty; the component behind this field starts off this boot")
		}
	}
}

// logBootVisibility writes the deterministic boot-visibility records
// (selection announcement, permissions warning, resolved profile) — extracted
// to keep Serve's branch fan-out flat.
func logBootVisibility(
	logger zerolog.Logger,
	resolution config.Resolution,
	cfg *config.Config,
	activeProfileName string,
	activeProfile config.ProfileConfig,
) {
	logger.Info().
		Str("agent_dir", resolution.AgentDir).
		Str("config", cfg.ElectedPath).
		Str("origin", string(cfg.Origin)).
		Str("runtime_home", cfg.RuntimeHome).
		Str("agent_id", cfg.Agent.ID).
		Str("agent_name", cfg.AgentDisplayName()).
		Msg("selected agent directory")
	if w := warnConfigPermissions(cfg.ElectedPath); w != "" {
		logger.Warn().Msg(w)
	}

	logger.Info().
		Str("default_profile", activeProfileName).
		Strs("inject_skills", activeProfile.InjectSkills).
		Msg("active MCP profile resolved")
}

// pendingForRef reports whether the candidate's provider instance carries a
// pending api_key (safe diagnostics below name the field, never the value).
func pendingForRef(_ *config.Config, ref string, pendingPaths map[string]bool) bool {
	instance, _, err := config.ParseModelReference(ref)
	if err != nil {
		return false
	}

	return pendingPaths["llm.providers."+instance+".api_key"]
}

// externalSecretFiles lists the `!secret_file` targets of a configuration.
func externalSecretFiles(cfg *config.Config) []string {
	var out []string
	for _, r := range cfg.SecretReferences() {
		if r.Ref.Kind == config.SecretRefFile {
			out = append(out, r.Ref.Path)
		}
	}

	return out
}

// secretPermissionWarnings renders the boot WARNs for a store, key or secret
// file other users could read.
func secretPermissionWarnings(cfg *config.Config) []string {
	return secretstore.PermissionWarnings(cfg.RuntimeHome, externalSecretFiles(cfg)...)
}
