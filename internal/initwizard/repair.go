// Bounded offline repair (design D5): when init runs over an existing
// agent, it may replace the default provider, fix the default provider's
// key, rotate the ZMQ shared secret, or add a missing agent identity — and
// nothing else. The physical agent must be inactive and the document a
// supported current-schema config. Values go into the agent's Secret store;
// config.yaml changes only to add a reference or the identity, committed
// through the shared source-check/backup/atomic-replace machinery.
package initwizard

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"time"

	"github.com/rs/zerolog"

	"github.com/marco-rozz/ready/internal/config"
	"github.com/marco-rozz/ready/internal/secretstore"
)

// repairKind is one answer of the fixed bounded-repair menu.
type repairKind string

const (
	// repairReplaceDefaultProvider replaces the default provider (keeping
	// the instance key), updating the default ref and the explicitly
	// confirmed failover dependencies.
	repairReplaceDefaultProvider repairKind = "1"
	// repairFixDefaultKey fixes only the default provider's key.
	repairFixDefaultKey repairKind = "2"
	// repairRotateZMQ rotates the ZMQ shared secret to a new random value in
	// the Secret store (also the remediation for a missing or non-conforming
	// ZMQ secret).
	repairRotateZMQ repairKind = "3"
	// repairAddIdentity adds the missing agent.id (and name) to the config.
	repairAddIdentity repairKind = "4"
	// repairDecline leaves the existing state unchanged (clean exit 0).
	repairDecline repairKind = ""
)

// Classified path segments this package edits (local spelling of the
// config package's own vocabulary).
const (
	segLLM       = "llm"
	segDefault   = "default"
	segProviders = "providers"
	segAPIKey    = "api_key"
	segFailover  = "failover"
	segChannels  = "channels"
	segZmqcli    = "zmqcli"
	segAuthToken = "auth_token"
	segKeepAlive = "keep_alive"
	segOptions   = "options"
)

// errUnsupportedDocument is the safe refusal for documents outside the
// repair scope (old/future schemas, beyond-marker invalidity, unsupported
// shapes). It never carries source content.
var errUnsupportedDocument = errors.New("unsupported configuration document for offline repair")

// errDeclinedExplicitly marks an operator refusal mid-repair (distinct from
// a declined menu: the repair was chosen but then cancelled).
var errDeclinedExplicitly = errors.New("repair cancelled by the operator")

// repairOccurrences summarises what a repair needs to know about the
// elected config before any edit. Metadata only — never a value.
type repairOccurrences struct {
	// DefaultInstance / DefaultModel: llm.default's parsed halves.
	DefaultInstance string
	DefaultModel    string
	// KeyRef / ZMQRef are the provider key and ZMQ secret references.
	KeyRef config.SecretRef
	ZMQRef config.SecretRef
	// HasKeepAlive / HasOptions: provider-incompatible fields to drop when
	// the provider type changes away from (or to) ollama.
	HasKeepAlive bool
	HasOptions   bool
	// FailoverRefs lists llm.failover entries that name the default
	// provider instance (indexes into the failover list).
	FailoverRefs []int
	// ProviderApi / ProviderBaseURL: the current provider connection.
	ProviderAPI      string
	ProviderBaseURL  string
	ProviderProvider string
	// AgentID / AgentName / MissingIdentity describe the identity block.
	AgentID         string
	AgentName       string
	MissingIdentity bool
	// KeySpec locates the Store key for this agent.
	KeySpec secretstore.KeySpec
}

