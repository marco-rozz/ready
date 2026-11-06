// The Run orchestration of one `ready init` invocation: target selection,
// the TTY gate, the fresh/repair branch, publication, the release of the
// locks before any optional external work, and the operator-facing output
// contract (exit/committed outcomes of cli-bootstrap).
package initwizard

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/marco-rozz/ready/internal/config"
	"github.com/marco-rozz/ready/internal/secretstore"
)

// RunOutcome is everything cmd/init.go needs to decide its exit: the
// wizard distinguishes declined repairs (exit 0, nothing changed) from
// refused work (exit 1), and reports committed-but-late failures honestly.
type RunOutcome struct {
	// Committed configs and credential files survive a later failure: the
	// output still states the committed fact.
	Committed bool
	// DeclinedRepair: the operator declined the repair menu (exit 0).
	DeclinedRepair bool
	// Interrupted: a Ctrl+C/EOF mid-run (the caller maps appropriately).
	Interrupted bool
	// Resolution of the selected agent for the caller's messages.
	Resolution config.Resolution
}

// Run executes one init invocation end to end. Nothing here prints the
// process banner or initializes logging (cli-bootstrap: init SHALL NOT).
func Run(ctx context.Context, o Options) (outcome RunOutcome, runErr error) {
	defer func() {
		if errors.Is(runErr, context.Canceled) || errors.Is(runErr, ErrCancelled) || errors.Is(runErr, ErrEOF) {
			outcome.Interrupted = true
		}
	}()

	res, err := config.SelectAgentDirForInit(ctx, o.ExplicitAgentDir)
	if err != nil {
		return outcome, err
	}
	outcome.Resolution = res

	if o.NewIdentity {
		if _, statErr := os.Stat(res.Path); statErr != nil {
			return outcome, fmt.Errorf("--new-identity needs an existing configuration at %s", res.Path)
		}

		return outcome, runNewIdentity(ctx, o, res)
	}

	// cli-bootstrap: a missing interactive console fails before any fresh
	// publication (piped stdin cannot run a deterministic interview).
	if !o.StdinTTY {
		return outcome, fmt.Errorf("%w: refuse to drive the wizard from a piped/empty stdin (use a real console, or edit config.yaml by hand)", ErrNotTerminal)
	}

	randHex := o.RandomHex
	if randHex == nil {
		randHex = randomTokenHex
	}

	w := o.stdout()
	_, _ = fmt.Fprintf(w, "Agent directory: %s\nConfiguration file: %s\nThis directory keeps this agent's configuration, credentials, conversations and skills.\n", res.AgentDir, res.Path)

	if _, statErr := os.Stat(res.Path); statErr == nil {
		// Existing config — the bounded repair path.
		repaired, rerr := runOneRepair(ctx, o, res)
		outcome.Committed = repaired.Committed
		outcome.Interrupted = errors.Is(rerr, ErrCancelled) || errors.Is(rerr, ErrEOF) || errors.Is(rerr, context.Canceled)
		reportRepairOutcome(ctx, o, res, repaired, rerr)
		if rerr != nil {
			outcome.DeclinedRepair = errors.Is(rerr, ErrDeclinedRepair)

			return outcome, rerr
		}

		return outcome, nil
	}

	// Fresh publication: interview, locks, exclusive writes, validation,
	// config last. Interruptions mid-interview are conversion errors of
	// the interview itself; a post-publication interruption/error still
	// states the committed fact.
	choice, ierr := runInterview(ctx, o)
	if ierr != nil {
		if errors.Is(ierr, ErrCancelled) || errors.Is(ierr, ErrEOF) || errors.Is(ierr, context.Canceled) {
			outcome.Interrupted = true
		}

		return outcome, ierr
	}

	guideCheck := o.ReadyGuideCheck
	if guideCheck == nil {
		guideCheck = readyGuideInstalled
	}
	prepared, perr := prepareFresh(ctx, res, choice, randHex, guideCheck())
	outcome.Committed = prepared.ConfigCommitted
	if perr != nil {
		if errors.Is(perr, ErrCancelled) || errors.Is(perr, ErrEOF) {
			outcome.Interrupted = true
		}

		if outcome.Committed {
			fmt.Fprintln(w, "NOTE: the config was already published before the failure; it stays in place (no rollback was attempted).")
			printInitialCommands(ctx, o, res, prepared.Candidate)
		}

		return outcome, perr
	}

	for _, warn := range prepared.Warnings {
		_, _ = fmt.Fprintf(w, "warning: %s\n", warn)
	}
	_, _ = fmt.Fprintf(w, "\nConfiguration saved successfully. READY is not running yet.\n\nConfig file: %s\nProvider: %s\nModel: %s\nAgent: %s (%s)\nSecrets: stored encrypted in %s (the key is %s); config.yaml holds references only.\n", res.Path, choice.Provider, choice.ModelID, prepared.AgentName, prepared.AgentID, secretstore.StoreFileName, secretstore.KeyFileName)
	printSavedDefaults(ctx, o, res, prepared.Candidate)
	if prepared.SkillsSynced {
		_, _ = fmt.Fprintf(w, "Skills prepared in %s. The server will keep them synchronized with library updates, preserving your edits.\n", prepared.Candidate.Skills.Path)
	} else {
		_, _ = fmt.Fprintf(w, "Skills were not copied: the shared library is missing at %s.\nInstall or update READY; serve will synchronize the skills on its next start.\n", prepared.Candidate.Skills.LibraryPath)
	}

	// The mutation locks are released inside prepareFresh; any optional
	// external work (login, probe) runs unowned.
	o.runVerification(ctx, appPromptReader(o.StdinTTY, o.Stdin, o.stdout(), o.ReaderOverride), res, choice, prepared.Candidate)
	printInitialCommands(ctx, o, res, prepared.Candidate)

	return outcome, nil
}

