package initwizard

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/rs/zerolog"

	"github.com/marco-rozz/ready/internal/config"
	"github.com/marco-rozz/ready/internal/secretstore"
)

// ErrNoIdentityToReplace reports `--new-identity` on a configuration that has no
// agent identity yet (the repair menu adds one).
var ErrNoIdentityToReplace = errors.New("this configuration has no agent identity; run `ready init` and choose to add one")

// runNewIdentity gives an existing agent a fresh, independent identity — the
// way to make a COPY of an agent directory a separate agent. Under the agent's
// physical ownership it: (1) for an os_vault key, copies the key to the new
// account; (2) rewrites the agent_id bound into the Secret store; (3) rewrites
// agent.id in the configuration. The store is rewritten first and the config
// second: a crash between the two leaves a mismatch that re-running this
// command completes.
func runNewIdentity(ctx context.Context, o Options, res config.Resolution) error {
	lks, err := launchLocks(ctx, res.AgentDir)
	if err != nil {
		return err
	}
	defer func() { _ = lks.release() }()

	source, err := config.ReadConfigSource(ctx, res.Path)
	if err != nil {
		return fmt.Errorf("read config source: %w", err)
	}
	cfg, err := config.DecodeForRepair(ctx, source.Data, res.Path)
	if err != nil {
		return fmt.Errorf("%w: %w", errUnsupportedDocument, err)
	}
	if cfg.Agent.ID == "" {
		return ErrNoIdentityToReplace
	}
	spec := cfg.StoreKeySpec()

	// A store already bound to another id means an earlier run stopped halfway:
	// finish it by adopting that id in the configuration.
	probe, err := secretstore.New(secretstore.Options{Dir: res.AgentDir, Key: spec, ReadOnly: true, Log: zerolog.Nop()})
	if err != nil {
		return err
	}
	newID := ""
	if probe.Exists() {
		p, verr := probe.View(ctx)
		if verr != nil {
			return fmt.Errorf("cannot open the secret store: %w", verr)
		}
		if p.AgentID != cfg.Agent.ID {
			newID = p.AgentID
		}
	}
	if newID == "" {
		u, gerr := uuid.NewV7()
		if gerr != nil {
			return fmt.Errorf("failed to generate the agent id: %w", gerr)
		}
		newID = u.String()
		if probe.Exists() {
			if err = rebindStore(ctx, res, spec, cfg.Agent.ID, newID); err != nil {
				return err
			}
		}
	}

	next, err := replaceScalarSpan(source.Data, []string{"agent", "id"}, newID)
	if err != nil {
		return fmt.Errorf("agent.id: %w", err)
	}
	if _, err = commitRepaired(ctx, res, source, next); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(o.stdout(), "New agent identity %s committed for %s.\nThis agent no longer shares its identity (or its vault entry) with the directory it was copied from.\n", newID, res.AgentDir)

	return nil
}

// rebindStore moves the store to newID; for an os_vault key it first copies the
// key to the new account, so the new identity can open the store.
func rebindStore(ctx context.Context, res config.Resolution, spec secretstore.KeySpec, oldID, newID string) error {
	old, err := secretstore.New(secretstore.Options{Dir: res.AgentDir, Key: spec, AgentID: oldID, Log: zerolog.Nop()})
	if err != nil {
		return err
	}
	if spec.Location == secretstore.KeyOSVault {
		id, ierr := old.Identity(ctx)
		if ierr != nil {
			return fmt.Errorf("cannot read the store key: %w", ierr)
		}
		dest := spec
		dest.AgentID = newID
		if err = secretstore.InstallKeyAt(ctx, res.AgentDir, dest, oldID, id); err != nil {
			return fmt.Errorf("cannot copy the vault key to the new identity: %w", err)
		}
	}

	return old.Rebind(ctx, newID)
}