// inspectForRepair decodes the source bytes for repair: unsupported or
// old/future documents are refused before the menu (init is not a migration
// writer). A missing agent.id is the one validation failure the repair is
// there to fix; every other validation error is refused.
func inspectForRepair(ctx context.Context, res config.Resolution, source config.SourceState) (repairOccurrences, error) {
	var occ repairOccurrences

	cfg, err := config.DecodeForRepair(ctx, source.Data, res.Path)
	if err != nil {
		return occ, fmt.Errorf("%w — edit the document by hand, or let the daemon run its migration: %s",
			errors.Join(errUnsupportedDocument, err), config.ServeMigrationCommand(res.AgentDir))
	}

	if valErr := cfg.ValidateForRepair(); valErr != nil {
		if !errors.Is(valErr, config.ErrAgentIDMissing) {
			return occ, fmt.Errorf("%w — edit the document by hand (%w)", errUnsupportedDocument, valErr)
		}
		occ.MissingIdentity = true
	}
	occ.AgentID, occ.AgentName = cfg.Agent.ID, cfg.Agent.Name
	occ.KeySpec = cfg.StoreKeySpec()
	if occ.MissingIdentity {
		return occ, nil
	}

	instance, model, refErr := config.ParseModelReference(cfg.LLM.Default)
	if refErr != nil {
		return occ, fmt.Errorf("llm.default %q: %w", cfg.LLM.Default, errUnsupportedDocument)
	}
	occ.DefaultInstance = instance
	occ.DefaultModel = model

	provider, ok := cfg.LLM.Providers[instance]
	if !ok {
		return occ, fmt.Errorf("llm.default names unknown provider %q: %w", instance, errUnsupportedDocument)
	}
	occ.ProviderProvider = provider.Provider
	occ.ProviderAPI = provider.API
	occ.ProviderBaseURL = provider.BaseURL
	occ.HasKeepAlive = provider.KeepAlive != nil
	occ.HasOptions = provider.Options != nil
	occ.KeyRef = provider.APIKey
	occ.ZMQRef = cfg.Channels.ZMQCLI.AuthToken

	for i, ref := range cfg.LLM.Failover {
		if refInstance, _, err := config.ParseModelReference(ref); err == nil && refInstance == instance {
			occ.FailoverRefs = append(occ.FailoverRefs, i)
		}
	}

	return occ, nil
}

// repairUI couples one repair's interviewer with its writer.
type repairUI struct {
	interview
	out io.Writer
}

// repairRunsByOperator summarises what one repair turn actually did,
// metadata-only.
type repairRunsByOperator struct {
	// BackupPath / Committed / Warnings come from the shared commit.
	BackupPath string
	Committed  bool
	Warnings   []string
	Done       []string
	Candidate  *config.Config
}

// runRepair walks the bounded repair menu over an existing config. Every
// mutation commits through ValidateCandidate + CommitCandidate against a
// re-read source snapshot; declined repairs leave existing state untouched
// and return ErrDeclinedRepair (a reported reason for exit 0 callers).
func runRepair(
	ctx context.Context,
	res config.Resolution,
	ui repairUI,
	choiceSource func(io.Writer) (ProviderChoice, error),
	randHex func(int) (string, error),
) (repairRunsByOperator, error) {
	var result repairRunsByOperator

	source, err := config.ReadConfigSource(ctx, res.Path)
	if err != nil {
		return result, fmt.Errorf("read config source: %w", err)
	}

	occ, err := inspectForRepair(ctx, res, source)
	if err != nil {
		return result, err
	}

	_, _ = fmt.Fprintf(ui.out, "\nAn existing configuration was found in %s.\nThis is the repair menu, not a new setup. Stop this agent's server before making changes.\n"+repairMenuText(occ), res.AgentDir)
	answer, err := ui.read.line(ctx)
	if err != nil {
		return result, err
	}

	if occ.MissingIdentity {
		if parseMenuChoice(answer) == repairAddIdentity {
			return addIdentity(ctx, res, source, occ)
		}

		return result, ErrDeclinedRepair
	}

	switch parseMenuChoice(answer) {
	case repairDecline:
		return result, ErrDeclinedRepair
	case repairAddIdentity:
		return result, errors.New("this agent already has an identity; nothing to add")
	case repairReplaceDefaultProvider:
		return replaceDefaultProvider(ctx, res, source, occ, ui, choiceSource)
	case repairFixDefaultKey:
		return withCandidate(ctx, res, source, func() (repairRunsByOperator, error) { return fixDefaultKey(ctx, res, source, occ, ui) })
	case repairRotateZMQ:
		return withCandidate(ctx, res, source, func() (repairRunsByOperator, error) { return rotateZMQ(ctx, res, source, occ, randHex) })
	default:
		return result, fmt.Errorf("unsupported repair selection %s (choose 1, 2 or 3, or press Enter to decline)", parseMenuChoice(answer))
	}
}

func repairMenuText(occ repairOccurrences) string {
	if occ.MissingIdentity {
		return `This configuration has no agent identity (agent.id), so READY will not start it.
  4  Add an agent identity (a unique id and the host name as display name)
  <Enter>  leave the existing configuration unchanged and exit
Selection: `
	}

	return `Choose what to change (all other settings are kept):
  1  Change the AI provider or model — answer the setup questions again
  2  Replace the default provider's API key — hidden terminal input
  3  Generate a new chat access secret — it is stored encrypted; chat on this machine picks it up by itself
  <Enter>  leave the existing configuration unchanged and exit
Selection: `
}

