package tools

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/rs/zerolog"

	"github.com/marco-rozz/ready/internal/config"
)

// Action names of the config-manager tool (single spelling per result).
const (
	actionRead    = "read"
	actionUpdate  = "update"
	actionSave    = "save"
	actionDiscard = "discard"
)

// ConfigManagerOptions wires the config-manager tool to the daemon's
// elected configuration and trusted Session identity. All identity and
// persistence inputs are injected so the tool cannot choose another config.
type ConfigManagerOptions struct {
	// ElectedPath is the config file elected at daemon boot — the tool's
	// ONLY editable configuration. It accepts no config target path.
	ElectedPath string
	// ProfileOverrides captures the boot --profile flag and
	// READY_DEFAULT_PROFILE env values for candidate validation precedence.
	ProfileOverrides config.ProfileOverrideInputs
	// ServeLocalChecks, when non-nil, runs the daemon's serve-local
	// enabled-channel/profile checks against a candidate. It must be
	// side-effect free.
	ServeLocalChecks func(*config.Config) error
	// SessionIdentity resolves the trusted Session identity from the
	// invocation context. The model cannot supply it; a missing identity
	// fails every action closed.
	SessionIdentity func(ctx context.Context) (sessionID string, ok bool)
	// SessionAddress identifies the conversation for operator guidance only.
	SessionAddress func(ctx context.Context) string
	// SessionTransport distinguishes ZMQ from the legacy stdin CLI address.
	SessionTransport func(ctx context.Context) string
	// SessionLoopback reports whether the invoking ZMQ Session is an
	// interactive connection on a numeric loopback listener (eligible for
	// 3-phase TUI Post-save activation).
	SessionLoopback func(ctx context.Context) bool
	// OnCommit is invoked after a Save commits a new configuration to disk
	// (never on a no-op save or a receipt replay). Observer panics/failures
	// are logged and never undo or misreport the committed save.
	OnCommit func(ctx context.Context, isLoopbackZMQTUI bool)
	// Logger receives metadata-only outcomes. No configuration content,
	// candidate bytes ever cross it.
	Logger zerolog.Logger

	// readSource and commit are injection seams for tests; production uses
	// config.ReadConfigSource and config.CommitCandidate.
	readSource func(ctx context.Context) (config.SourceState, error)
	commit     func(ctx context.Context, state config.SourceState, candidate []byte) (config.CommitReport, error)
	// nextStageID yields fresh staging identifiers (uuid in production).
	nextStageID func() string
}

// ConfigManager owns the daemon's single config staging buffer (design D5).
// One mutex guards every state transition: ownership, stage identity and
// revision are checked atomically with each action.
type ConfigManager struct {
	opts ConfigManagerOptions

	mu      sync.Mutex
	stage   *stageState
	receipt *saveReceipt
}

// stageState is the single staging buffer: the staged text (references only;
// hand-edited literals are masked) and its validation state.
type stageState struct {
	// owner is the trusted owning Session identity.
	owner string
	// address is trusted routing metadata, never an authorization identity.
	address string
	// stageID is a fresh random identifier for this staging allocation.
	stageID string
	// revision increments on every content-changing update.
	revision int
	// text is the staged text — the editing truth.
	text []byte
	// source is the raw elected file snapshot the staging was opened with
	// — private daemon memory, the conflict-check baseline.
	source config.SourceState
	// dirty marks any committed content-changing update.
	dirty bool
	// conflicted marks a refused save against a changed source base until
	// an explicit discard.
	conflicted bool
	// validation runs against the private candidate at the elected path.
	validation candidateValidationRef
}

// candidateValidationRef records the last known validation status.
type candidateValidationRef struct {
	ran bool
}

// saveReceipt is the metadata-only record of a Session's most recent
// completed save, keyed by stage_id and expected revision, so a save whose
// result never reached the owner replays safely without a second commit.
type saveReceipt struct {
	owner            string
	stageID          string
	expectedRevision int
	result           string // marshaled save result, replayed verbatim
}

// validationStatus reports the candidate validation outcome. Validation is
// structural (no secret I/O), so a staged text that passed is complete.
type validationStatus struct {
	Complete bool `json:"complete"`
}

