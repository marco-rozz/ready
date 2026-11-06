package secretstore

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"filippo.io/age"
	"github.com/rs/zerolog"

	"github.com/marco-rozz/ready/internal/daemonlock"
)

// lockTimeout bounds the wait for another writer's store lock.
const lockTimeout = 10 * time.Second

var (
	// ErrNoStore reports that the agent has no secrets.age yet.
	ErrNoStore = errors.New("secret store: no store exists for this agent")
	// ErrAgentMismatch reports a store bound to a different Agent ID than the config.
	ErrAgentMismatch = errors.New("secret store: store belongs to a different agent")
	// ErrKeyOrphanVault reports a vault entry for this Agent ID with no store:
	// it may be another copy's key, so READY never reuses or overwrites it.
	ErrKeyOrphanVault = errors.New("secret store: the vault already holds a key for this agent id but no store exists")
)

// Options configures a Store.
type Options struct {
	// Dir is the physical agent directory.
	Dir string
	// Key is the configured Store key location.
	Key KeySpec
	// AgentID is the config's agent.id; when set it must equal the payload's.
	AgentID string
	// ReadOnly forbids creating the store/key and any write (login --status, chat).
	ReadOnly bool
	Log      zerolog.Logger
}

// Store is one agent's encrypted Secret store.
type Store struct {
	opt     Options
	backend keyBackend
	log     zerolog.Logger

	mu      sync.Mutex
	cache   *Payload
	cacheFI os.FileInfo
	degrade bool // last read fell back or failed (WARN once per transition)
}

// New builds a Store. It performs no I/O.
func New(opt Options) (*Store, error) {
	if opt.Key.Location == KeyOSVault && opt.Key.AgentID == "" {
		opt.Key.AgentID = opt.AgentID
	}
	b, err := newBackend(opt.Dir, opt.Key)
	if err != nil {
		return nil, err
	}

	return &Store{opt: opt, backend: b, log: opt.Log.With().Str("component", "secretstore").Logger()}, nil
}

func (s *Store) path() string     { return filepath.Join(s.opt.Dir, StoreFileName) }
func (s *Store) prevPath() string { return s.path() + prevSuffix }
func (s *Store) lockPath() string { return filepath.Join(s.opt.Dir, lockFileName) }

// Exists reports whether secrets.age is present.
func (s *Store) Exists() bool {
	_, err := os.Stat(s.path())

	return err == nil
}

// View returns the decrypted payload (a private copy). It is cached by file
// identity: one stat decides whether to decrypt again. A failure keeps the last
// good view with one WARN per transition; with no prior view it is an error.
func (s *Store) View(ctx context.Context) (*Payload, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	fi, err := os.Stat(s.path())
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNoStore
	}
	if err != nil {
		return s.keepOrFail(fmt.Errorf("failed to stat the secret store: %w", err))
	}
	if s.cache != nil && sameFile(s.cacheFI, fi) {
		return s.cache.Clone(), nil
	}
	p, err := s.readLocked(ctx)
	if err != nil {
		return s.keepOrFail(err)
	}
	s.cache, s.cacheFI = p, fi
	s.recovered()

	return p.Clone(), nil
}

func sameFile(a, b os.FileInfo) bool {
	return a != nil && b != nil && os.SameFile(a, b) && a.Size() == b.Size() && a.ModTime().Equal(b.ModTime())
}

func (s *Store) keepOrFail(err error) (*Payload, error) {
	if s.cache == nil {
		return nil, err
	}
	if !s.degrade {
		s.degrade = true
		s.log.Warn().Err(err).Msg("secret store unreadable; keeping the last good view")
	}

	return s.cache.Clone(), nil
}

func (s *Store) recovered() {
	if s.degrade {
		s.degrade = false
		s.log.Info().Msg("secret store readable again")
	}
}

// readLocked decrypts secrets.age, falling back to the previous generation.
func (s *Store) readLocked(ctx context.Context) (*Payload, error) {
	id, err := s.backend.load(ctx)
	if err != nil {
		return nil, fmt.Errorf("cannot open the secret store: %w", err)
	}
	p, err := s.readFile(id, s.path())
	if err == nil {
		return s.checkAgent(p)
	}
	// A wrong key fails the previous generation too, so this only rescues a
	// corrupt or torn current file.
	if prev, perr := s.readFile(id, s.prevPath()); perr == nil {
		s.log.Warn().Err(err).Msg("secret store unreadable; using the previous generation")

		return s.checkAgent(prev)
	}

	return nil, err
}

func (s *Store) readFile(id *age.X25519Identity, path string) (*Payload, error) {
	blob, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read %s: %w", filepath.Base(path), err)
	}

	return openPayload(id, blob)
}