// parseMenuChoice trims the operator's menu answer.
func parseMenuChoice(raw string) repairKind { return repairKind(strings.TrimSpace(raw)) }

// commitRepaired validates the span-edited candidate and commits it with
// the shared machinery, then reports metadata honestly (pre-commit refusal
// = nothing changed; committed = even warnings stay committed).
func commitRepaired(ctx context.Context, res config.Resolution, source config.SourceState, affect []byte) (repairRunsByOperator, error) {
	var result repairRunsByOperator

	candidate, verr := config.ValidateCandidate(ctx, affect, res.Path, config.ProfileOverrideInputs{}, nil)
	if verr != nil {
		return result, fmt.Errorf("repaired candidate rejected: %w (existing state preserved)", verr)
	}

	report, cerr := config.CommitCandidate(ctx, source, affect)
	result.BackupPath = report.BackupPath
	result.Committed = report.Committed
	result.Warnings = report.Warnings
	result.Candidate = candidate

	if cerr != nil && !report.Committed {
		return result, cerr
	}

	return result, cerr
}

// repairStore opens the agent's Secret store for a repair write.
func repairStore(res config.Resolution, occ repairOccurrences) (*secretstore.Store, error) {
	store, err := secretstore.New(secretstore.Options{Dir: res.AgentDir, Key: occ.KeySpec, AgentID: occ.AgentID, Log: zerolog.Nop()})
	if err != nil {
		return nil, fmt.Errorf("failed to open the secret store: %w", err)
	}

	return store, nil
}

// secretUndo restores one Secret to its state before a repair wrote it.
type secretUndo func(context.Context) error

// writeRepairSecret stores value under name after the value rules, returning
// an undo for a refused later config commit.
func writeRepairSecret(ctx context.Context, store *secretstore.Store, name, field, value string) (secretUndo, error) {
	if err := secretstore.CheckValue([]string{field}, value); err != nil {
		return nil, fmt.Errorf("the value was refused: %w", err)
	}
	var prior secretstore.Secret
	var had bool
	if err := store.Update(ctx, func(p *secretstore.Payload) error {
		prior, had = p.Secrets[name]
		p.Secrets[name] = secretstore.Secret{Value: value, UpdatedAt: time.Now().UTC()}

		return nil
	}); err != nil {
		return nil, fmt.Errorf("failed to write the secret: %w", err)
	}

	return func(uctx context.Context) error {
		return store.Update(uctx, func(p *secretstore.Payload) error {
			if had {
				p.Secrets[name] = prior
			} else {
				delete(p.Secrets, name)
			}

			return nil
		})
	}, nil
}

// withCandidate runs one repair and, when it changed the store only, attaches
// the decoded current configuration so the follow-up guidance (endpoint, chat
// command) is printed exactly as after a config commit.
func withCandidate(ctx context.Context, res config.Resolution, source config.SourceState, run func() (repairRunsByOperator, error)) (repairRunsByOperator, error) {
	result, err := run()
	if result.Committed && result.Candidate == nil {
		if cfg, derr := config.DecodeForRepair(ctx, source.Data, res.Path); derr == nil {
			result.Candidate = cfg
		}
	}

	return result, err
}

// storeOnlyResult is the outcome of a repair that changed no config bytes.
func storeOnlyResult(done string) repairRunsByOperator {
	return repairRunsByOperator{Committed: true, Done: []string{done}}
}

