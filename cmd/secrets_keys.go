package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"filippo.io/age"
	"github.com/urfave/cli/v3"

	"github.com/marco-rozz/ready/internal/config"
	"github.com/marco-rozz/ready/internal/secretstore"
)

// parseImportedKey reads a bare or passphrase-wrapped Store key.
func parseImportedKey(raw []byte, sio secretsIO) (*age.X25519Identity, error) {
	if secretstore.IsWrapped(raw) {
		pass, err := sio.hidden("Passphrase of the exported key (input hidden): ")
		if err != nil {
			return nil, err
		}

		return secretstore.UnwrapIdentity(raw, pass)
	}

	return secretstore.ParseIdentity(string(raw))
}

func secretsMoveKey(ctx context.Context, c *cli.Command, sio secretsIO) error {
	dest := secretstore.KeySpec{Location: secretstore.KeyLocation(c.String("to")), File: c.String("path")}
	env, release, err := openSecretsEnv(ctx, c, sio, false)
	if err != nil {
		return err
	}
	defer release()
	dest.AgentID = env.cfg.Agent.ID
	if err = dest.Validate(); err != nil {
		return err
	}
	old := env.cfg.StoreKeySpec()
	if strings.EqualFold(string(old.Location), string(dest.Location)) && old.File == dest.File {
		return errors.New("the store key is already at that location")
	}
	id, err := env.store.Identity(ctx)
	if err != nil {
		return fmt.Errorf("cannot read the current store key: %w", err)
	}

	// 1. For an external file we write it ourselves (READY then only reads it).
	if dest.Location == secretstore.KeyFile && !fileExists(dest.File) {
		if err = secretstore.WriteNewPrivateFile(dest.File, []byte(id.String()+"\n")); err != nil {
			return err
		}
	}
	// 2. Install at the destination and prove the store opens through it.
	if err = secretstore.InstallKeyAt(ctx, env.cfg.RuntimeHome, dest, env.cfg.Agent.ID, id); err != nil {
		return fmt.Errorf("could not install the key at %s: %w", dest.Location, err)
	}
	// 3. Commit the configuration change through the shared commit.
	state, err := config.ReadConfigSource(ctx, env.cfg.ElectedPath)
	if err != nil {
		return err
	}
	next, err := config.SetStoreKeyLocation(state.Data, string(dest.Location), dest.File)
	if err != nil {
		return err
	}
	if _, err = config.ValidateCandidate(ctx, next, env.cfg.ElectedPath, config.ProfileOverrideInputs{}, nil); err != nil {
		return fmt.Errorf("the configuration change was rejected; the old key is untouched: %w", err)
	}
	if report, cerr := config.CommitCandidate(ctx, state, next); cerr != nil && !report.Committed {
		return fmt.Errorf("the configuration was not changed; the old key is untouched: %w", cerr)
	}
	// 4. Only now remove the old copy (never an external file).
	if rerr := secretstore.RemoveKey(ctx, env.cfg.RuntimeHome, old); rerr != nil {
		_, _ = fmt.Fprintf(sio.stderr, "warning: the old key copy could not be removed: %v\n", rerr)
	}
	_, _ = fmt.Fprintf(sio.stdout, "store key moved to %s; restart READY to use the new location\n", dest.Location)
	if old.Location == secretstore.KeyFile {
		_, _ = fmt.Fprintf(sio.stdout, "the external key file %s was left in place\n", old.File)
	}

	return nil
}

func secretsForgetKey(ctx context.Context, c *cli.Command, sio secretsIO) error {
	res, err := config.SelectAgentDir(ctx, c.String(flagAgentDirName))
	if err != nil {
		return err
	}
	cfg, err := config.Load(ctx, res.Path)
	if err != nil {
		return err
	}
	spec := cfg.StoreKeySpec()
	switch spec.Location {
	case secretstore.KeyFile:
		_, _ = fmt.Fprintf(sio.stdout, "the store key is the external file %s; READY never deletes it — remove it yourself if you want it gone\n", spec.File)

		return nil
	case secretstore.KeyOSVault:
	default:
		_, _ = fmt.Fprintln(sio.stdout, "the store key lives in the agent directory (secrets.key); there is no vault entry to forget — it goes away with the directory")

		return nil
	}
	if !c.Bool("yes") {
		return errors.New("forgetting the key makes the store unreadable here, and for any COPY of this agent on this host that uses the same vault entry; pass --yes to confirm")
	}
	if err = secretstore.RemoveKey(ctx, res.AgentDir, spec); err != nil {
		return fmt.Errorf("could not delete the vault entry: %w", err)
	}
	_, _ = fmt.Fprintln(sio.stdout, "vault entry deleted; any copy of this agent on this host that shares it lost its key too")

	return nil
}

func fileExists(path string) bool {
	_, err := os.Lstat(path)

	return err == nil
}
