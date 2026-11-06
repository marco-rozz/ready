package main

// `ready secrets` — the universal, offline way to manage the agent's Secret
// store without the TUI: list/set/rm/reveal for Secrets, adopt for literals a
// hand edit left in config.yaml, and export/import/move/forget for the Store
// key. It runs alongside a live daemon (shared maintenance gate, store lock),
// owns no daemon lock, initialises no logging, and is the only command that
// ever outputs a secret value (reveal, to a file or a terminal).

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/rs/zerolog"
	"github.com/urfave/cli/v3"
	"golang.org/x/term"

	"github.com/marco-rozz/ready/internal/config"
	"github.com/marco-rozz/ready/internal/daemonlock"
	"github.com/marco-rozz/ready/internal/secretstore"
	"github.com/marco-rozz/ready/internal/tools"
)

const secretsLabel = "secrets"

// argsName is the ArgsUsage of the subcommands that take one Secret name.
const argsName = "<name>"

// secretsIO is the command's terminal surface, injectable for tests.
type secretsIO struct {
	stdin     io.Reader
	stdout    io.Writer
	stderr    io.Writer
	stdinTTY  bool
	stdoutTTY bool
	// readPass reads a hidden line (nil → the real terminal reader).
	readPass func(prompt string) (string, error)
}

func realSecretsIO(stderr io.Writer) secretsIO {
	return secretsIO{
		stdin: os.Stdin, stdout: os.Stdout, stderr: stderr,
		stdinTTY:  term.IsTerminal(int(os.Stdin.Fd())),
		stdoutTTY: term.IsTerminal(int(os.Stdout.Fd())),
	}
}

func (s secretsIO) hidden(prompt string) (string, error) {
	if s.readPass != nil {
		return s.readPass(prompt)
	}
	if !s.stdinTTY {
		return "", errors.New("no terminal for hidden input; pipe the value with --stdin")
	}
	_, _ = fmt.Fprint(s.stderr, prompt)
	b, err := term.ReadPassword(int(os.Stdin.Fd()))
	_, _ = fmt.Fprintln(s.stderr)
	if err != nil {
		return "", fmt.Errorf("failed to read the hidden input: %w", err)
	}

	return string(b), nil
}

func newSecretsCommand(stderr io.Writer) *cli.Command {
	return newSecretsCommandIO(realSecretsIO(stderr))
}

// newSecretsCommandIO builds the command over an injectable terminal surface.
func newSecretsCommandIO(sio secretsIO) *cli.Command {
	agentDir := &cli.StringFlag{Name: flagAgentDirName, Usage: "agent directory (default: ~/.ready)"}
	run := func(fn func(context.Context, *cli.Command, secretsIO) error) cli.ActionFunc {
		return func(ctx context.Context, c *cli.Command) error {
			return runSecretsAction(ctx, c, sio, fn)
		}
	}

	return &cli.Command{
		Name:  secretsLabel,
		Usage: "Manage the agent's encrypted Secret store and its Store key (offline)",
		Commands: []*cli.Command{
			{Name: "list", Usage: "list Secrets, their state and Credentials (never values)", Flags: []cli.Flag{agentDir}, Action: run(secretsList)},
			{Name: "set", Usage: "set a Secret (hidden prompt, or --stdin)", ArgsUsage: argsName, Flags: []cli.Flag{agentDir, &cli.BoolFlag{Name: "stdin", Usage: "read the value from stdin (one trailing newline is removed)"}}, Action: run(secretsSet)},
			{Name: "rm", Usage: "remove an unused Secret", ArgsUsage: argsName, Flags: []cli.Flag{agentDir, &cli.BoolFlag{Name: "force", Usage: "remove even if the configuration still references it"}}, Action: run(secretsRemove)},
			{Name: "reveal", Usage: "write a Secret to a new private file (or a terminal)", ArgsUsage: argsName, Flags: []cli.Flag{agentDir, &cli.StringFlag{Name: "to-file", Usage: "create this file exclusively (0600)"}}, Action: run(secretsReveal)},
			{Name: "adopt", Usage: "move literals left in config.yaml into the Secret store", Flags: []cli.Flag{agentDir}, Action: run(secretsAdopt)},
			{Name: "export-key", Usage: "export the Store key, passphrase-protected", Flags: []cli.Flag{agentDir, &cli.StringFlag{Name: "to", Usage: "write to this new file instead of stdout"}, &cli.BoolFlag{Name: "raw", Usage: "write the bare key to a non-terminal stdout (for systemd-creds)"}}, Action: run(secretsExportKey)},
			{Name: "import-key", Usage: "install a Store key (file or stdin) after verifying it decrypts the store", ArgsUsage: "[file]", Flags: []cli.Flag{agentDir}, Action: run(secretsImportKey)},
			{Name: "move-key", Usage: "move the Store key to agent_dir, os_vault or a file", Flags: []cli.Flag{agentDir, &cli.StringFlag{Name: "to", Usage: "agent_dir | os_vault | file", Required: true}, &cli.StringFlag{Name: "path", Usage: "absolute key file path (with --to file)"}}, Action: run(secretsMoveKey)},
			{Name: "forget-key", Usage: "delete the vault entry that holds this agent's Store key", Flags: []cli.Flag{agentDir, &cli.BoolFlag{Name: "yes", Usage: "do not ask for confirmation"}}, Action: run(secretsForgetKey)},
		},
	}
}