// replaceDefaultProvider re-asks the provider questions and rewrites the
// default provider's spans: connection fields, the default model ref, and —
// explicitly — the failover entries naming the same instance. The new key goes
// into the Secret store under the instance's canonical name; incompatible
// provider fields (ollama-only keep_alive/options; a key reference for a
// no-key provider) are removed only within the repaired provider.
func replaceDefaultProvider(
	ctx context.Context,
	res config.Resolution,
	source config.SourceState,
	occ repairOccurrences,
	ui repairUI,
	choiceSource func(io.Writer) (ProviderChoice, error),
) (repairRunsByOperator, error) {
	var result repairRunsByOperator

	_, _ = fmt.Fprintf(ui.out, "\nChanging the default AI provider. Its configuration name (%s) is kept so existing references still work.\n", occ.DefaultInstance)
	choice, err := choiceSource(ui.out)
	if err != nil {
		return result, err
	}
	choice.Instance = occ.DefaultInstance

	affect := source.Data
	affect, err = replaceScalarSpan(affect, []string{segLLM, segDefault}, choice.Instance+"::"+choice.ModelID)
	if err != nil {
		return result, fmt.Errorf("llm.default: %w", err)
	}

	provPath := []string{segLLM, segProviders, occ.DefaultInstance}
	affect, err = setScalar(affect, append(provPath, "provider"), choice.Provider, "provider")
	if err != nil {
		return result, fmt.Errorf("provider type: %w", err)
	}
	affect, err = setScalar(affect, append(provPath, "api"), choice.API, "api")
	if err != nil {
		return result, fmt.Errorf("wire api: %w", err)
	}

	var undo secretUndo
	if choice.needsAPIKey() {
		store, serr := repairStore(res, occ)
		if serr != nil {
			return result, serr
		}
		name := secretNameFor(occ.DefaultInstance)
		undo, err = writeRepairSecret(ctx, store, name, "llm.providers."+occ.DefaultInstance+".api_key", choice.APIKey)
		if err != nil {
			return result, err
		}
		affect, err = setReference(affect, append(provPath, segAPIKey), config.TagSecret+" "+name, "api")
		if err != nil {
			_ = undo(ctx)

			return result, fmt.Errorf("api_key: %w", err)
		}
	} else if occ.KeyRef.IsSet() {
		// No-key provider (local ollama/codex): a leftover key reference
		// would fail validation; it is a repair-scope field removable here.
		affect, err = removeMappingLeaf(affect, append(provPath, segAPIKey))
		if err != nil {
			return result, fmt.Errorf("removing api_key: %w — edit the document manually", err)
		}
	}
	rollback := func() {
		if undo != nil && !result.Committed {
			_ = undo(ctx)
		}
	}

	if choice.Provider != config.ProviderOpenAICodex {
		affect, err = setScalar(affect, append(provPath, "base_url"), choice.BaseURL, "provider")
		if err != nil {
			rollback()

			return result, fmt.Errorf("base_url: %w", err)
		}
	} else if _, nodeErr := locateValueNode(source.Data, append(provPath, "base_url")); nodeErr == nil {
		affect, err = removeMappingLeaf(affect, append(provPath, "base_url"))
		if err != nil {
			rollback()

			return result, fmt.Errorf("remove configurable endpoint: %w", err)
		}
	}

	// Provider-incompatible fields, only where repaired: an ollama-only
	// block carried into a non-ollama provider is removed when scalar
	// (keep_alive) or refused to the operator when structured (options).
	if choice.Provider != config.ProviderOllama && occ.HasKeepAlive {
		affect, err = removeMappingLeaf(affect, append(provPath, segKeepAlive))
		if err != nil {
			rollback()

			return result, fmt.Errorf("removing keep_alive: %w — edit the document manually", err)
		}
	}
	if choice.Provider != config.ProviderOllama && occ.HasOptions {
		rollback()

		return result, fmt.Errorf("%w: this provider block carries an ollama options mapping — remove it by hand first (a span repair never rewrites block mappings)", errUnsupportedDocument)
	}

	affect, err = repairFailoverReferences(ctx, source.Data, affect, occ, ui)
	if err != nil {
		rollback()

		return result, err
	}

	repaired, cerr := commitRepaired(ctx, res, source, affect)
	result.BackupPath = repaired.BackupPath
	result.Committed = repaired.Committed
	result.Warnings = repaired.Warnings
	result.Candidate = repaired.Candidate
	if !repaired.Committed {
		rollback()
	}
	result.Done = append(result.Done, "default provider replaced (instance "+occ.DefaultInstance+" → "+choice.Provider+")")

	return result, cerr
}

// setScalar replaces an existing scalar or inserts it after the mapping's
// first key when missing.
func setScalar(data []byte, path []string, value, siblingFallback string) ([]byte, error) {
	if _, err := locateValueNode(data, path); err == nil {
		return replaceScalarSpan(data, path, value)
	}

	parent := path[:len(path)-1]
	after := siblingFallback

	return insertKeyAfter(data, parent, after, path[len(path)-1], value)
}

