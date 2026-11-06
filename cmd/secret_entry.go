package main

import (
	"context"
	"errors"
	"sort"
	"time"

	"github.com/marco-rozz/ready/internal/channel/zmqcli"
	"github.com/marco-rozz/ready/internal/codexauth"
	"github.com/marco-rozz/ready/internal/config"
	"github.com/marco-rozz/ready/internal/oauth"
	"github.com/marco-rozz/ready/internal/profile"
	"github.com/marco-rozz/ready/internal/protocol"
	"github.com/marco-rozz/ready/internal/secretstore"
	"github.com/marco-rozz/ready/internal/tools"
)

// isConfigManagerAllowed reports whether the active Profile's builtin_tools
// policy exposes the config-manager tool (fail-closed when the profile is
// undeclared).
func isConfigManagerAllowed(cfg *config.Config, activeProfileName string) bool {
	if cfg == nil {
		return false
	}
	rules, ok := cfg.BuiltinTools.Profiles[activeProfileName]
	if !ok {
		return false
	}

	return profile.Allowed(config.ConfigManagerToolKey, rules.Allow, rules.Deny)
}

// diskConfig reads and decodes the current configuration ON DISK, falling back
// to the running configuration when the file cannot be read or decoded.
func diskConfig(ctx context.Context, cfg *config.Config) *config.Config {
	if cfg == nil {
		return nil
	}
	if state, err := config.ReadConfigSource(ctx, cfg.ElectedPath); err == nil {
		if disk, derr := config.DecodeForRepair(ctx, state.Data, cfg.ElectedPath); derr == nil {
			disk.SecretValues = cfg.SecretValues
			disk.LLM.Secrets = cfg.SecretValues

			return disk
		}
	}

	return cfg
}

// diskReferences reads the references of the configuration ON DISK, so a
// reference the config manager just saved is pending immediately. When the
// file cannot be decoded it falls back to the running configuration.
func diskReferences(ctx context.Context, cfg *config.Config) ([]secretstore.Reference, config.SecretRef) {
	disk := diskConfig(ctx, cfg)

	return disk.SecretStoreReferences(), disk.Channels.ZMQCLI.AuthToken
}

// deriveOAuthCredentials derives the usability state of every OAuth target
// declared in the on-disk configuration in deterministic order: llm:<provider>
// targets alphabetically first, followed by mcp:<server> targets alphabetically.
func deriveOAuthCredentials(ctx context.Context, cfg *config.Config, store *secretstore.Store) []tools.CredentialSummary {
	disk := diskConfig(ctx, cfg)
	if disk == nil {
		return nil
	}

	var llmNames []string
	for name, prov := range disk.LLM.Providers {
		if prov.Provider == config.ProviderOpenAICodex {
			llmNames = append(llmNames, name)
		}
	}
	sort.Strings(llmNames)

	var mcpNames []string
	for name, srv := range disk.MCPServers {
		if srv.OAuth != nil {
			mcpNames = append(mcpNames, name)
		}
	}
	sort.Strings(mcpNames)

	if len(llmNames) == 0 && len(mcpNames) == 0 {
		return nil
	}

	now := time.Now()
	var codexStore *codexauth.SecretStore
	var mcpStore *oauth.SecretStore
	if store != nil {
		codexStore = codexauth.NewSecretStore(store, cfg.OAuth.Path)
		mcpStore = oauth.NewSecretStore(store, cfg.OAuth.Path)
	}

	out := make([]tools.CredentialSummary, 0, len(llmNames)+len(mcpNames))
	for _, name := range llmNames {
		target := "llm:" + name
		stateStr := "login_required"
		var exp time.Time
		if codexStore != nil {
			if cred, _ := codexStore.Load(ctx, name); cred != nil {
				exp = cred.Expiry
				if cred.Usable(name, now, 30*time.Second) {
					stateStr = "usable"
				}
			}
		}
		out = append(out, tools.CredentialSummary{Target: target, State: stateStr, ExpiresAt: exp})
	}

	for _, name := range mcpNames {
		target := "mcp:" + name
		srv := disk.MCPServers[name]
		grant := srv.OAuth.Grant
		if grant == "" {
			grant = grantAuto
		}
		if isLikelyAutonomous(srv.OAuth, grant) {
			out = append(out, tools.CredentialSummary{Target: target, State: "autonomous"})

			continue
		}
		stateStr := "login_required"
		var exp time.Time
		if mcpStore != nil {
			if cred, _ := mcpStore.Load(ctx, name); cred != nil {
				exp = cred.Expiry
				if cred.Usable(now) {
					stateStr = "usable"
				}
			}
		}
		out = append(out, tools.CredentialSummary{Target: target, State: stateStr, ExpiresAt: exp})
	}

	return out
}