// secretsEnv is the loaded agent a subcommand works on.
type secretsEnv struct {
	cfg   *config.Config
	store *secretstore.Store
	io    secretsIO
}

func fail(w io.Writer, err error) error {
	_, _ = fmt.Fprintln(w, err)

	return cli.Exit("", ExitErr)
}

// runSecretsAction wraps one subcommand with the shared preamble: agent
// selection, the shared maintenance gate (so an installer purge excludes it),
// and the exit-code contract 0/1/124/130.
func runSecretsAction(ctx context.Context, c *cli.Command, sio secretsIO, fn func(context.Context, *cli.Command, secretsIO) error) error {
	err := fn(ctx, c, sio)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, context.Canceled):
		return cli.Exit("", ExitSIGINT)
	case errors.Is(err, context.DeadlineExceeded):
		return cli.Exit("", ExitTimeout)
	}
	var exit cli.ExitCoder
	if errors.As(err, &exit) {
		return err
	}

	return fail(sio.stderr, err)
}

// openSecretsEnv loads the agent read-only or read-write.
func openSecretsEnv(ctx context.Context, c *cli.Command, sio secretsIO, readOnly bool) (*secretsEnv, func(), error) {
	res, err := config.SelectAgentDir(ctx, c.String(flagAgentDirName))
	if err != nil {
		return nil, nil, err
	}
	cfg, err := config.Load(ctx, res.Path)
	if err != nil {
		return nil, nil, err
	}

	return finishSecretsEnv(ctx, cfg, res, sio, readOnly)
}

func finishSecretsEnv(ctx context.Context, cfg *config.Config, res config.Resolution, sio secretsIO, readOnly bool) (*secretsEnv, func(), error) {
	gate, err := daemonlock.AcquireMaintenanceShared(ctx, res.AgentDir)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to acquire the maintenance gate on %s: %w", res.AgentDir, err)
	}
	release := func() { _ = gate.Release() }
	store, err := cfg.OpenSecretStore(zerolog.Nop(), readOnly)
	if err != nil {
		release()

		return nil, release, err
	}
	cfg.SecretValues = config.NewSecretSource(store)

	return &secretsEnv{cfg: cfg, store: store, io: sio}, release, nil
}

func (e *secretsEnv) payload(ctx context.Context) (*secretstore.Payload, error) {
	p, err := e.store.View(ctx)
	if errors.Is(err, secretstore.ErrNoStore) {
		return secretstore.NewPayload(e.cfg.Agent.ID), nil
	}
	if err != nil {
		return nil, fmt.Errorf("cannot open the secret store: %w", err)
	}

	return p, nil
}