func plural(n int) string {
	if n == 1 {
		return "y"
	}

	return "ies"
}

// fixDefaultKey replaces only the default provider's key: the value goes into
// the Secret store under the name the provider already references. A
// reference to an external `!secret_file` is never rewritten (READY does not
// own that file); a provider without a reference gets the canonical one.
func fixDefaultKey(
	ctx context.Context,
	res config.Resolution,
	source config.SourceState,
	occ repairOccurrences,
	ui repairUI,
) (repairRunsByOperator, error) {
	var result repairRunsByOperator

	if occ.KeyRef.Kind == config.SecretRefFile {
		return result, fmt.Errorf("%w: api_key points at the external file %s, which READY never rewrites — edit that file, or replace the reference with `!secret <name>`", errUnsupportedDocument, occ.KeyRef.Path)
	}

	_, _ = fmt.Fprintln(ui.out, "Paste the replacement API key, then press Enter. Nothing appears while you type or paste.")
	_, _ = fmt.Fprintf(ui.out, "New API key for provider %s (input hidden): ", occ.DefaultInstance)
	key, err := ui.read.secret(ctx)
	if err != nil {
		return result, err
	}
	key = strings.TrimRight(key, "\r\n")
	if strings.TrimSpace(key) == "" {
		return result, fmt.Errorf("an emptied answer cannot repair a credential; nothing changed")
	}

	store, err := repairStore(res, occ)
	if err != nil {
		return result, err
	}
	name := occ.KeyRef.Name
	field := "llm.providers." + occ.DefaultInstance + ".api_key"
	if name != "" {
		if _, err = writeRepairSecret(ctx, store, name, field, key); err != nil {
			return result, err
		}

		return storeOnlyResult("provider key replaced for " + occ.DefaultInstance + " (stored encrypted; the running server picks it up on restart)"), nil
	}

	// No reference yet: write the canonical reference, then the value.
	name = secretNameFor(occ.DefaultInstance)
	undo, err := writeRepairSecret(ctx, store, name, field, key)
	if err != nil {
		return result, err
	}
	affect, err := setReference(source.Data, []string{segLLM, segProviders, occ.DefaultInstance, segAPIKey}, config.TagSecret+" "+name, "api")
	if err != nil {
		_ = undo(ctx)

		return result, fmt.Errorf("api_key: %w", err)
	}
	repaired, cerr := commitRepaired(ctx, res, source, affect)
	result.BackupPath, result.Committed, result.Warnings, result.Candidate = repaired.BackupPath, repaired.Committed, repaired.Warnings, repaired.Candidate
	if !repaired.Committed {
		_ = undo(ctx)
	}
	result.Done = append(result.Done, "provider key repaired for "+occ.DefaultInstance)

	return result, cerr
}

// rotateZMQ stores a fresh ZMQ shared secret under the name the config
// references; the running channel adopts it at the next handshake. An absent
// reference is added, an external `!secret_file` is never rewritten.
func rotateZMQ(
	ctx context.Context,
	res config.Resolution,
	source config.SourceState,
	occ repairOccurrences,
	randHex func(int) (string, error),
) (repairRunsByOperator, error) {
	var result repairRunsByOperator

	if occ.ZMQRef.Kind == config.SecretRefFile {
		return result, fmt.Errorf("%w: auth_token points at the external file %s, which READY never rewrites — edit that file, or replace the reference with `!secret <name>`", errUnsupportedDocument, occ.ZMQRef.Path)
	}
	zmqHex, rerr := randHex(generateZMQTokenByteLength)
	if rerr != nil {
		return result, rerr
	}
	store, err := repairStore(res, occ)
	if err != nil {
		return result, err
	}
	name := occ.ZMQRef.Name
	if name != "" {
		if _, err = writeRepairSecret(ctx, store, name, secretstore.FieldZMQAuthToken, zmqHex); err != nil {
			return result, err
		}

		return storeOnlyResult("ZMQ shared secret rotated (stored encrypted)"), nil
	}

	name = zmqSecretName()
	undo, err := writeRepairSecret(ctx, store, name, secretstore.FieldZMQAuthToken, zmqHex)
	if err != nil {
		return result, err
	}
	affect, err := setReference(source.Data, []string{segChannels, segZmqcli, segAuthToken}, config.TagSecret+" "+name, "bind")
	if err != nil {
		_ = undo(ctx)

		return result, err
	}
	repaired, cerr := commitRepaired(ctx, res, source, affect)
	result.BackupPath, result.Committed, result.Warnings, result.Candidate = repaired.BackupPath, repaired.Committed, repaired.Warnings, repaired.Candidate
	if !repaired.Committed {
		_ = undo(ctx)

		return result, cerr
	}
	result.Done = append(result.Done, "ZMQ shared secret created (stored encrypted)")

	return result, cerr
}

