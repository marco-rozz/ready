// Fresh publication (design D4/A.7): under the maintenance gate and
// temporary physical ownership, create the encrypted Secret store and its key,
// store the provider key and the generated chat secret, validate the complete
// candidate at the elected path, and publish config.yaml last (it holds only
// references), then prepare the initial skill copy under the same ownership.
// No existing state is ever clobbered, a key is never created over an existing
// store, and an interrupted publication removes only the files this invocation
// created and the committed config provably does not reference.
package initwizard

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/google/uuid"
	"github.com/marco-rozz/ready/internal/config"
	"github.com/marco-rozz/ready/internal/daemonlock"
	"github.com/marco-rozz/ready/internal/secretstore"
	"github.com/marco-rozz/ready/internal/skillssync"
	"github.com/rs/zerolog"
)

// prepareOutcome reports one completed fresh init before any follow-up
// offer. CommittedAlready carries the after-interruption honesty rule: a
// publication that happened before an error surfaces is still stated as
// committed, never implied rolled back.
type prepareOutcome struct {
	Resolution config.Resolution
	Choice     ProviderChoice
	// Candidate is the decoded view of the committed config (the source
	// of the follow-up verification's registry).
	Candidate *config.Config
	// AgentID and AgentName are the identity written to the new config.
	AgentID   string
	AgentName string
	// ConfigCommitted is true once config.yaml exists with the generated
	// bytes.
	ConfigCommitted bool
	// SkillsSynced reports the initial copy from the installed library,
	// performed after configuration publication while ownership is held.
	SkillsSynced bool
	// Warnings carries committed-but-not-perfect details (durability).
	Warnings []string
}

// locks contains the two OS locks a mutation holds over one agent.
type locks struct {
	maintenance *daemonlock.Lock
	ownership   *daemonlock.Lock
}

// release drops both gates in reverse acquisition order; release errors
// join but never block the caller's own error.
func (l locks) release() error {
	var err error
	if l.ownership != nil {
		if rerr := l.ownership.Release(); rerr != nil {
			err = errors.Join(err, fmt.Errorf("release ownership: %w", rerr))
		}
	}
	if l.maintenance != nil {
		if rerr := l.maintenance.Release(); rerr != nil {
			err = errors.Join(err, fmt.Errorf("release maintenance gate: %w", rerr))
		}
	}

	return err
}

// launchLocks creates the missing agent directory, then takes the shared
// maintenance gate and exclusive ownership. The ownership lock is the
// serialization point for concurrent inits and daemons on the same physical
// agent.
func launchLocks(ctx context.Context, agentDir string) (locks, error) {
	// The locks are files under the agent directory: create it first, so a
	// fresh bootstrap and every later init/serve share one identity.
	if derr := ensureAgentDir(ctx, agentDir); derr != nil {
		return locks{}, derr
	}

	maintenance, err := daemonlock.AcquireMaintenanceShared(ctx, agentDir)
	if err != nil {
		return locks{}, fmt.Errorf("maintenance gate on %s: %w (another ready process owns this agent right now)", agentDir, err)
	}

	ownership, err := daemonlock.AcquireOwnership(ctx, agentDir)
	if err != nil {
		_ = maintenance.Release()

		return locks{}, fmt.Errorf("physical ownership of %s: %w — a daemon or another init is active on this agent", agentDir, err)
	}

	return locks{maintenance: maintenance, ownership: ownership}, nil
}

