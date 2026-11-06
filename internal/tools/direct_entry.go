package tools

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/marco-rozz/ready/internal/secretstore"
)

// Direct secret entry writes values into the agent's Secret store only; it
// never touches config.yaml, so it needs no staging, no conflict check and no
// exclusion against the config manager. The values pass through here once,
// from the authenticated admin ZMQ session straight to the encrypted store.

var (
	// ErrSecretEntryActive reports that another direct-entry operation owns the daemon.
	ErrSecretEntryActive = errors.New("a secret entry operation is already active")
	// ErrSecretEntryExpired reports an operation that was released or timed out.
	ErrSecretEntryExpired = errors.New("the secret entry operation expired")
	// ErrNoPendingSecrets reports a begin with nothing to enter and no name.
	ErrNoPendingSecrets = errors.New("no pending secrets")
	// ErrUnknownName reports a Secret name that is neither referenced nor stored.
	ErrUnknownName = errors.New("unknown secret name")
	// ErrNameInUse reports a delete of a name a configuration field references.
	ErrNameInUse = errors.New("the secret is still referenced by the configuration")
	// ErrInvalidValue reports a value that breaks a value rule; see RuleCode.
	ErrInvalidValue = errors.New("the value breaks a value rule")
)

// SecretEntryOptions wires direct entry to the daemon.
type SecretEntryOptions struct {
	// Store is the agent's Secret store.
	Store *secretstore.Store
	// References returns the references of the configuration on disk (so a
	// reference just saved by the config manager is already pending).
	References func(context.Context) ([]secretstore.Reference, error)
	// Credentials summarizes login-managed Credentials for list; may be nil.
	Credentials func(context.Context) []CredentialSummary
	// ZMQName returns the Secret name behind channels.zmqcli.auth_token ("" when
	// that field is not a `!secret`); a write to it is adopted immediately.
	ZMQName func(context.Context) string
	// AdoptZMQ swaps the live channel secret after a ZMQ-name write.
	AdoptZMQ func(string)
	// OnChange is called after a write or delete (metadata-only invalidation).
	OnChange func(context.Context)
	// IdleTimeout ends an operation without activity (connection_idle_timeout).
	IdleTimeout time.Duration
	// Now is the clock (tests).
	Now func() time.Time
}

// CredentialSummary is one Credential for list: target, state, expiry.
type CredentialSummary struct {
	Target    string
	State     string
	ExpiresAt time.Time
}

// SecretEntryResult is the metadata-only outcome of an action.
type SecretEntryResult struct {
	OperationID string
	Outcome     string
	Secrets     []secretstore.SecretStatus
	External    []secretstore.ExternalStatus
	Credentials []CredentialSummary
	RuleCode    string
}

// Outcome vocabulary shared with the wire protocol.
const (
	OutcomeListed    = "listed"
	OutcomeActive    = "active"
	OutcomeCommitted = "committed"
	OutcomeDeleted   = "deleted"
	OutcomeReleased  = "released"
)

type entryOp struct {
	id       string
	owner    string
	lastSeen time.Time
}

// SecretEntry is the daemon's single direct-entry state.
type SecretEntry struct {
	opts SecretEntryOptions

	mu sync.Mutex
	op *entryOp
}

// NewSecretEntry builds the direct-entry service.
func NewSecretEntry(opts SecretEntryOptions) *SecretEntry {
	if opts.Now == nil {
		opts.Now = time.Now
	}

	return &SecretEntry{opts: opts}
}

// report derives every Secret and external reference state.
func (e *SecretEntry) report(ctx context.Context) (secretstore.Report, error) {
	refs, err := e.opts.References(ctx)
	if err != nil {
		return secretstore.Report{}, fmt.Errorf("failed to read the configuration references: %w", err)
	}
	payload, err := e.opts.Store.View(ctx)
	if errors.Is(err, secretstore.ErrNoStore) {
		payload = nil
	} else if err != nil {
		return secretstore.Report{}, fmt.Errorf("failed to read the secret store: %w", err)
	}

	return secretstore.Derive(payload, refs, nil), nil
}

// Report exposes the derived state (handshake snapshot, `pending_secrets_changed`).
func (e *SecretEntry) Report(ctx context.Context) (secretstore.Report, error) { return e.report(ctx) }

// List returns every Secret name, referencing fields and state, plus the
// Credential summaries — never a value.
func (e *SecretEntry) List(ctx context.Context) (SecretEntryResult, error) {
	rep, err := e.report(ctx)
	if err != nil {
		return SecretEntryResult{}, err
	}
	res := SecretEntryResult{Outcome: OutcomeListed, Secrets: rep.Secrets, External: rep.External}
	if e.opts.Credentials != nil {
		res.Credentials = e.opts.Credentials(ctx)
	}

	return res, nil
}

// expireLocked releases an operation idle past the timeout.
func (e *SecretEntry) expireLocked() {
	if e.op != nil && e.opts.IdleTimeout > 0 && e.opts.Now().Sub(e.op.lastSeen) > e.opts.IdleTimeout {
		e.op = nil
	}
}