// NewConfigManager constructs the manager. Production wiring injects
// config.ReadConfigSource/CommitCandidate bound to ElectedPath.
func NewConfigManager(opts ConfigManagerOptions) (*ConfigManager, error) {
	if opts.ElectedPath == "" {
		return nil, ErrConfigNoStaging
	}
	if opts.SessionIdentity == nil {
		return nil, ErrConfigNoSession
	}
	if opts.Logger.GetLevel() == 0 {
		opts.Logger = opts.Logger.With().Str("component", "config-manager").Logger()
	}
	if opts.readSource == nil {
		opts.readSource = func(ctx context.Context) (config.SourceState, error) {
			return config.ReadConfigSource(ctx, opts.ElectedPath)
		}
	}
	if opts.commit == nil {
		opts.commit = config.CommitCandidate
	}
	if opts.nextStageID == nil {
		opts.nextStageID = func() string {
			return "stage-" + randomHexID()
		}
	}

	return &ConfigManager{opts: opts}, nil
}

// Close releases all staging state (daemon shutdown). No takeover, no save.
func (m *ConfigManager) Close() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.stage = nil
	m.receipt = nil
}

// ReleaseSession releases the staging and receipt owned by sessionID, after
// that Session's in-flight work. Connection loss alone never reaches here.
func (m *ConfigManager) ReleaseSession(sessionID string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.stage != nil && m.stage.owner == sessionID {
		m.stage = nil
		m.opts.Logger.Info().Str("session", sessionID).Msg("config staging released at session end")
	}
	if m.receipt != nil && m.receipt.owner == sessionID {
		m.receipt = nil
	}
}

// identity resolves the trusted Session identity or fails closed.
func (m *ConfigManager) identity(ctx context.Context) (string, error) {
	if m.opts.SessionIdentity == nil {
		return "", ErrConfigNoSession
	}
	id, ok := m.opts.SessionIdentity(ctx)
	if !ok || strings.TrimSpace(id) == "" {
		return "", ErrConfigNoSession
	}

	return id, nil
}

// ownership checks the trusted identity against the current staging for
// foreign-busy refusal. Callers run inside the mutex-guarded action methods.
func (m *ConfigManager) checkOwner(ctx context.Context) error {
	id, err := m.identity(ctx)
	if err != nil {
		return err
	}
	if m.stage == nil {
		return fmt.Errorf("%w: read the configuration first", ErrConfigNoStaging)
	}
	if m.stage.owner != id {
		return ErrConfigStagingBusy
	}

	return nil
}

// checkMutation validates identity, stage id and expected revision for
// update/save. Invalid arguments fail before any state change.
func (m *ConfigManager) checkMutation(ctx context.Context, stageID string, expectedRevision int) error {
	if err := m.checkStageIdentity(ctx, stageID, expectedRevision); err != nil {
		return err
	}
	if m.stage.conflicted {
		return fmt.Errorf("%w: the staged editing base was invalidated by an external change — discard this staging and read again to reapply your intent", ErrConfigInvalidCandidate)
	}

	return nil
}

func (m *ConfigManager) isZMQSession(ctx context.Context) bool {
	return m.opts.SessionTransport != nil && m.opts.SessionTransport(ctx) == "zmq"
}

func (m *ConfigManager) isLoopbackZMQTUI(ctx context.Context) bool {
	return m.isZMQSession(ctx) && m.opts.SessionLoopback != nil && m.opts.SessionLoopback(ctx)
}

const (
	saveInstructionLoopbackTUI = "Committed. At the end of this turn the TUI will automatically open the RED secret-entry editor for any pending `!secret` references, run interactive OAuth logins for any `login_required` targets, and reload the daemon runtime in-process — no manual daemon restart is needed."
	saveInstructionImmediate   = "Committed. The daemon reloads its runtime automatically at the end of this turn — no manual daemon restart is needed. Any referenced `!secret` without a stored value or unauthenticated OAuth target remains off until completed in the local loopback ZMQ chat TUI or with `ready secrets set <name>` / `ready login`."
)

// checkStageIdentity validates identity, stage id and expected revision.
func (m *ConfigManager) checkStageIdentity(ctx context.Context, stageID string, expectedRevision int) error {
	if err := m.checkOwner(ctx); err != nil {
		return err
	}
	if m.stage.stageID != stageID || expectedRevision != m.stage.revision {
		return fmt.Errorf("%w: stage_id/expected_revision do not match the current staging state (revision %d)", ErrConfigStaleState, m.stage.revision)
	}

	return nil
}

// stagedView is the owner-facing view of the current staging returned by
// every successful read.
type stagedView struct {
	Action     string           `json:"action"`
	Config     string           `json:"config"`
	StageID    string           `json:"stage_id"`
	Revision   int              `json:"revision"`
	Text       string           `json:"text"`
	Dirty      bool             `json:"dirty"`
	Conflicted bool             `json:"conflicted"`
	Validation validationStatus `json:"validation"`
}