func (e *secretsEnv) report(ctx context.Context) (secretstore.Report, *secretstore.Payload, error) {
	p, err := e.payload(ctx)
	if err != nil {
		return secretstore.Report{}, nil, err
	}

	return secretstore.Derive(p, e.cfg.SecretStoreReferences(), nil), p, nil
}

func (e *secretsEnv) fieldsOf(rep secretstore.Report, name string) []string {
	for _, s := range rep.Secrets {
		if s.Name == name {
			return s.Fields
		}
	}

	return nil
}

func secretsList(ctx context.Context, c *cli.Command, sio secretsIO) error {
	env, release, err := openSecretsEnv(ctx, c, sio, true)
	if err != nil {
		return err
	}
	defer release()
	rep, p, err := env.report(ctx)
	if err != nil {
		return err
	}
	out := sio.stdout
	_, _ = fmt.Fprintf(out, "agent: %s (%s)\n\n", env.cfg.AgentDisplayName(), env.cfg.Agent.ID)
	_, _ = fmt.Fprintf(out, "%-32s %-10s %s\n", "SECRET", "STATE", "USED BY")
	for _, s := range rep.Secrets {
		state := string(s.State)
		if s.Rule != "" {
			state += " (" + s.Rule + ")"
		}
		_, _ = fmt.Fprintf(out, "%-32s %-10s %s\n", s.Name, state, strings.Join(s.Fields, ", "))
	}
	for _, e := range rep.External {
		_, _ = fmt.Fprintf(out, "%-32s %-10s %s\n", e.Path, e.State, e.Field)
	}
	if len(p.Credentials) > 0 {
		targets := make([]string, 0, len(p.Credentials))
		for t := range p.Credentials {
			targets = append(targets, t)
		}
		sort.Strings(targets)
		_, _ = fmt.Fprintln(out, "\nCREDENTIALS (managed by `ready login`)")
		for _, t := range targets {
			_, _ = fmt.Fprintf(out, "%-32s stored\n", t)
		}
	}

	return nil
}

func secretsSet(ctx context.Context, c *cli.Command, sio secretsIO) error {
	name := c.Args().First()
	if !config.ValidSecretName(name) {
		return errors.New("a secret name is 1-64 characters of [a-z0-9_] starting with [a-z0-9]")
	}
	env, release, err := openSecretsEnv(ctx, c, sio, false)
	if err != nil {
		return err
	}
	defer release()

	var value string
	if c.Bool("stdin") {
		raw, rerr := io.ReadAll(bufio.NewReader(sio.stdin))
		if rerr != nil {
			return fmt.Errorf("failed to read stdin: %w", rerr)
		}
		value = secretstore.NormalizeFileSecret(raw)
	} else {
		first, herr := sio.hidden("Value for " + name + " (input hidden): ")
		if herr != nil {
			return herr
		}
		again, herr := sio.hidden("Repeat the value: ")
		if herr != nil {
			return herr
		}
		if first != again {
			return errors.New("the two entries differ; nothing changed")
		}
		value = first
	}
	if value != strings.TrimSpace(value) {
		_, _ = fmt.Fprintln(sio.stderr, "warning: the value has leading or trailing whitespace; it is stored exactly as entered")
	}
	rep, _, err := env.report(ctx)
	if err != nil {
		return err
	}
	if vErr := secretstore.CheckValue(env.fieldsOf(rep, name), value); vErr != nil {
		return fmt.Errorf("the value was refused: %w", vErr)
	}
	if err = env.store.Update(ctx, func(p *secretstore.Payload) error {
		p.Secrets[name] = secretstore.Secret{Value: value, UpdatedAt: nowUTC()}

		return nil
	}); err != nil {
		return fmt.Errorf("failed to write the secret: %w", err)
	}
	_, _ = fmt.Fprintf(sio.stdout, "secret %s stored (restart READY to activate components that were pending; the chat secret is adopted by itself)\n", name)

	return nil
}