// Begin opens the operation for owner. With a name it targets that Secret for
// rotation; without one it requires at least one pending `!secret` name.
func (e *SecretEntry) Begin(ctx context.Context, owner, name string) (SecretEntryResult, error) {
	rep, err := e.report(ctx)
	if err != nil {
		return SecretEntryResult{}, err
	}
	if name != "" {
		if !e.known(rep, name) {
			return SecretEntryResult{}, ErrUnknownName
		}
	} else if len(rep.PendingNames()) == 0 {
		return SecretEntryResult{}, ErrNoPendingSecrets
	}

	e.mu.Lock()
	defer e.mu.Unlock()
	e.expireLocked()
	if e.op != nil {
		return SecretEntryResult{}, ErrSecretEntryActive
	}
	e.op = &entryOp{id: "op-" + randomHexID(), owner: owner, lastSeen: e.opts.Now()}

	return SecretEntryResult{OperationID: e.op.id, Outcome: OutcomeActive, Secrets: rep.Secrets, External: rep.External}, nil
}

func (e *SecretEntry) known(rep secretstore.Report, name string) bool {
	for _, s := range rep.Secrets {
		if s.Name == name {
			return true
		}
	}

	return false
}

func (e *SecretEntry) fieldsOf(rep secretstore.Report, name string) []string {
	for _, s := range rep.Secrets {
		if s.Name == name {
			return s.Fields
		}
	}

	return nil
}

func (e *SecretEntry) touch(owner, opID string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.expireLocked()
	if e.op == nil || e.op.id != opID || e.op.owner != owner {
		return ErrSecretEntryExpired
	}
	e.op.lastSeen = e.opts.Now()

	return nil
}

// Submit validates value against the rules of every field referencing name
// and writes it to the store. The value is never retained or echoed.
func (e *SecretEntry) Submit(ctx context.Context, owner, opID, name, value string) (SecretEntryResult, error) {
	if err := e.touch(owner, opID); err != nil {
		return SecretEntryResult{}, err
	}
	rep, err := e.report(ctx)
	if err != nil {
		return SecretEntryResult{}, err
	}
	if !e.known(rep, name) {
		return SecretEntryResult{}, ErrUnknownName
	}
	if vErr := secretstore.CheckValue(e.fieldsOf(rep, name), value); vErr != nil {
		return SecretEntryResult{RuleCode: secretstore.RuleCode(vErr)}, fmt.Errorf("%w: %w", ErrInvalidValue, vErr)
	}
	err = e.opts.Store.Update(ctx, func(p *secretstore.Payload) error {
		p.Secrets[name] = secretstore.Secret{Value: value, UpdatedAt: e.opts.Now().UTC()}

		return nil
	})
	if err != nil {
		return SecretEntryResult{}, fmt.Errorf("failed to write the secret: %w", err)
	}
	if e.opts.AdoptZMQ != nil && e.opts.ZMQName != nil && e.opts.ZMQName(ctx) == name {
		e.opts.AdoptZMQ(value)
	}
	e.changed(ctx)

	return SecretEntryResult{OperationID: opID, Outcome: OutcomeCommitted}, nil
}

// Delete removes an unused Secret. A referenced name is refused.
func (e *SecretEntry) Delete(ctx context.Context, name string) (SecretEntryResult, error) {
	rep, err := e.report(ctx)
	if err != nil {
		return SecretEntryResult{}, err
	}
	if len(e.fieldsOf(rep, name)) > 0 {
		return SecretEntryResult{}, ErrNameInUse
	}
	present := false
	for _, s := range rep.Secrets {
		if s.Name == name {
			present = true
		}
	}
	if !present {
		return SecretEntryResult{}, ErrUnknownName
	}
	err = e.opts.Store.Update(ctx, func(p *secretstore.Payload) error {
		delete(p.Secrets, name)

		return nil
	})
	if err != nil {
		return SecretEntryResult{}, fmt.Errorf("failed to delete the secret: %w", err)
	}
	e.changed(ctx)

	return SecretEntryResult{Outcome: OutcomeDeleted}, nil
}

// End releases the operation.
func (e *SecretEntry) End(owner, opID string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.op == nil || e.op.id != opID || e.op.owner != owner {
		return ErrSecretEntryExpired
	}
	e.op = nil

	return nil
}

// Release drops the operation owned by connectionID (connection end/expiry).
func (e *SecretEntry) Release(connectionID string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.op != nil && e.op.owner == connectionID {
		e.op = nil
	}
}

func (e *SecretEntry) changed(ctx context.Context) {
	if e.opts.OnChange != nil {
		e.opts.OnChange(ctx)
	}
}

// SortedNames lists Secret names for stable rendering (helper for callers).
func SortedNames(rep secretstore.Report) []string {
	names := make([]string, 0, len(rep.Secrets))
	for _, s := range rep.Secrets {
		names = append(names, s.Name)
	}
	sort.Strings(names)

	return names
}