// Read implements the read action.
func (m *ConfigManager) Read(ctx context.Context) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	id, err := m.identity(ctx)
	if err != nil {
		return "", err
	}

	if m.stage != nil {
		if m.stage.owner != id {
			return "", ErrConfigStagingBusy
		}

		return m.renderStaged(), nil
	}

	// Open staging: read the elected file with its source protections. A
	// hand-edited literal credential is masked by node position, or the read
	// is refused when it cannot be masked safely.
	state, err := m.opts.readSource(ctx)
	if err != nil {
		return "", wrapUncommittedIO("reading the elected configuration failed", err)
	}
	staged, literals, err := maskLiterals(state.Data)
	if err != nil {
		return "", wrapCensusRefusal(err)
	}

	stageID := m.opts.nextStageID()
	address := id
	if m.opts.SessionAddress != nil && m.opts.SessionAddress(ctx) != "" {
		address = m.opts.SessionAddress(ctx)
	}
	m.stage = &stageState{
		owner:      id,
		address:    address,
		stageID:    stageID,
		revision:   0,
		text:       staged,
		source:     state,
		conflicted: false,
		validation: candidateValidationRef{ran: true},
	}

	m.opts.Logger.Info().
		Str("session", id).
		Str("stage_id", stageID).
		Str("config", m.opts.ElectedPath).
		Int("literals_masked", literals).
		Msg("config staging opened")

	return m.renderStaged(), nil
}

// renderStaged renders the owner's current staged view.
func (m *ConfigManager) renderStaged() string {
	st := m.stage
	view := stagedView{
		Action:     actionRead,
		Config:     m.opts.ElectedPath,
		StageID:    st.stageID,
		Revision:   st.revision,
		Text:       string(st.text),
		Dirty:      st.dirty,
		Conflicted: st.conflicted,
		Validation: validationStatus{Complete: st.validation.ran && !st.conflicted},
	}

	return marshalJSON(view)
}

// updateResult reports a committed update batch.
type updateResult struct {
	Action          string           `json:"action"`
	StageID         string           `json:"stage_id"`
	Applied         int              `json:"applied_edits"`
	Revision        int              `json:"revision"`
	NoOp            bool             `json:"no_op,omitempty"`
	Diff            string           `json:"diff,omitempty"`
	Validation      validationStatus `json:"validation"`
	ValidationError string           `json:"validation_error,omitempty"`
	Complete        bool             `json:"-"`
}

// editPair is one sequential exact-match replacement of a batch.
type editPair struct {
	OldText string `json:"old_text"`
	NewText string `json:"new_text"`
}

// Update implements the update action: sequential exact-match edits applied
// atomically over the staged text, then parse/literal-check/validation of the
// resulting state. Any failure keeps the prior buffer, map and revision.
func (m *ConfigManager) Update(ctx context.Context, stageID string, expectedRevision int, edits []editPair) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if err := m.checkMutation(ctx, stageID, expectedRevision); err != nil {
		return "", err
	}
	if len(edits) == 0 {
		return "", fmt.Errorf("%w: the update carries no edits — use discard, or read to re-inspect", ErrConfigInvalidEdit)
	}

	current := m.stage.text
	working := cloneBytes(current)
	for idx, edit := range edits {
		if edit.OldText == "" {
			return "", fmt.Errorf("%w: edit %d carries an empty old_text — give each edit unique surrounding context", ErrConfigInvalidEdit, idx)
		}
		pos, count := locateAll(working, edit.OldText)
		switch count {
		case 1:
			updated := make([]byte, 0, len(working)-len(edit.OldText)+len(edit.NewText))
			updated = append(updated, working[:pos[0]]...)
			updated = append(updated, edit.NewText...)
			updated = append(updated, working[pos[0]+len(edit.OldText):]...)
			working = updated
		default:
			hint := "matches multiple locations; add surrounding context to make it unique"
			if count == 0 {
				hint = "does not appear in the staged text; re-read the staging and adjust"
			}

			return "", fmt.Errorf("%w: edit %d old_text %s", ErrConfigInvalidEdit, idx, hint)
		}
	}

	if string(working) == string(current) {
		// A no-op batch: nothing changed; revision and dirty stay.
		result := updateResult{
			Action:     actionUpdate,
			StageID:    m.stage.stageID,
			Applied:    len(edits),
			Revision:   m.stage.revision,
			NoOp:       true,
			Validation: validationStatus{Complete: m.stage.validation.ran},
		}

		return marshalJSON(result), nil
	}

	if paths, litErr := newLiterals(working); litErr != nil {
		return "", wrapBatchRefusal(litErr)
	} else if len(paths) > 0 {
		return "", wrapBatchRefusal(literalRefusal(paths))
	}
	if referenceChanged(current, working, "agent", "id") {
		return "", fmt.Errorf("%w: agent.id is immutable; `ready init --new-identity` creates an independent copy", ErrConfigInvalidEdit)
	}
	if !m.isZMQSession(ctx) && referenceChanged(current, working, "channels", "zmqcli", "auth_token") {
		return "", fmt.Errorf("%w: channels.zmqcli.auth_token changes require the ZMQ TUI", ErrConfigInvalidEdit)
	}

	candidate, subErr := substituteLiterals(working)
	if subErr != nil {
		return "", wrapBatchRefusal(subErr)
	}
	if _, valErr := config.ValidateCandidate(ctx, candidate, m.opts.ElectedPath, m.opts.ProfileOverrides, m.opts.ServeLocalChecks); valErr != nil {
		// Safe diagnostics: field/cause information, never source snippets
		// or values (the config package guards its own errors).
		return "", fmt.Errorf("%w: %w", ErrConfigInvalidCandidate, valErr)
	}

	m.stage.text = append([]byte(nil), working...)
	m.stage.revision++
	m.stage.dirty = true
	m.stage.validation = candidateValidationRef{ran: true}

	result := updateResult{
		Action:     actionUpdate,
		StageID:    m.stage.stageID,
		Applied:    len(edits),
		Revision:   m.stage.revision,
		Diff:       unifiedDiff(current, working),
		Validation: validationStatus{Complete: true},
	}

	return marshalJSON(result), nil
}