func (s *Store) checkAgent(p *Payload) (*Payload, error) {
	if s.opt.AgentID != "" && p.AgentID != s.opt.AgentID {
		return nil, fmt.Errorf("%w: config agent.id is %q, store agent_id is %q", ErrAgentMismatch, s.opt.AgentID, p.AgentID)
	}

	return p, nil
}

// Update applies fn to a fresh, private copy of the payload under the store
// lock and persists the result. fn sees the latest on-disk state, so it is the
// place for per-entry compare-before-save. The first Update creates the store
// and its key together (never a key over an existing store).
func (s *Store) Update(ctx context.Context, fn func(*Payload) error) error {
	return s.update(ctx, fn, "")
}

// Rebind changes the agent_id the store is bound to (`init --new-identity`).
// It is the only writer allowed to; the caller commits the matching
// configuration change afterwards, and re-running it completes an interrupted
// pair.
func (s *Store) Rebind(ctx context.Context, newAgentID string) error {
	if newAgentID == "" {
		return errors.New("secret store: a new agent id is required")
	}

	return s.update(ctx, func(*Payload) error { return nil }, newAgentID)
}

func (s *Store) update(ctx context.Context, fn func(*Payload) error, rebindTo string) error {
	if s.opt.ReadOnly {
		return errors.New("secret store: opened read-only")
	}
	if err := ensurePrivateDir(s.opt.Dir); err != nil {
		return err
	}
	lock, err := daemonlock.WaitForLock(ctx, s.lockPath(), false, lockTimeout)
	if err != nil {
		return fmt.Errorf("failed to lock the secret store: %w", err)
	}
	defer func() { _ = lock.Release() }()

	s.mu.Lock()
	defer s.mu.Unlock()

	id, current, err := s.prepareLocked(ctx)
	if err != nil {
		return err
	}
	next := current.Clone()
	if err = fn(next); err != nil {
		return err
	}
	next.Format, next.AgentID = PayloadFormat, current.AgentID
	if rebindTo != "" {
		next.AgentID = rebindTo
	}
	blob, err := next.seal(id)
	if err != nil {
		return err
	}
	if old, rerr := os.ReadFile(s.path()); rerr == nil {
		if err = writeFileAtomic(s.prevPath(), old); err != nil {
			return fmt.Errorf("failed to keep the previous generation: %w", err)
		}
	}
	if err = writeFileAtomic(s.path(), blob); err != nil {
		return err
	}
	if fi, serr := os.Stat(s.path()); serr == nil {
		s.cache, s.cacheFI = next, fi
		s.recovered()
	}

	return nil
}

// prepareLocked returns the key and the current payload, creating both on the
// very first write. Caller holds the store lock and s.mu.
func (s *Store) prepareLocked(ctx context.Context) (*age.X25519Identity, *Payload, error) {
	if s.Exists() {
		id, err := s.backend.load(ctx)
		if err != nil {
			return nil, nil, fmt.Errorf("cannot open the secret store: %w", err)
		}
		p, err := s.readFile(id, s.path())
		if err != nil {
			prev, perr := s.readFile(id, s.prevPath())
			if perr != nil {
				return nil, nil, err
			}
			p = prev
		}
		p, err = s.checkAgent(p)
		if err != nil {
			return nil, nil, err
		}

		return id, p, nil
	}
	if s.opt.AgentID == "" {
		return nil, nil, errors.New("secret store: cannot create a store without an agent id")
	}
	id, err := s.ensureNewKey(ctx)
	if err != nil {
		return nil, nil, err
	}

	return id, NewPayload(s.opt.AgentID), nil
}

// ensureNewKey installs a key for a store that does not exist yet.
func (s *Store) ensureNewKey(ctx context.Context) (*age.X25519Identity, error) {
	existing, err := s.backend.load(ctx)
	switch {
	case err == nil:
		// agent_dir: an orphan key from a crash before the first store write
		// encrypted nothing, so it is safe to reuse. A vault entry may belong
		// to another copy of this agent, and an external file is the operator's.
		if s.opt.Key.normalized() == KeyAgentDir || s.opt.Key.normalized() == KeyFile {
			return existing, nil
		}

		return nil, ErrKeyOrphanVault
	case errors.Is(err, ErrKeyNotFound):
		if s.opt.Key.normalized() == KeyFile {
			return nil, fmt.Errorf("%w: %s", ErrKeyNotFound, s.opt.Key.File)
		}
	default:
		return nil, fmt.Errorf("cannot open the store key: %w", err)
	}
	id, err := GenerateIdentity()
	if err != nil {
		return nil, err
	}
	if err = s.backend.save(ctx, id); err != nil {
		return nil, fmt.Errorf("failed to install the store key: %w", err)
	}

	return id, nil
}