// prepareFresh runs the interview-following fresh publication of one agent.
// The guided admin skill is injected only when its library is installed;
// existing state survives every failure. Only this invocation's created files
// are removed on failure.
func prepareFresh(
	ctx context.Context,
	res config.Resolution,
	choice ProviderChoice,
	randHex func(int) (string, error),
	readyGuideInstalled bool,
) (prepareOutcome, error) {
	out := prepareOutcome{Resolution: res, Choice: choice}

	if !readyGuideInstalled {
		out.Warnings = append(out.Warnings, fmt.Sprintf("The ready-guide setup skill is missing from %s.\n  Your configuration can still be saved, but the guided setup skill will be unavailable.\n  Install or update the shared READY skill library, then add ready-guide to\n  profiles.admin.inject_skills to enable it. serve will retry the skill sync.", config.SharedLibraryDir()))
	}

	// A rescue rule: existing but empty agent directories (created by a
	// previous crashed init) receive their secrets, never configured
	// logging, never a config clobber.
	if _, err := os.Stat(res.AgentDir); err != nil && !os.IsNotExist(err) {
		return out, fmt.Errorf("agent directory %s: %w", res.AgentDir, err)
	}

	lks, launchErr := launchLocks(ctx, res.AgentDir)
	if launchErr != nil {
		return out, launchErr
	}

	created := make([]string, 0, 2)
	committed := false
	failure := func(err error) (prepareOutcome, error) {
		_, configErr := os.Lstat(res.Path)
		if !committed && errors.Is(configErr, os.ErrNotExist) {
			// D4 cleanup rule: remove only invocation-created files that
			// the committed config provably does not reference (absent
			// config ⇒ unreferenced). Crash leftovers are never touched
			// (they were never created by this invocation).
			for _, p := range created {
				_ = os.Remove(p)
			}
		}

		if relErr := lks.release(); relErr != nil {
			err = errors.Join(err, relErr)
		}

		return out, err
	}

	// 1. Identity. An orphan store left by a crashed init keeps its agent id,
	// so a retried init never mints a second identity for the same store.
	storePath := filepath.Join(res.AgentDir, secretstore.StoreFileName)
	keyPath := filepath.Join(res.AgentDir, secretstore.KeyFileName)
	storeExisted, keyExisted := fileExists(storePath), fileExists(keyPath)
	agentID, err := freshAgentID(ctx, res.AgentDir, storeExisted)
	if err != nil {
		return failure(err)
	}
	agentName := hostName()
	out.AgentID, out.AgentName = agentID, agentName

	// 2. The Secret store and key (created together at the first write; an
	// existing store is reused with its own key, never re-keyed): the chat
	// secret, and the provider key where required.
	zmqHex, err := randHex(generateZMQTokenByteLength)
	if err != nil {
		return failure(err)
	}
	if choice.needsAPIKey() {
		if vErr := secretstore.CheckValue([]string{"llm.providers." + choice.Instance + ".api_key"}, choice.APIKey); vErr != nil {
			return failure(fmt.Errorf("the provider key was refused: %w", vErr))
		}
	}
	store, err := secretstore.New(secretstore.Options{
		Dir:     res.AgentDir,
		Key:     secretstore.KeySpec{Location: secretstore.KeyAgentDir},
		AgentID: agentID,
		Log:     zerolog.Nop(),
	})
	if err != nil {
		return failure(err)
	}
	err = store.Update(ctx, func(p *secretstore.Payload) error {
		p.Secrets[zmqSecretName()] = secretstore.Secret{Value: zmqHex, UpdatedAt: time.Now().UTC()}
		if choice.needsAPIKey() {
			p.Secrets[secretNameFor(choice.Instance)] = secretstore.Secret{Value: choice.APIKey, UpdatedAt: time.Now().UTC()}
		}

		return nil
	})
	if err != nil {
		if !storeExisted {
			created = append(created, storePath)
		}
		if !keyExisted {
			created = append(created, keyPath)
		}

		return failure(fmt.Errorf("failed to create the secret store: %w", err))
	}
	if !storeExisted {
		created = append(created, storePath)
	}
	if !keyExisted {
		created = append(created, keyPath)
	}

	// 3. The candidate configuration, validated privately at the elected
	// path (same rules as boot) before anything is published.
	candidate := generateConfig(choice, agentID, agentName, readyGuideInstalled)
	candidateCfg, verr := config.ValidateCandidate(ctx, candidate, res.Path, config.ProfileOverrideInputs{}, nil)
	if verr != nil {
		return failure(fmt.Errorf("generated config failed validation: %w", verr))
	}

	// 4. Publish config strictly last, exclusively (bounded by the captured
	// ownership no other process holds).
	if _, err := os.Lstat(res.Path); err == nil {
		return failure(fmt.Errorf("%s appeared during init — refusing to clobber (remove it, or rerun init after backing it up)", res.Path))
	}
	published, publishErr := writeConfigExclusive(ctx, res.Path, candidate)
	committed = published
	out.ConfigCommitted = published
	if publishErr != nil {
		if !published {
			return failure(publishErr)
		}
		out.Warnings = append(out.Warnings, publishErr.Error())
	}
	out.Candidate = candidateCfg
	if err := syncDirOf(ctx, res.AgentDir); err != nil {
		out.Warnings = append(out.Warnings, fmt.Sprintf("descriptor sync failed: %v", err))
	}

	// Configuration and credentials are committed before skill preparation.
	// A copy failure must preserve that fact; serve can retry the same sync.
	var syncErr error
	out.SkillsSynced, syncErr = syncInitialSkills(ctx, candidateCfg)
	if syncErr != nil {
		return failure(fmt.Errorf("configuration saved, but initial skill copy failed (serve will retry): %w", syncErr))
	}

	if relErr := lks.release(); relErr != nil {
		return out, errors.Join(fmt.Errorf("release init locks: %w", relErr))
	}

	return out, nil
}