// runOneRepair isolates the interactive repair flow (streams bound, then
// locks — the repair path refuses online ownership/maintenance conflicts by
// the launchLocks guard).
func runOneRepair(ctx context.Context, o Options, res config.Resolution) (repairRunsByOperator, error) {
	ui := repairUI{interview: interview{
		in:   o.Stdin,
		out:  o.stdout(),
		read: appPromptReader(o.StdinTTY, o.Stdin, o.stdout(), o.ReaderOverride),
	}, out: o.stdout()}

	randHex := o.RandomHex
	if randHex == nil {
		randHex = randomTokenHex
	}

	choiceSource := func(_ io.Writer) (ProviderChoice, error) { return runInterview(ctx, o) }

	lks, lerr := launchLocks(ctx, res.AgentDir)
	if lerr != nil {
		return repairRunsByOperator{}, lerr
	}

	// recheck the target under the locks: the config must still be present
	// and the ownership must be ours — the launch sequence is the guard.
	if _, statErr := os.Stat(res.Path); statErr != nil {
		if relErr := lks.release(); relErr != nil {
			return repairRunsByOperator{}, errors.Join(
				fmt.Errorf("%s: %w", res.Path, statErr), relErr,
			)
		}

		return repairRunsByOperator{}, fmt.Errorf("%s: %w", res.Path, statErr)
	}

	result, err := runRepair(ctx, res, ui, choiceSource, randHex)
	if rerr := lks.release(); rerr != nil {
		err = errors.Join(err, rerr)
	}
	if result.Committed {
		for _, d := range result.Done {
			fmt.Fprintf(o.stdout(), "repaired: %s\n", d)
		}
	}

	return result, err
}

// readyGuideInstalled checks the shared shipped library for the
// ready-guide skill content before init advertises the guided admin policy
// (absent content yields install guidance instead of a broken prompt).
func readyGuideInstalled() bool {
	lib := config.SharedLibraryDir()
	if lib == "" {
		return false
	}

	_, err := os.Stat(filepath.Join(lib, "ready-guide", "SKILL.md"))

	return err == nil
}

// reportRepairOutcome preserves the replacement fact even when a later
// durability or lock-release error occurs. Commands use the committed view.
func reportRepairOutcome(ctx context.Context, o Options, res config.Resolution, repaired repairRunsByOperator, err error) {
	out := o.stdout()
	for _, warning := range repaired.Warnings {
		_, _ = fmt.Fprintf(out, "warning: %s\n", warning)
	}
	if !repaired.Committed {
		return
	}
	_, _ = fmt.Fprintf(out, "Config repair committed: %s\n", res.Path)
	if err != nil {
		_, _ = fmt.Fprintln(out, "NOTE: the repair was already committed before the failure; the config and its credentials stay in place. Do not repeat the write.")
		printInitialCommands(ctx, o, res, repaired.Candidate)

		return
	}
	_, _ = fmt.Fprintln(out, "\nChanges saved. Start the server again to use them; the chat command below reads this agent's chat secret by itself.")
	printInitialCommands(ctx, o, res, repaired.Candidate)
}