// newSecretEntry builds the direct-entry service over the agent's Secret store.
func newSecretEntry(cfg *config.Config, state *bootSecretState, adopt func(string), onChange func(context.Context)) *tools.SecretEntry {
	return tools.NewSecretEntry(tools.SecretEntryOptions{
		Store: state.Store,
		References: func(ctx context.Context) ([]secretstore.Reference, error) {
			refs, _ := diskReferences(ctx, cfg)

			return refs, nil
		},
		Credentials: func(ctx context.Context) []tools.CredentialSummary {
			return deriveOAuthCredentials(ctx, cfg, state.Store)
		},
		ZMQName: func(ctx context.Context) string {
			_, ref := diskReferences(ctx, cfg)
			if ref.Kind == config.SecretRefName {
				return ref.Name
			}

			return ""
		},
		AdoptZMQ:    adopt,
		OnChange:    onChange,
		IdleTimeout: cfg.Channels.ZMQCLI.ConnectionIdleTimeout,
	})
}

// protocolSecrets renders the derived state for the wire: Secret names with
// their referencing fields, and external-file references as information.
func protocolSecrets(rep secretstore.Report) []protocol.PendingSecret {
	out := make([]protocol.PendingSecret, 0, len(rep.Secrets)+len(rep.External))
	for _, s := range rep.Secrets {
		out = append(out, protocol.PendingSecret{Name: s.Name, Fields: s.Fields, State: protocol.SecretState(s.State), Rule: s.Rule})
	}
	for _, e := range rep.External {
		if e.State == secretstore.StatePendingExternal {
			out = append(out, protocol.PendingSecret{Name: e.Path, Fields: []string{e.Field}, State: protocol.SecretStateExternal})
		}
	}

	return out
}

func protocolCredentials(in []tools.CredentialSummary) []protocol.CredentialInfo {
	out := make([]protocol.CredentialInfo, 0, len(in))
	for _, c := range in {
		item := protocol.CredentialInfo{Target: c.Target, State: c.State}
		if !c.ExpiresAt.IsZero() {
			item.ExpiresAt = c.ExpiresAt.UTC().Format("2006-01-02T15:04:05Z")
		}
		out = append(out, item)
	}

	return out
}

// wireSecretEntry connects direct entry, OAuth login, and the pending snapshot
// to the ZMQ channel.
func wireSecretEntry(ctx context.Context, zmqCh *zmqcli.Channel, cfg *config.Config, state *bootSecretState, activeProfileName string, reloadHandler zmqcli.RuntimeReloadHandler) {
	notifyChanged := func(context.Context) { _ = zmqCh.SendPendingSecretsChanged(ctx) }
	entry := newSecretEntry(cfg, state, zmqCh.ReplaceAuthSecret, notifyChanged)
	oauthSvc := newOAuthLoginService(
		ctx,
		func(cctx context.Context) *config.Config { return diskConfig(cctx, cfg) },
		state.Store,
		func(connID string, resp protocol.Response) { _ = zmqCh.SendOAuthLoginResult(connID, resp) },
		notifyChanged,
	)

	allowed := isConfigManagerAllowed(cfg, activeProfileName)
	zmqCh.SetAgentIdentity(cfg.Agent.ID, cfg.AgentDisplayName())
	zmqCh.SetSecretEntryReleaseHandler(entry.Release)
	zmqCh.SetOAuthLoginReleaseHandler(oauthSvc.Release)
	zmqCh.SetPendingSource(func(pctx context.Context) []protocol.PendingSecret {
		rep, err := entry.Report(pctx)
		if err != nil {
			return nil
		}

		return protocolSecrets(rep)
	})
	zmqCh.SetCredentialsSource(func(cctx context.Context) []protocol.CredentialInfo {
		return protocolCredentials(deriveOAuthCredentials(cctx, cfg, state.Store))
	})
	zmqCh.SetSecretRefresh(func(rctx context.Context) (string, bool) {
		value, ok, err := cfg.ResolveSecret(rctx, cfg.Channels.ZMQCLI.AuthToken, secretstore.FieldZMQAuthToken)

		return value, ok && err == nil
	})
	zmqCh.SetSecretEntryHandler(buildSecretEntryHandler(entry, allowed))
	zmqCh.SetOAuthLoginHandler(buildOAuthLoginHandler(oauthSvc, func() bool { return allowed }))
	zmqCh.SetRuntimeReloadHandler(reloadHandler)
}