func secretsRemove(ctx context.Context, c *cli.Command, sio secretsIO) error {
	name := c.Args().First()
	env, release, err := openSecretsEnv(ctx, c, sio, false)
	if err != nil {
		return err
	}
	defer release()
	rep, p, err := env.report(ctx)
	if err != nil {
		return err
	}
	if _, ok := p.Secrets[name]; !ok {
		return fmt.Errorf("no secret named %q is stored", name)
	}
	if fields := env.fieldsOf(rep, name); len(fields) > 0 && !c.Bool("force") {
		return fmt.Errorf("%q is still referenced by %s; edit the configuration first, or pass --force", name, strings.Join(fields, ", "))
	}
	if err = env.store.Update(ctx, func(p *secretstore.Payload) error {
		delete(p.Secrets, name)

		return nil
	}); err != nil {
		return fmt.Errorf("failed to remove the secret: %w", err)
	}
	_, _ = fmt.Fprintf(sio.stdout, "secret %s removed\n", name)

	return nil
}

func secretsReveal(ctx context.Context, c *cli.Command, sio secretsIO) error {
	name, target := c.Args().First(), c.String("to-file")
	if target == "" && !sio.stdoutTTY {
		return errors.New("reveal prints only to a terminal; use --to-file <path> to write a private file")
	}
	env, release, err := openSecretsEnv(ctx, c, sio, true)
	if err != nil {
		return err
	}
	defer release()
	p, err := env.payload(ctx)
	if err != nil {
		return err
	}
	sec, ok := p.Secrets[name]
	if !ok {
		return fmt.Errorf("no secret named %q is stored", name)
	}
	if target != "" {
		if err = secretstore.WriteNewPrivateFile(target, []byte(sec.Value)); err != nil {
			return err
		}
		_, _ = fmt.Fprintf(sio.stdout, "secret %s written to %s (mode 0600); delete it when you no longer need it\n", name, target)

		return nil
	}
	_, _ = fmt.Fprintln(sio.stdout, sec.Value)

	return nil
}

func secretsAdopt(ctx context.Context, c *cli.Command, sio secretsIO) error {
	res, err := config.SelectAgentDir(ctx, c.String(flagAgentDirName))
	if err != nil {
		return err
	}
	state, err := config.ReadConfigSource(ctx, res.Path)
	if err != nil {
		return err
	}
	if len(config.LiteralSecretPaths(state.Data)) == 0 {
		_, _ = fmt.Fprintln(sio.stdout, "nothing to adopt: config.yaml holds no literal credential")

		return nil
	}
	// The literals fail strict loading by design, so plan on the raw bytes
	// and decode the rewritten document for the agent identity.
	taken := map[string]bool{}
	next, items, err := tools.AdoptLiterals(state.Data, func(path string) string {
		base, _ := config.CanonicalSecretName(strings.Split(path, "."))
		if base == "" {
			base = "adopted_secret"
		}
		name := config.UniqueSecretName(base, func(n string) bool { return taken[n] })
		taken[name] = true

		return name
	})
	if err != nil {
		return err
	}
	cfg, err := config.DecodeForRepair(ctx, next, res.Path)
	if err != nil {
		return fmt.Errorf("the document cannot be adopted automatically: %w", err)
	}
	if err = cfg.ValidateForRepair(); err != nil {
		return fmt.Errorf("the document is not valid once adopted: %w", err)
	}
	env, release, err := finishSecretsEnv(ctx, cfg, res, sio, false)
	if err != nil {
		return err
	}
	defer release()
	existing, err := env.payload(ctx)
	if err != nil {
		return err
	}
	for _, it := range items {
		if vErr := secretstore.CheckValue([]string{it.Path}, it.Value); vErr != nil {
			return fmt.Errorf("%s: the value was refused: %w", it.Path, vErr)
		}
		if cur, ok := existing.Secrets[it.Name]; ok && cur.Value != it.Value {
			return fmt.Errorf("a different secret named %q already exists; adopt stopped before changing anything", it.Name)
		}
	}
	if err = env.store.Update(ctx, func(p *secretstore.Payload) error {
		for _, it := range items {
			p.Secrets[it.Name] = secretstore.Secret{Value: it.Value, UpdatedAt: nowUTC()}
		}

		return nil
	}); err != nil {
		return fmt.Errorf("failed to write the secrets: %w", err)
	}
	if _, err = config.ValidateCandidate(ctx, next, res.Path, config.ProfileOverrideInputs{}, nil); err != nil {
		return fmt.Errorf("the adopted configuration was rejected (the secrets stay stored, unused): %w", err)
	}
	report, err := config.CommitCandidateEncrypted(ctx, state, next, func(b []byte) ([]byte, error) { return env.store.Seal(ctx, b) })
	if err != nil && !report.Committed {
		return fmt.Errorf("the configuration was not changed (the secrets stay stored, unused): %w", err)
	}
	for _, it := range items {
		_, _ = fmt.Fprintf(sio.stdout, "%s -> !secret %s\n", it.Path, it.Name)
	}
	_, _ = fmt.Fprintf(sio.stdout, "adopted %d literal(s); restart READY to apply the configuration\n", len(items))

	return nil
}