// addIdentity adds the missing agent block. An orphan store keeps its agent
// id (the config must match the store it belongs to); otherwise a new UUIDv7.
func addIdentity(ctx context.Context, res config.Resolution, source config.SourceState, occ repairOccurrences) (repairRunsByOperator, error) {
	var result repairRunsByOperator

	id, err := freshAgentID(ctx, res.AgentDir, fileExists(filepath.Join(res.AgentDir, secretstore.StoreFileName)))
	if err != nil {
		return result, err
	}
	block := "agent:\n  id: " + id + "\n  name: " + yamlQuote(hostName()) + "\n"
	affect, err := insertAgentBlock(source.Data, block, occ)
	if err != nil {
		return result, err
	}
	repaired, cerr := commitRepaired(ctx, res, source, affect)
	result.BackupPath, result.Committed, result.Warnings, result.Candidate = repaired.BackupPath, repaired.Committed, repaired.Warnings, repaired.Candidate
	result.Done = append(result.Done, "agent identity added ("+id+")")

	return result, cerr
}

// insertAgentBlock puts the agent block right after the config_version line.
func insertAgentBlock(data []byte, block string, _ repairOccurrences) ([]byte, error) {
	if _, err := locateMappingNode(data, []string{"agent"}); err == nil {
		return nil, fmt.Errorf("%w: an agent block without an id already exists — add `id: <uuid>` under it by hand", errUnsupportedDocument)
	}
	text := string(data)
	idx := strings.Index(text, "config_version:")
	if idx < 0 {
		return nil, fmt.Errorf("%w: config_version is missing", errUnsupportedDocument)
	}
	nl := strings.Index(text[idx:], "\n")
	if nl < 0 {
		return nil, fmt.Errorf("%w: unexpected document shape", errUnsupportedDocument)
	}
	at := idx + nl + 1

	return []byte(text[:at] + block + text[at:]), nil
}

// repairFailoverReferences explicitly reviews every model on the replaced
// instance instead of collapsing the chain onto the new default model.
func repairFailoverReferences(ctx context.Context, source, affect []byte, occ repairOccurrences, ui repairUI) ([]byte, error) {
	var err error
	if len(occ.FailoverRefs) > 0 {
		_, _ = fmt.Fprintf(ui.out, "llm.failover has %d entr%s naming %s. Review each dependent model before replacing %s::%s? (y/N): ",
			len(occ.FailoverRefs), plural(len(occ.FailoverRefs)), occ.DefaultInstance, occ.DefaultInstance, occ.DefaultModel)
		ok, cerr := ui.read.confirm(ctx, "")
		if cerr != nil {
			return nil, cerr
		}
		if !ok {
			return nil, fmt.Errorf("%w: failover still references the replaced provider — update llm.failover by hand, or rerun repair and confirm", errDeclinedExplicitly)
		}
		for _, idx := range occ.FailoverRefs {
			node, nerr := locateValueNode(source, []string{segLLM, segFailover, fmt.Sprint(idx)})
			if nerr != nil {
				return nil, nerr
			}
			_, previousModel, nerr := config.ParseModelReference(node.Value)
			if nerr != nil {
				return nil, nerr
			}
			_, _ = fmt.Fprintf(ui.out, "Model for llm.failover[%d] on instance %s [%s]: ", idx, occ.DefaultInstance, previousModel)
			answer, nerr := ui.read.line(ctx)
			if nerr != nil {
				return nil, nerr
			}
			reference := occ.DefaultInstance + "::" + firstNonEmpty(answer, previousModel)
			if _, _, nerr := config.ParseModelReference(reference); nerr != nil {
				return nil, fmt.Errorf("failover model: %w", nerr)
			}
			affect, err = replaceScalarSpan(affect, []string{segLLM, segFailover, fmt.Sprint(idx)}, reference)
			if err != nil {
				return nil, fmt.Errorf("llm.failover[%d]: %w", idx, err)
			}
		}
	}

	return affect, nil
}