// locateAll returns the starting offsets of needle in hay.
func locateAll(hay []byte, needle string) (positions []int, count int) {
	sub := []byte(needle)
	idx := 0
	for {
		found := indexBytes(hay[idx:], sub)
		if found < 0 {
			break
		}
		positions = append(positions, idx+found)
		count++
		idx += found + 1
	}
	if count > 1 {
		positions = positions[:1]
	}

	return positions, count
}

func indexBytes(hay, sub []byte) int {
	return strings.Index(string(hay), string(sub))
}

// saveResult reports the save outcome to the model and is replayed from the
// receipt on a repeated save.
type saveResult struct {
	Action      string   `json:"action"`
	Config      string   `json:"config"`
	Outcome     string   `json:"outcome"`  // committed | no_changes
	Revision    int      `json:"revision"` // the saved expected revision
	BackupPath  string   `json:"backup_path,omitempty"`
	Warnings    []string `json:"warnings,omitempty"`
	Instruction string   `json:"instruction"`
}

// Save implements the save action: private reconstruction, validation, then
// the shared, conflict-checked commit. No-op saves close staging without a
// backup or replacement; committed outcomes release staging; a positively
// not-committed failure retains everything for a safe retry.
func (m *ConfigManager) Save(ctx context.Context, stageID string, expectedRevision int) (string, error) {
	id, err := m.identity(ctx)
	if err != nil {
		return "", err
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	// Completed-save replay first: no staging interaction, no second commit.
	if m.stage == nil && m.receipt != nil && m.receipt.owner == id &&
		m.receipt.stageID == stageID && m.receipt.expectedRevision == expectedRevision {
		return m.receipt.result, nil
	}

	if err := m.checkMutation(ctx, stageID, expectedRevision); err != nil {
		// A foreign session gets nothing about the receipt: checkMutation
		// already refuses before any receipt comparison.

		return "", err
	}

	// A literal credential (masked or not) must be adopted before saving:
	// the committed file holds references only.
	if n, litErr := remainingLiterals(m.stage.text); litErr != nil {
		return "", wrapBatchRefusal(litErr)
	} else if n > 0 {
		return "", wrapBatchRefusal(fmt.Errorf("%w: %d hand-edited literal(s) remain; %s", ErrLiteralSecret, n, adoptGuidance))
	}
	candidate := cloneBytes(m.stage.text)

	if _, valErr := config.ValidateCandidate(ctx, candidate, m.opts.ElectedPath, m.opts.ProfileOverrides, m.opts.ServeLocalChecks); valErr != nil {
		return "", fmt.Errorf("%w: %w", ErrConfigInvalidCandidate, valErr)
	}
	m.stage.validation = candidateValidationRef{ran: true}

	// No-op detection against the staged snapshot, then a live source
	// equality check (both private compares).
	if bytesEqual(candidate, m.stage.source.Data) {
		current, err := m.opts.readSource(ctx)
		if err != nil {
			return "", wrapUncommittedIO("verifying the elected configuration failed", err)
		}
		if !bytesEqual(current.Data, m.stage.source.Data) || current.Mode != m.stage.source.Mode {
			m.stage.conflicted = true

			return "", fmt.Errorf(
				"%w: the elected configuration changed since the staging was opened — discard this staging and read again to reapply your intent",
				config.ErrConfigSourceConflict,
			)
		}

		stageIDStr := m.stage.stageID
		revision := m.stage.revision
		m.closeStageLocked(id, stageIDStr, revision, marshalJSON(saveResult{
			Action:      actionSave,
			Config:      m.opts.ElectedPath,
			Outcome:     "no_changes",
			Revision:    revision,
			Instruction: "No changes to save: the staged state matches the elected configuration. Re-run read to open fresh staging.",
		}))

		return m.receipt.result, nil
	}

	report, commitErr := m.opts.commit(ctx, m.stage.source, candidate)
	switch {
	case commitErr == nil:
		// Committed cleanly.
	case report.Committed || isTransientPostCommit(commitErr):
		// Committed with a durability/retention warning: never retry, no
		// second write, no rollback implication.
		report.Warnings = append(report.Warnings, safeCommitWarning(commitErr))
	case errors.Is(commitErr, config.ErrConfigSourceConflict):
		m.stage.conflicted = true

		return "", fmt.Errorf(
			"%w: the elected configuration changed since the staging was opened (identity/mode/bytes) — discard this staging and read again to reapply your intent",
			config.ErrConfigSourceConflict,
		)
	case isTransientCommit(commitErr):
		// Positively not committed, retry safe; state retained.
		m.opts.Logger.Warn().Err(commitErr).Msg("config save not committed (transient)")

		return "", fmt.Errorf("the configuration was NOT saved (retry is safe against the same revision): %w", commitErr)
	default:
		return "", fmt.Errorf("saving the configuration failed: %w", commitErr)
	}

	stageIDStr := m.stage.stageID
	revision := m.stage.revision
	isLoopbackTUI := m.isLoopbackZMQTUI(ctx)
	instruction := saveInstructionImmediate
	if isLoopbackTUI {
		instruction = saveInstructionLoopbackTUI
	}
	result := saveResult{
		Action:      "save",
		Config:      m.opts.ElectedPath,
		Outcome:     "committed",
		Revision:    revision,
		BackupPath:  report.BackupPath,
		Warnings:    report.Warnings,
		Instruction: instruction,
	}

	m.closeStageLocked(id, stageIDStr, revision, marshalJSON(result))
	savedResult := m.receipt.result

	if m.opts.OnCommit != nil {
		func() {
			defer func() {
				if r := recover(); r != nil {
					m.opts.Logger.Warn().Interface("panic", r).Msg("config save commit observer panicked")
				}
			}()
			m.opts.OnCommit(ctx, isLoopbackTUI)
		}()
	}

	return savedResult, nil
}

// isTransientPostCommit reports a durability failure after replacement —
// committed is true, the error is only honest uncertainty reporting.
func isTransientPostCommit(err error) bool {
	return errors.Is(err, config.ErrCommitPostCommit)
}

// closeStageLocked records the receipt (metadata only) and releases staging.
func (m *ConfigManager) closeStageLocked(owner, stageID string, revision int, resultJSON string) {
	m.receipt = &saveReceipt{owner: owner, stageID: stageID, expectedRevision: revision, result: resultJSON}
	m.stage = nil
	m.opts.Logger.Info().
		Str("session", owner).
		Str("stage_id", stageID).
		Int("revision", revision).
		Msg("config save completed; staging released")
}

// Discard implements the discard action: release staging, keep the elected
// file untouched.
func (m *ConfigManager) Discard(ctx context.Context, stageID string, expectedRevision int) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Discard is the recovery path for a conflicted staging: identity,
	// stage and revision still apply, but the invalidated base does not
	// block an explicit release.
	if err := m.checkStageIdentity(ctx, stageID, expectedRevision); err != nil {
		return "", err
	}

	id := m.stage.owner
	m.stage = nil
	m.opts.Logger.Info().Str("session", id).Str("stage_id", stageID).Msg("config staging discarded")

	return marshalJSON(map[string]string{
		"action":      "discard",
		"stage_id":    stageID,
		"outcome":     "discarded",
		"instruction": "Staging released without touching the elected configuration. Run read to open fresh staging.",
	}), nil
}

// marshalJSON adds a newline-free JSON rendering for tool results.
func marshalJSON(v any) string {
	return jsonMarshal(v)
}

// safeCommitWarning renders a post-commit failure as a metadata-only warning.
func safeCommitWarning(err error) string {
	if errors.Is(err, config.ErrCommitPostCommit) {
		return "configuration committed; directory durability could not be confirmed"
	}

	return "configuration committed with a persistence warning"
}