func secretsExportKey(ctx context.Context, c *cli.Command, sio secretsIO) error {
	env, release, err := openSecretsEnv(ctx, c, sio, true)
	if err != nil {
		return err
	}
	defer release()
	id, err := env.store.Identity(ctx)
	if err != nil {
		return fmt.Errorf("cannot read the store key: %w", err)
	}
	var data []byte
	if c.Bool("raw") {
		if sio.stdoutTTY || c.String("to") != "" {
			return errors.New("--raw writes the bare key to a non-terminal stdout only")
		}
		_, _ = fmt.Fprintln(sio.stderr, "warning: writing the UNPROTECTED store key to stdout; protect it right away (e.g. `systemd-creds encrypt`)")
		data = []byte(id.String() + "\n")
	} else {
		first, herr := sio.hidden("Passphrase for the exported key (input hidden): ")
		if herr != nil {
			return herr
		}
		again, herr := sio.hidden("Repeat the passphrase: ")
		if herr != nil {
			return herr
		}
		if first == "" || first != again {
			return errors.New("the passphrases are empty or differ; nothing exported")
		}
		if data, err = secretstore.WrapIdentity(id, first); err != nil {
			return err
		}
	}
	if to := c.String("to"); to != "" {
		if err = secretstore.WriteNewPrivateFile(to, data); err != nil {
			return err
		}
		_, _ = fmt.Fprintf(sio.stderr, "store key exported to %s\n", to)

		return nil
	}
	_, err = sio.stdout.Write(data)

	return err
}

func secretsImportKey(ctx context.Context, c *cli.Command, sio secretsIO) error {
	var raw []byte
	var err error
	if f := c.Args().First(); f != "" {
		raw, err = os.ReadFile(f)
	} else {
		raw, err = io.ReadAll(sio.stdin)
	}
	if err != nil {
		return fmt.Errorf("failed to read the key: %w", err)
	}
	env, release, err := openSecretsEnv(ctx, c, sio, false)
	if err != nil {
		return err
	}
	defer release()
	key, perr := parseImportedKey(raw, sio)
	if perr != nil {
		return perr
	}
	if err = env.store.InstallKey(ctx, key); err != nil {
		if errors.Is(err, secretstore.ErrKeyExists) {
			return errors.New("a store key is already installed at the configured location; `ready secrets forget-key` or move it first")
		}

		return err
	}
	_, _ = fmt.Fprintln(sio.stdout, "store key installed and verified against the existing store")

	return nil
}