func syncInitialSkills(ctx context.Context, cfg *config.Config) (bool, error) {
	info, err := os.Stat(cfg.Skills.LibraryPath)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read shared skill library: %w", err)
	}
	if !info.IsDir() {
		return false, fmt.Errorf("shared skill library %s must be a directory", cfg.Skills.LibraryPath)
	}
	if target, targetErr := os.Stat(filepath.Clean(cfg.Skills.Path)); targetErr == nil {
		if !target.IsDir() {
			return false, fmt.Errorf("skill destination %s must be a directory", cfg.Skills.Path)
		}
	} else if !errors.Is(targetErr, os.ErrNotExist) {
		return false, fmt.Errorf("read skill destination: %w", targetErr)
	}
	if err := skillssync.Run(ctx, cfg.Skills, cfg.RuntimeHome, zerolog.Nop()); err != nil {
		return false, err
	}

	return true, nil
}

// writeConfigExclusive links a fully flushed private sibling into place.
// The final name becomes visible only after all bytes have been written;
// Link refuses existing targets atomically on every supported platform.
func writeConfigExclusive(ctx context.Context, path string, candidate []byte) (bool, error) {
	suffix, err := randomTokenHex(16)
	if err != nil {
		return false, err
	}
	temp := filepath.Join(filepath.Dir(path), ".ready-init-"+suffix)
	if err := createExclusiveFile(ctx, temp, candidate); err != nil {
		return false, err
	}
	published := false
	defer func() {
		if !published {
			_ = os.Remove(temp)
		}
	}()
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if err := os.Link(temp, path); err != nil {
		return false, fmt.Errorf("publish config exclusively: %w", err)
	}
	published = true
	if err := os.Remove(temp); err != nil {
		return true, fmt.Errorf("config committed; remove the temporary link %s before future config edits: %w", temp, err)
	}

	return true, nil
}

func fileExists(path string) bool {
	_, err := os.Lstat(path)

	return err == nil
}

// freshAgentID reuses the agent id of an existing (orphan) store, otherwise
// mints a new UUIDv7.
func freshAgentID(ctx context.Context, agentDir string, storeExisted bool) (string, error) {
	if storeExisted {
		probe, err := secretstore.New(secretstore.Options{
			Dir: agentDir, Key: secretstore.KeySpec{Location: secretstore.KeyAgentDir}, ReadOnly: true, Log: zerolog.Nop(),
		})
		if err != nil {
			return "", fmt.Errorf("failed to inspect the existing secret store: %w", err)
		}
		p, err := probe.View(ctx)
		if err != nil {
			return "", fmt.Errorf("an existing secret store was found but cannot be opened (%w); restore its key (`ready secrets import-key`) or move it away before init", err)
		}

		return p.AgentID, nil
	}
	id, err := uuid.NewV7()
	if err != nil {
		return "", fmt.Errorf("failed to generate the agent id: %w", err)
	}

	return id.String(), nil
}

// hostName proposes the agent display name.
func hostName() string {
	if h, err := os.Hostname(); err == nil && h != "" {
		return h
	}

	return "ready"
}