func buildSecretEntryHandler(entry *tools.SecretEntry, configManagerAllowed bool) zmqcli.SecretEntryHandler {
	return func(hctx context.Context, _ []byte, loopback bool, req protocol.Request) protocol.Response {
		payload := req.SecretEntry
		writes := payload.Action == protocol.SecretEntryActionBegin || payload.Action == protocol.SecretEntryActionSubmit || payload.Action == protocol.SecretEntryActionDelete
		if writes && !configManagerAllowed {
			return protocol.Response{Status: protocol.ResponseStatusError, ErrorCode: protocol.ErrorCodeProfileRequired}
		}
		if writes && !loopback {
			return protocol.Response{Status: protocol.ResponseStatusError, ErrorCode: protocol.ErrorCodeLoopbackRequired}
		}
		owner := req.ConnectionID
		switch payload.Action {
		case protocol.SecretEntryActionList:
			result, err := entry.List(hctx)

			return secretEntryResponse(result, err, payload.Action)
		case protocol.SecretEntryActionBegin:
			result, err := entry.Begin(hctx, owner, payload.Name)

			return secretEntryResponse(result, err, payload.Action)
		case protocol.SecretEntryActionSubmit:
			result, err := entry.Submit(hctx, owner, payload.OperationID, payload.Name, *payload.Value)

			return secretEntryResponse(result, err, payload.Action)
		case protocol.SecretEntryActionDelete:
			result, err := entry.Delete(hctx, payload.Name)

			return secretEntryResponse(result, err, payload.Action)
		case protocol.SecretEntryActionEnd:
			err := entry.End(owner, payload.OperationID)

			return secretEntryResponse(tools.SecretEntryResult{Outcome: tools.OutcomeReleased}, err, payload.Action)
		default:
			return protocol.Response{Status: protocol.ResponseStatusError, ErrorCode: protocol.ErrorCodeMalformedRequest}
		}
	}
}

// secretEntryResponse maps an outcome or error to the wire. It never carries a
// value: errors reduce to stable codes (and the violated rule code).
func secretEntryResponse(result tools.SecretEntryResult, err error, action protocol.SecretEntryAction) protocol.Response {
	if err != nil {
		code := protocol.ErrorCodeWriteNotCommitted
		switch {
		case errors.Is(err, tools.ErrSecretEntryActive):
			code = protocol.ErrorCodeSecretEntryActive
		case errors.Is(err, tools.ErrSecretEntryExpired):
			code = protocol.ErrorCodeSecretEntryExpired
		case errors.Is(err, tools.ErrNoPendingSecrets):
			code = protocol.ErrorCodeNoPendingSecrets
		case errors.Is(err, tools.ErrUnknownName):
			code = protocol.ErrorCodeUnknownName
		case errors.Is(err, tools.ErrNameInUse):
			code = protocol.ErrorCodeNameInUse
		case errors.Is(err, tools.ErrInvalidValue):
			return protocol.Response{
				Status: protocol.ResponseStatusError, ResponseType: protocol.ResponseTypeReply, ErrorCode: protocol.ErrorCodeInvalidValue,
				SecretEntry: &protocol.SecretEntryResponse{Action: action, Outcome: protocol.SecretEntryOutcomeRefused, RuleCode: result.RuleCode},
			}
		}

		return protocol.Response{Status: protocol.ResponseStatusError, ResponseType: protocol.ResponseTypeReply, ErrorCode: code}
	}

	return protocol.Response{
		Status: protocol.ResponseStatusOK, ResponseType: protocol.ResponseTypeReply,
		SecretEntry: &protocol.SecretEntryResponse{
			Action: action, OperationID: result.OperationID, Outcome: protocol.SecretEntryOutcome(result.Outcome),
			Secrets: protocolSecrets(secretstore.Report{Secrets: result.Secrets, External: result.External}), Credentials: protocolCredentials(result.Credentials),
		},
	}
}
