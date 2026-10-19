package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/marco-rozz/ready/internal/codexauth"
	"github.com/marco-rozz/ready/internal/config"
	homemcp "github.com/marco-rozz/ready/internal/mcp"
	"github.com/marco-rozz/ready/internal/oauth"
	"github.com/marco-rozz/ready/internal/profile"
	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/oauthex"
	"github.com/rs/zerolog"
	"github.com/urfave/cli/v3"
	"golang.org/x/oauth2"
)

const loginLabel = "login"

// errAuthorizationFailed classifies a login failure as an authorization
// problem (the interactive fetcher failed/was denied, or the server rejected
// the credentials with an authentication error) rather than a connection or
// configuration problem. mapLoginErr maps it to ExitAuthFail.
var errAuthorizationFailed = errors.New("oauth: authorization failed")

// tokenEndpointAuthMethodNone is the RFC 7591 token_endpoint_auth_method
// value for a public, PKCE-only client — every client this package resolves
// without an explicit confidential-client secret uses it. Named separately
// from statusActionNone (same text, unrelated meaning: UI status vs. wire
// protocol value) so the two don't collapse into one accidental constant.
const tokenEndpointAuthMethodNone = "none"

// statusActionNone is the --status table's "no action required" / "no
// stored credential" placeholder text.
const statusActionNone = "none"

// grantAuto is the config.MCPOAuthConfig.Grant value that defers resolution
// to the design doc (oauth.ResolveGrant / resolveDaemonGrant) instead of naming
// a specific grant. Shared between login.go and serve.go so the literal
// exists exactly once in this package.
const grantAuto = "auto"

// newLoginCommand declares the `login` subcommand. It is the only place in
// the binary a browser is ever opened (the design doc in
// the mcp-oauth-authorization change design); `ready serve` injects
// a non-interactive fetcher instead. login MUST NOT initialise the logging
// subsystem and the only permitted write outside config files is through the
// credential store built from oauth.path — see runLogin.
func newLoginCommand(stderr io.Writer) *cli.Command {
	return &cli.Command{
		Name:      loginLabel,
		Usage:     "Authenticate an OAuth-capable MCP server or LLM provider (the only place a browser is opened)",
		ArgsUsage: "[mcp:<server>|llm:<provider>|name]",
		Flags: []cli.Flag{
			&cli.StringFlag{
				// Optional: without it login elects the config exactly like
				// serve (flag > ./.ready/config.yaml > ~/.ready/config.yaml).
				Name:  "config",
				Usage: "path to the YAML configuration file (elected through the resolution chain when omitted)",
			},
			&cli.BoolFlag{
				Name:  "status",
				Usage: "print stored credential status without authenticating; writes nothing",
			},
			&cli.BoolFlag{
				Name:  "device-code",
				Usage: "use the headless ChatGPT device authorization flow (openai-codex only)",
			},
			&cli.BoolFlag{
				Name:  "forget",
				Usage: "delete a stored credential (deregistering it first when possible); requires a server argument",
			},
		},
		Action: func(ctx context.Context, c *cli.Command) error {
			return runLogin(ctx, c, os.Stdout, stderr)
		},
	}
}

// runLogin is the testable body of the login Action, mirroring runChat's
// separation from newChatCommand.
func runLogin(ctx context.Context, c *cli.Command, stdout, stderr io.Writer) error {
	return runLoginWithIO(ctx, c, os.Stdin, stdout, stderr)
}

type oauthTargetKind string

const (
	targetMCP oauthTargetKind = "mcp"
	targetLLM oauthTargetKind = "llm"
)

type oauthTarget struct {
	Kind oauthTargetKind
	Name string
}

func (t oauthTarget) String() string { return string(t.Kind) + ":" + t.Name }

// validateLoginModes rejects combinations of --status, --forget and
// --device-code that cannot run together (and a --forget without a target).
func validateLoginModes(status, forget, deviceCode bool, requested string) error {
	switch {
	case status && forget:
		return errors.New("--status and --forget are mutually exclusive")
	case forget && requested == "":
		return errors.New("--forget requires a server argument or LLM provider target")
	case deviceCode && (status || forget):
		return errors.New("--device-code cannot be combined with --status or --forget")
	default:
		return nil
	}
}

// electAndLoadConfig resolves the config chain (matching serve's election
// when --config is absent), loads the file and validates it so login's
// target enumeration, grants and secrets come from the config the daemon
// runs. "using config <path>" is the FIRST output line: the login↔daemon
// config mismatch is visible at the announcement, not at the next ping.
func electAndLoadConfig(ctx context.Context, configFlag string, stdout io.Writer) (*config.Config, error) {
	resolution, err := config.Elect(ctx, configFlag)
	if err != nil {
		return nil, err
	}

	_, _ = fmt.Fprintf(stdout, "using config %s\n", resolution.Path)

	cfg, err := config.Load(ctx, resolution.Path)
	if err != nil {
		return nil, err
	}
	cfg.Origin = resolution.Origin

	if verr := cfg.Validate(); verr != nil {
		return nil, verr
	}

	return cfg, nil
}

// dispatchLoginMode routes the resolved targets to the selected mode:
// status table, forget, or the interactive login flows.
func dispatchLoginMode(
	ctx context.Context,
	cfg *config.Config,
	store oauth.Store,
	targets []oauthTarget,
	status, forget, deviceCode bool,
	stdin io.Reader,
	stdout, stderr io.Writer,
) error {
	switch {
	case status:
		printOAuthStatus(ctx, store, cfg, targets, stdout)

		return nil
	case forget:
		return runForgetTarget(ctx, store, cfg, targets[0], stdout, stderr)
	default:
		if deviceCode && (len(targets) != 1 || targets[0].Kind != targetLLM) {
			_, _ = fmt.Fprintln(stderr, "--device-code is supported only for an openai-codex LLM target")

			return cli.Exit("", ExitErr)
		}

		return runOAuthTargets(ctx, cfg, store, targets, deviceCode, stdin, stdout, stderr)
	}
}

func runLoginWithIO(ctx context.Context, c *cli.Command, stdin io.Reader, stdout, stderr io.Writer) error {
	return runLoginWithConfig(ctx, c, stdin, stdout, stderr, electAndLoadConfig)
}

// runLoginWithConfig permits testing version failures on every mode before any
// target lookup or credential/network side effect, without a mutable global.
func runLoginWithConfig(ctx context.Context, c *cli.Command, stdin io.Reader, stdout, stderr io.Writer, load func(context.Context, string, io.Writer) (*config.Config, error)) error {
	status := c.Bool("status")
	forget := c.Bool("forget")
	deviceCode := c.Bool("device-code")
	requested := c.Args().First()

	if err := validateLoginModes(status, forget, deviceCode, requested); err != nil {
		_, _ = fmt.Fprintln(stderr, err)

		return cli.Exit("", ExitErr)
	}

	cfg, err := load(ctx, c.String("config"), stdout)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "%v\n", err)

		return cli.Exit("", ExitErr)
	}

	// Target enumeration comes before store construction: with no
	// OAuth-capable target there is no store to build, and the target error
	// wins over a store-path error.
	targets, err := resolveOAuthTargets(cfg, requested)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "%v\n", err)

		return cli.Exit("", ExitErr)
	}

	store, err := oauth.NewStore(ctx, cfg.OAuth)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "%v\n", err)

		return cli.Exit("", ExitErr)
	}

	return dispatchLoginMode(ctx, cfg, store, targets, status, forget, deviceCode, stdin, stdout, stderr)
}

// resolveOAuthTargets applies the typed, backward-compatible target rules,
// dispatching to one helper per resolution shape (qualified "kind:name",
// unqualified bare name, or full enumeration) so each stays flat.
func resolveOAuthTargets(cfg *config.Config, requested string) ([]oauthTarget, error) {
	if requested == "" {
		return enumerateOAuthTargets(cfg)
	}
	if prefix, name, qualified := strings.Cut(requested, ":"); qualified {
		return resolveQualifiedOAuthTarget(cfg, oauthTargetKind(prefix), name, requested)
	}

	return resolveUnqualifiedOAuthTarget(cfg, requested)
}

// resolveQualifiedOAuthTarget resolves an explicit "mcp:<name>" / "llm:<name>"
// target, validating the namespace and the named entry against the elected
// config.
func resolveQualifiedOAuthTarget(cfg *config.Config, kind oauthTargetKind, name, requested string) ([]oauthTarget, error) {
	if name == "" {
		return nil, fmt.Errorf("OAuth target %q has an empty name", requested)
	}
	switch kind {
	case targetMCP:
		srv, ok := cfg.MCPServers[name]
		if !ok || srv.OAuth == nil {
			return nil, fmt.Errorf("unknown or non-OAuth MCP target %q (not found in the elected config %s)", name, cfg.ElectedPath)
		}
	case targetLLM:
		provider, ok := cfg.LLM.Providers[name]
		if !ok || provider.Provider != config.ProviderOpenAICodex {
			return nil, fmt.Errorf("unknown or non-OAuth LLM target %q (not found in the elected config %s)", name, cfg.ElectedPath)
		}
	default:
		return nil, fmt.Errorf("unknown OAuth target namespace %q", kind)
	}

	return []oauthTarget{{Kind: kind, Name: name}}, nil
}

// resolveUnqualifiedOAuthTarget resolves a bare name against both namespaces,
// accepting exactly one match and turning zero or two matches into typed
// errors.
func resolveUnqualifiedOAuthTarget(cfg *config.Config, requested string) ([]oauthTarget, error) {
	var matches []oauthTarget
	srv, mcpExists := cfg.MCPServers[requested]
	if mcpExists && srv.OAuth != nil {
		matches = append(matches, oauthTarget{Kind: targetMCP, Name: requested})
	}
	if provider, ok := cfg.LLM.Providers[requested]; ok && provider.Provider == config.ProviderOpenAICodex {
		matches = append(matches, oauthTarget{Kind: targetLLM, Name: requested})
	}

	switch len(matches) {
	case 1:
		return matches, nil
	case 0:
		if mcpExists {
			return nil, fmt.Errorf("mcp_servers.%s has no oauth block configured (elected config: %s)", requested, cfg.ElectedPath)
		}

		// Naming the elected config path makes a wrong-chain election
		// (login from a different directory than the daemon) diagnosable
		// at enumeration instead of surfacing as "login required" later.
		return nil, fmt.Errorf("unknown mcp server or OAuth-capable LLM provider %q (not found in the elected config %s)", requested, cfg.ElectedPath)
	default:
		return nil, fmt.Errorf("OAuth target %q is ambiguous; use mcp:%s or llm:%s", requested, requested, requested)
	}
}

// enumerateOAuthTargets collects every OAuth-capable MCP server and
// openai-codex LLM provider declared by the elected config, in stable
// "kind:name" order.
func enumerateOAuthTargets(cfg *config.Config) ([]oauthTarget, error) {
	var targets []oauthTarget
	for name, srv := range cfg.MCPServers {
		if srv.OAuth != nil {
			targets = append(targets, oauthTarget{Kind: targetMCP, Name: name})
		}
	}
	for name, provider := range cfg.LLM.Providers {
		if provider.Provider == config.ProviderOpenAICodex {
			targets = append(targets, oauthTarget{Kind: targetLLM, Name: name})
		}
	}
	sort.Slice(targets, func(i, j int) bool { return targets[i].String() < targets[j].String() })
	if len(targets) == 0 {
		return nil, errors.New("no OAuth-capable MCP servers or LLM providers are configured")
	}

	return targets, nil
}

func printOAuthStatus(ctx context.Context, mcpStore oauth.Store, cfg *config.Config, targets []oauthTarget, stdout io.Writer) {
	printMCPStatusEntries(ctx, mcpStore, cfg, targets, stdout)
	printCodexStatusEntries(ctx, cfg, targets, stdout)
}

// printMCPStatusEntries renders the MCP-server section of the status table
// when at least one MCP target is present.
func printMCPStatusEntries(ctx context.Context, mcpStore oauth.Store, cfg *config.Config, targets []oauthTarget, stdout io.Writer) {
	var mcpTargets []string
	for _, target := range targets {
		if target.Kind == targetMCP {
			mcpTargets = append(mcpTargets, target.Name)
		}
	}
	if len(mcpTargets) > 0 {
		printLoginStatus(ctx, mcpStore, cfg, mcpTargets, stdout)
	}
}

// printCodexStatusEntries renders one status line per openai-codex LLM
// target, reusing the credential store across rows.
func printCodexStatusEntries(ctx context.Context, cfg *config.Config, targets []oauthTarget, stdout io.Writer) {
	var codexStore codexauth.Store
	for _, target := range targets {
		if target.Kind != targetLLM {
			continue
		}
		codexStore = printCodexStatusEntry(ctx, cfg, target.Name, codexStore, stdout)
	}
}

// printCodexStatusEntry lazily builds the codex credential store on the
// first row that needs it (returning nil again on failure, so the next row
// retries creation) and prints that row's status line.
func printCodexStatusEntry(ctx context.Context, cfg *config.Config, name string, codexStore codexauth.Store, stdout io.Writer) codexauth.Store {
	if codexStore == nil {
		store, err := codexauth.NewFileStore(ctx, cfg.OAuth.Path)
		if err != nil {
			_, _ = fmt.Fprintf(stdout, "llm:%s  credential error  action: ready login llm:%s\n", name, name)

			return nil
		}
		codexStore = store
	}

	cred, _ := codexStore.Load(ctx, name)
	present, expiry, account, action := codexStatusColumns(name, cred, time.Now())
	_, _ = fmt.Fprintf(stdout, "llm:%-20s openai-codex credential=%s expiry=%s account=%s action=%s\n", name, present, expiry, account, action)

	return codexStore
}

// codexStatusColumns derives the four status columns for a codex
// credential row: "none" with the relogin action when no credential is
// stored, otherwise presence, account id, RFC3339 expiry and the action
// the Usable predicate deserves.
func codexStatusColumns(name string, cred *codexauth.Credential, now time.Time) (present, expiry, account, action string) {
	present, expiry, account = "none", "-", "-"
	action = fmt.Sprintf("run `ready login llm:%s`", name)
	if cred == nil {
		return present, expiry, account, action
	}

	present, account = "present", cred.PublicAccountID()
	if !cred.Expiry.IsZero() {
		expiry = cred.Expiry.Format(time.RFC3339)
	}
	if cred.Usable(name, now, 30*time.Second) {
		action = statusActionNone
	}

	return present, expiry, account, action
}

func runForgetTarget(ctx context.Context, mcpStore oauth.Store, cfg *config.Config, target oauthTarget, stdout, stderr io.Writer) error {
	if target.Kind == targetMCP {
		return runForget(ctx, mcpStore, cfg, target.Name, stdout, stderr)
	}
	store, err := codexauth.NewFileStore(ctx, cfg.OAuth.Path)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)

		return cli.Exit("", ExitErr)
	}
	cred, err := store.Load(ctx, target.Name)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)

		return cli.Exit("", ExitErr)
	}
	if cred == nil {
		_, _ = fmt.Fprintf(stdout, "no stored credential for %q\n", target.String())

		return nil
	}
	if err := store.Delete(ctx, target.Name); err != nil {
		_, _ = fmt.Fprintln(stderr, err)

		return cli.Exit("", ExitErr)
	}
	_, _ = fmt.Fprintf(stdout, "removed stored credential for %q\n", target.String())

	return nil
}

func runOAuthTargets(ctx context.Context, cfg *config.Config, mcpStore oauth.Store, targets []oauthTarget, deviceCode bool, stdin io.Reader, stdout, stderr io.Writer) error {
	var firstErr error
	for _, target := range targets {
		err := runSingleOAuthTarget(ctx, cfg, mcpStore, target, deviceCode, stdin, stdout)
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "%s: %v\n", target.String(), err)
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	if firstErr != nil {
		return mapLoginErr(ctx, firstErr)
	}

	return nil
}

// runSingleOAuthTarget dispatches one target to its login flow: MCP
// servers through discovery/registration/authorization, openai-codex LLM
// providers through the ChatGPT flows.
func runSingleOAuthTarget(
	ctx context.Context,
	cfg *config.Config,
	mcpStore oauth.Store,
	target oauthTarget,
	deviceCode bool,
	stdin io.Reader,
	stdout io.Writer,
) error {
	if target.Kind == targetMCP {
		return loginServer(ctx, cfg, mcpStore, target.Name, stdout)
	}

	return loginCodexProvider(ctx, cfg, target.Name, deviceCode, stdin, stdout)
}

func loginCodexProvider(ctx context.Context, cfg *config.Config, name string, deviceCode bool, stdin io.Reader, stdout io.Writer) error {
	store, err := codexauth.NewFileStore(ctx, cfg.OAuth.Path)
	if err != nil {
		return err
	}
	client := codexauth.NewClient(&http.Client{Timeout: 30 * time.Second})
	cred, err := obtainCodexCredential(ctx, client, name, deviceCode, stdin, stdout)
	if err != nil {
		return err
	}
	if err := store.Save(ctx, cred); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(stdout, "%s: ChatGPT authorization stored for account %s\n", name, cred.PublicAccountID())

	return nil
}

// obtainCodexCredential runs the selected ChatGPT authorization flow
// (headless --device-code or interactive browser/PKCE) and returns the
// credential to persist.
func obtainCodexCredential(
	ctx context.Context,
	client *codexauth.Client,
	name string,
	deviceCode bool,
	stdin io.Reader,
	stdout io.Writer,
) (*codexauth.Credential, error) {
	if deviceCode {
		return deviceCodeCredential(ctx, client, name, stdout)
	}

	return browserCodeCredential(ctx, client, name, stdin, stdout)
}

// deviceCodeCredential runs the headless ChatGPT device flow: start the
// device authorization, show the verification URL and code, then poll until
// the user completes it.
func deviceCodeCredential(
	ctx context.Context,
	client *codexauth.Client,
	name string,
	stdout io.Writer,
) (*codexauth.Credential, error) {
	device, err := client.StartDeviceAuthorization(ctx)
	if err != nil {
		return nil, err
	}
	_, _ = fmt.Fprintf(stdout, "Open %s and enter code %s\n", device.VerificationURI, device.UserCode)

	cred, err := client.PollDeviceAuthorization(ctx, name, device)
	if err != nil {
		return nil, err
	}

	return cred, nil
}

// browserCodeCredential runs the interactive ChatGPT authorization-code
// flow: PKCE setup, the browser/manual callback wait, and the code
// exchange. Both the callback wait and the exchange are classified as
// authorization failures (not connection/config problems).
func browserCodeCredential(
	ctx context.Context,
	client *codexauth.Client,
	name string,
	stdin io.Reader,
	stdout io.Writer,
) (*codexauth.Credential, error) {
	verifier, challenge, err := codexauth.NewPKCE()
	if err != nil {
		return nil, err
	}
	state, err := codexauth.NewState()
	if err != nil {
		return nil, err
	}
	authURL, err := client.AuthorizationURL(state, challenge)
	if err != nil {
		return nil, err
	}

	_, _ = fmt.Fprintf(stdout, "Open this URL to authenticate:\n%s\n", authURL)
	result, err := waitCodexBrowserOrManual(ctx, authURL, state, stdin, stdout)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", errAuthorizationFailed, err)
	}

	cred, err := client.ExchangeCode(ctx, name, result.Code, verifier)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", errAuthorizationFailed, err)
	}

	return cred, nil
}

// codexAuthResult carries one completion of the racing browser/manual
// callback wait.
type codexAuthResult struct {
	value codexauth.AuthorizationResult
	err   error
}

// waitCodexBrowserOrManual races two completions of the ChatGPT
// authorization-code flow: the local loopback callback (opening a browser)
// and — when stdin is available — a pasted redirect URL or code.
func waitCodexBrowserOrManual(ctx context.Context, authURL, state string, stdin io.Reader, stdout io.Writer) (codexauth.AuthorizationResult, error) {
	flowCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	results := make(chan codexAuthResult, 2)
	go waitForLoopbackResult(flowCtx, authURL, state, results)
	if stdin != nil {
		_, _ = fmt.Fprintln(stdout, "If the callback cannot reach this machine, paste the redirect URL or code here:")
		go readManualCallback(stdin, state, results)
	}

	return collectAuthorizationResult(ctx, results)
}

// waitForLoopbackResult reports the loopback-callback outcome on results.
func waitForLoopbackResult(ctx context.Context, authURL, state string, results chan<- codexAuthResult) {
	value, err := codexauth.WaitForLoopback(ctx, authURL, state, codexauth.DefaultFlowTimeout, openLoginBrowser)
	results <- codexAuthResult{value: value, err: err}
}

// readManualCallback reads a pasted redirect URL or callback code from
// stdin and reports the parsed result on results; silent EOF or an empty
// line report nothing.
func readManualCallback(stdin io.Reader, state string, results chan<- codexAuthResult) {
	line, err := bufio.NewReader(stdin).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		results <- codexAuthResult{err: err}

		return
	}
	if strings.TrimSpace(line) == "" {
		return
	}
	value, parseErr := codexauth.ParseManualCallback(line, state)
	results <- codexAuthResult{value: value, err: parseErr}
}

// collectAuthorizationResult drains the racing result channel: the first
// successful completion wins; once a failure has been seen it is the
// terminal error, whatever ends the wait first (a second failure, outer
// cancellation or the flow timeout). A loopback bind/browser failure still
// leaves manual completion available — a manual parse failure is returned
// after both paths have failed or the bounded flow context expires.
func collectAuthorizationResult(ctx context.Context, results <-chan codexAuthResult) (codexauth.AuthorizationResult, error) {
	var firstErr error
	for {
		select {
		case got := <-results:
			if got.err == nil {
				return got.value, nil
			}
			if firstErr == nil {
				firstErr = got.err
			}
		case <-ctx.Done():
			return codexauth.AuthorizationResult{}, terminalAuthErr(firstErr, ctx.Err())
		case <-time.After(codexauth.DefaultFlowTimeout):
			return codexauth.AuthorizationResult{}, terminalAuthErr(firstErr, context.DeadlineExceeded)
		}
	}
}

// terminalAuthErr prefers an already-seen flow failure over the wait's own
// end condition (cancellation or deadline).
func terminalAuthErr(firstErr, fallback error) error {
	if firstErr != nil {
		return firstErr
	}

	return fallback
}

func openLoginBrowser(ctx context.Context, target string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.CommandContext(ctx, "open", target)
	case "windows":
		cmd = exec.CommandContext(ctx, "rundll32", "url.dll,FileProtocolHandler", target)
	default:
		cmd = exec.CommandContext(ctx, "xdg-open", target)
	}

	return cmd.Start()
}

// printLoginStatus implements `--status` (task 7.6): one row per server,
// driven entirely by the shared Credential.Usable predicate,
// never printing token material, and performing no network I/O (a local
// client_secret_file read is the only disk access, mirroring config.Load).
func printLoginStatus(ctx context.Context, store oauth.Store, cfg *config.Config, servers []string, stdout io.Writer) {
	now := time.Now()

	const rowFmt = "%-24s %-18s %-11s %-30s %s\n"

	_, _ = fmt.Fprintf(stdout, rowFmt, "SERVER", "GRANT", "CREDENTIAL", "EXPIRY", "ACTION REQUIRED")

	for _, name := range servers {
		printLoginStatusRow(ctx, store, name, cfg.MCPServers[name], now, stdout)
	}
}

// printLoginStatusRow renders one server row: the autonomous
// (client_credentials) placeholder when D7's client-side half says so,
// otherwise the stored-credential columns derived from Credential.Usable.
func printLoginStatusRow(
	ctx context.Context,
	store oauth.Store,
	name string,
	srv config.MCPServerConfig,
	now time.Time,
	stdout io.Writer,
) {
	const rowFmt = "%-24s %-18s %-11s %-30s %s\n"

	grant := srv.OAuth.Grant
	if grant == "" {
		grant = grantAuto
	}

	if isLikelyAutonomous(srv.OAuth, grant) {
		_, _ = fmt.Fprintf(stdout, rowFmt, name, grant, "n/a", "n/a", "none (client_credentials — autonomous)")

		return
	}

	cred, _ := store.Load(ctx, name)
	credentialCol, expiryCol, actionCol := loginStatusColumns(name, cred, now)
	_, _ = fmt.Fprintf(stdout, rowFmt, name, grant, credentialCol, expiryCol, actionCol)
}

// loginStatusColumns derives the credential/expiry/action columns from a
// stored MCP credential: "none" with the relogin action when absent,
// otherwise presence, the RFC3339 expiry when recorded, and the action the
// Usable predicate deserves at now.
func loginStatusColumns(name string, cred *oauth.Credential, now time.Time) (credentialCol, expiryCol, actionCol string) {
	credentialCol, expiryCol, actionCol = statusActionNone, "-", fmt.Sprintf("run `ready login %s`", name)
	if cred == nil {
		return credentialCol, expiryCol, actionCol
	}

	credentialCol = "present"
	if !cred.Expiry.IsZero() {
		expiryCol = cred.Expiry.Format(time.RFC3339)
	}
	if cred.Usable(now) {
		actionCol = statusActionNone
	}

	return credentialCol, expiryCol, actionCol
}

// isLikelyAutonomous reports whether srv is configured for the
// client_credentials grant without requiring the authorization-server
// round-trip the design doc's full resolution needs: an explicit
// grant: client_credentials, or grant: auto with both a client_id and a
// resolvable secret configured (the only client-side half of D7's
// resolution --status can check without network I/O).
func isLikelyAutonomous(oa *config.MCPOAuthConfig, grant string) bool {
	if grant == oauth.GrantClientCredentials {
		return true
	}

	if grant != grantAuto {
		return false
	}

	secret, err := oa.ResolveClientSecret()

	return err == nil && oa.ClientID != "" && secret != ""
}

// runForget implements `--forget` (task 7.7): deregister when RFC 7592
// registration-management fields are present, then delete regardless of
// whether deregistration succeeded.
func runForget(ctx context.Context, store oauth.Store, cfg *config.Config, name string, stdout, stderr io.Writer) error {
	srv := cfg.MCPServers[name]

	cred, err := store.Load(ctx, name)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "%v\n", err)

		return cli.Exit("", ExitErr)
	}

	if cred == nil {
		_, _ = fmt.Fprintf(stdout, "no stored credential for %q\n", name)

		return nil
	}

	if cred.RegistrationAccessToken != "" && cred.RegistrationClientURI != "" {
		httpClient := homemcp.NewScopedHTTPClient(&srv)

		if derr := oauth.Deregister(ctx, httpClient, cred.RegistrationClientURI, cred.RegistrationAccessToken); derr != nil {
			_, _ = fmt.Fprintf(stderr, "warning: failed to deregister client at the authorization server: %v\n", derr)
		}
	}

	if err := store.Delete(ctx, name); err != nil {
		_, _ = fmt.Fprintf(stderr, "%v\n", err)

		return cli.Exit("", ExitErr)
	}

	_, _ = fmt.Fprintf(stdout, "removed stored credential for %q\n", name)

	return nil
}

// mapLoginErr maps a login failure to the chat exit-code contract (task
// 7.8): SIGINT -> 130, a deadline (interactive fetcher timeout) -> 124, an
// authorization failure -> 3, everything else -> 1. The message was already
// printed by the caller, so the cli.Exit body stays empty to avoid a double
// print (mirroring mapChatError in chat.go).
func mapLoginErr(ctx context.Context, err error) error {
	switch {
	case errors.Is(ctx.Err(), context.Canceled):
		return cli.Exit("", ExitSIGINT)
	case errors.Is(err, context.DeadlineExceeded):
		return cli.Exit("", ExitTimeout)
	case errors.Is(err, errAuthorizationFailed):
		return cli.Exit("", ExitAuthFail)
	default:
		return cli.Exit("", ExitErr)
	}
}

// loginServer performs task 7.3's flow for a single server: discovery,
// registration if needed, interactive authorization, persistence via
// oauth.NewFromExchange, connect, and tool listing. A server that turns out
// not to require authorization, or that resolves to client_credentials
// (the design doc/D7 — `ready login` never applies to it), is reported and
// skipped, not treated as an error.
func loginServer(ctx context.Context, cfg *config.Config, store oauth.Store, name string, stdout io.Writer) error {
	srv := cfg.MCPServers[name]
	httpClient := homemcp.NewScopedHTTPClient(&srv)

	challenges, err := oauth.Probe(ctx, httpClient, srv.URL)
	if err != nil {
		if errors.Is(err, oauth.ErrNoAuthorizationRequired) {
			_, _ = fmt.Fprintf(stdout, "%s: server does not require authorization\n", name)

			return nil
		}

		return fmt.Errorf("probe failed: %w", err)
	}

	prm, err := oauth.DiscoverProtectedResource(ctx, httpClient, challenges, srv.URL)
	if err != nil {
		return fmt.Errorf("protected resource discovery failed: %w", err)
	}

	asm, err := oauth.DiscoverAuthServer(ctx, httpClient, prm)
	if err != nil {
		return fmt.Errorf("authorization server discovery failed: %w", err)
	}

	secret, err := srv.OAuth.ResolveClientSecret()
	if err != nil {
		return fmt.Errorf("failed to resolve client secret: %w", err)
	}

	hasClientCredentials := srv.OAuth.ClientID != "" && secret != ""
	authServerSupportsCC := slices.Contains(asm.GrantTypesSupported, "client_credentials")
	grant := oauth.ResolveGrant(srv.OAuth.Grant, hasClientCredentials, authServerSupportsCC)

	if grant == oauth.GrantClientCredentials {
		_, _ = fmt.Fprintf(stdout, "%s: uses client_credentials — autonomous, no login required\n", name)

		return nil
	}

	return loginAuthorizationCode(ctx, cfg, store, name, srv, httpClient, asm, prm, secret, stdout)
}

// loginAuthorizationCode runs the authorization_code branch of loginServer:
// resolve (or perform) client registration, mount the handler, connect, and
// list tools. It retries once, forcing a fresh Dynamic Client Registration,
// when a reused registration is repudiated with invalid_client.
func loginAuthorizationCode(
	ctx context.Context,
	cfg *config.Config,
	store oauth.Store,
	name string,
	srv config.MCPServerConfig,
	httpClient *http.Client,
	asm *oauthex.AuthServerMeta,
	prm *oauthex.ProtectedResourceMetadata,
	configuredSecret string,
	stdout io.Writer,
) error {
	existing, err := store.Load(ctx, name)
	if err != nil {
		return fmt.Errorf("failed to load stored credential: %w", err)
	}

	redirectURI := fmt.Sprintf("http://127.0.0.1:%d/callback", cfg.OAuth.RedirectPort)

	var fetcherErr error

	fetcher := oauth.InteractiveFetcher(cfg.OAuth.RedirectPort, oauth.DefaultAuthorizationTimeout)
	trackedFetcher := trackingFetcher(fetcher, &fetcherErr)

	for attempt := range 2 {
		client, retry, err := runAuthorizationCodeAttempt(ctx, cfg, store, name, srv, httpClient, asm, prm, existing, redirectURI, configuredSecret, trackedFetcher, stdout, attempt == 0)
		if client != nil {
			return nil
		}
		if !retry {
			return classifyLoginConnectErr(err, fetcherErr)
		}

		// A reused registration repudiated with invalid_client gets one
		// retry forcing a fresh Dynamic Client Registration.
		existing = nil
		fetcherErr = nil
	}

	return fmt.Errorf("%w: registration repeatedly repudiated by the authorization server", errAuthorizationFailed)
}

// runAuthorizationCodeAttempt performs one authorization-code attempt:
// registration resolution, handler build and connect. It returns the
// connected client on success; on failure, retry is true only when the
// error is the stored registration being repudiated with invalid_client on
// the first attempt (the single case that justifies retrying with a fresh
// Dynamic Client Registration).
func runAuthorizationCodeAttempt(
	ctx context.Context,
	cfg *config.Config,
	store oauth.Store,
	name string,
	srv config.MCPServerConfig,
	httpClient *http.Client,
	asm *oauthex.AuthServerMeta,
	prm *oauthex.ProtectedResourceMetadata,
	existing *oauth.Credential,
	redirectURI string,
	configuredSecret string,
	trackedFetcher auth.AuthorizationCodeFetcher,
	stdout io.Writer,
	firstAttempt bool,
) (client *homemcp.Client, retry bool, err error) {
	registeredClient, meta, regErr := resolveClientRegistration(ctx, httpClient, srv, name, asm, prm, existing, redirectURI, configuredSecret)
	if regErr != nil {
		return nil, false, fmt.Errorf("client registration failed: %w", regErr)
	}

	var persisted *oauth.Credential
	handler, err := buildAuthorizationCodeHandler(store, name, httpClient, registeredClient, meta, redirectURI, trackedFetcher, &persisted)
	if err != nil {
		return nil, false, fmt.Errorf("failed to build oauth handler: %w", err)
	}

	client, err = connectAuthorizedClient(ctx, srv, httpClient, handler)
	if err == nil {
		printLoginSuccess(ctx, cfg, name, srv, client, persisted, stdout)
		_ = client.Close()

		return client, false, nil
	}

	return nil, repudiatedStoredRegistration(err, firstAttempt, existing), err
}

// trackingFetcher wraps the interactive fetcher so a later connect failure
// can be attributed to the interactive authorization step (lets
// classifyLoginConnectErr decide the authorization-failure exit path).
func trackingFetcher(
	fetcher auth.AuthorizationCodeFetcher,
	fetcherErrOut *error,
) auth.AuthorizationCodeFetcher {
	return func(fctx context.Context, args *auth.AuthorizationArgs) (*auth.AuthorizationResult, error) {
		res, ferr := fetcher(fctx, args)
		if ferr != nil {
			*fetcherErrOut = ferr
		}

		return res, ferr
	}
}

// repudiatedStoredRegistration reports whether a failed first attempt is
// the authorization server repudiating a reused stored registration's
// client_id with invalid_client — the single case that justifies a retry
// forcing a fresh Dynamic Client Registration.
func repudiatedStoredRegistration(err error, firstAttempt bool, existing *oauth.Credential) bool {
	return firstAttempt && existing != nil && existing.ClientID != "" && oauth.IsInvalidClient(err)
}

// buildAuthorizationCodeHandler assembles the oauth handler for one
// authorization-code attempt: the tracked fetcher reports its failure to
// the caller, and the token-source callback persists the exchange while
// capturing the newly stored credential for the success report.
func buildAuthorizationCodeHandler(
	store oauth.Store,
	name string,
	httpClient *http.Client,
	registeredClient *oauthex.ClientCredentials,
	meta oauth.CredentialMeta,
	redirectURI string,
	trackedFetcher auth.AuthorizationCodeFetcher,
	persistedOut **oauth.Credential,
) (auth.OAuthHandler, error) {
	handlerCfg := oauth.HandlerConfig{
		ServerKey:        name,
		Grant:            oauth.GrantAuthorizationCode,
		HTTPClient:       httpClient,
		RegisteredClient: registeredClient,
		RedirectURL:      redirectURI,
		Fetcher:          trackedFetcher,
		NewTokenSource: func(tsCtx context.Context, oa2Cfg *oauth2.Config, tok *oauth2.Token) (oauth2.TokenSource, error) {
			ts, tsErr := oauth.NewFromExchange(tsCtx, store, httpClient, meta, oa2Cfg, tok)
			if tsErr != nil {
				return nil, tsErr
			}

			*persistedOut, _ = store.Load(tsCtx, name)

			return ts, nil
		},
	}

	return oauth.BuildHandler(handlerCfg)
}

// connectAuthorizedClient mounts the built handler over a fresh MCP client
// connection with exactly one transport-level retry, so an authorization
// failure surfaces as the flow's own error instead of a reconnect loop.
func connectAuthorizedClient(
	ctx context.Context,
	srv config.MCPServerConfig,
	httpClient *http.Client,
	handler auth.OAuthHandler,
) (*homemcp.Client, error) {
	return homemcp.NewClient(ctx, &srv, homemcp.ClientConfig{
		HTTPClient:   httpClient,
		OAuthHandler: handler,
		Timeout:      srv.Timeout,
		MaxRetries:   1,
		Logger:       zerolog.Nop(),
	})
}

// classifyLoginConnectErr wraps err in errAuthorizationFailed when the
// failure originated in the interactive fetcher (browser/callback problem)
// or was classified by internal/mcp as an authentication rejection, so
// mapLoginErr can map it to ExitAuthFail; a context-deadline fetcher error
// still classifies via errors.Is(err, context.DeadlineExceeded), giving it
// ExitTimeout instead. Any other error (discovery/registration/connection
// failure) passes through unchanged for the generic ExitErr path.
func classifyLoginConnectErr(err, fetcherErr error) error {
	if fetcherErr != nil {
		return fmt.Errorf("%w: %w", errAuthorizationFailed, fetcherErr)
	}

	if errors.Is(err, homemcp.ErrMCPAuth) {
		return fmt.Errorf("%w: %w", errAuthorizationFailed, err)
	}

	return err
}

// resolveClientRegistration resolves the *oauthex.ClientCredentials and
// oauth.CredentialMeta BuildHandler and NewFromExchange need, per the design section:
// reuse a stored registration's client_id when present, else a configured
// client_id (an operator's pre-registered confidential client, D7), else
// perform a one-time Dynamic Client Registration.
func resolveClientRegistration(
	ctx context.Context,
	httpClient *http.Client,
	srv config.MCPServerConfig,
	name string,
	asm *oauthex.AuthServerMeta,
	prm *oauthex.ProtectedResourceMetadata,
	existing *oauth.Credential,
	redirectURI string,
	configuredSecret string,
) (*oauthex.ClientCredentials, oauth.CredentialMeta, error) {
	meta := oauth.CredentialMeta{
		ServerKey:   name,
		ResourceURL: srv.URL,
		Issuer:      asm.Issuer,
		Resource:    prm.Resource,
		RedirectURI: redirectURI,
	}

	switch {
	case existing != nil && existing.ClientID != "":
		rc, updated := reuseStoredRegistration(existing, meta)

		return rc, updated, nil

	case srv.OAuth.ClientID != "":
		rc, updated := operatorRegisteredClient(srv.OAuth.ClientID, configuredSecret, meta)

		return rc, updated, nil

	default:
		return dynamicRegisterClient(ctx, httpClient, asm, name, redirectURI, meta)
	}
}

// reuseStoredRegistration rebuilds the client credentials (and
// registration-management metadata) from an earlier stored registration.
func reuseStoredRegistration(existing *oauth.Credential, meta oauth.CredentialMeta) (*oauthex.ClientCredentials, oauth.CredentialMeta) {
	rc := &oauthex.ClientCredentials{ClientID: existing.ClientID}
	if existing.ClientSecret != "" {
		rc.ClientSecretAuth = &oauthex.ClientSecretAuth{ClientSecret: existing.ClientSecret}
	}

	meta.TokenEndpointAuthMethod = existing.TokenEndpointAuthMethod
	meta.RegistrationAccessToken = existing.RegistrationAccessToken
	meta.RegistrationClientURI = existing.RegistrationClientURI
	meta.ClientSecretExpiresAt = existing.ClientSecretExpiresAt

	return rc, meta
}

// operatorRegisteredClient wraps an operator-pre-registered confidential
// client (D7): public/PKCE authentication by default, client_secret_post
// when a resolvable secret is configured.
func operatorRegisteredClient(clientID, configuredSecret string, meta oauth.CredentialMeta) (*oauthex.ClientCredentials, oauth.CredentialMeta) {
	rc := &oauthex.ClientCredentials{ClientID: clientID}
	meta.TokenEndpointAuthMethod = tokenEndpointAuthMethodNone

	if configuredSecret != "" {
		rc.ClientSecretAuth = &oauthex.ClientSecretAuth{ClientSecret: configuredSecret}
		meta.TokenEndpointAuthMethod = "client_secret_post"
	}

	return rc, meta
}

// dynamicRegisterClient performs the one-time Dynamic Client Registration
// fallback, refusing upfront when the authorization server advertises no
// registration_endpoint.
func dynamicRegisterClient(
	ctx context.Context,
	httpClient *http.Client,
	asm *oauthex.AuthServerMeta,
	name string,
	redirectURI string,
	meta oauth.CredentialMeta,
) (*oauthex.ClientCredentials, oauth.CredentialMeta, error) {
	if asm.RegistrationEndpoint == "" {
		return nil, meta, fmt.Errorf( // user-facing CLI error
			"no client_id configured and the authorization server advertises no registration_endpoint; set mcp_servers.%s.oauth.client_id",
			name,
		)
	}

	reg, err := oauth.RegisterClient(ctx, httpClient, asm.RegistrationEndpoint, redirectURI)
	if err != nil {
		return nil, meta, err
	}

	rc := &oauthex.ClientCredentials{ClientID: reg.ClientID}
	if reg.ClientSecret != "" {
		rc.ClientSecretAuth = &oauthex.ClientSecretAuth{ClientSecret: reg.ClientSecret}
	}

	meta.TokenEndpointAuthMethod = tokenEndpointAuthMethodNone
	meta.RegistrationAccessToken = reg.RegistrationAccessToken
	meta.RegistrationClientURI = reg.RegistrationClientURI
	meta.ClientSecretExpiresAt = reg.ClientSecretExpiresAt

	return rc, meta, nil
}

// printLoginSuccess prints task 7.4's output: the unfiltered tool catalog,
// the count the active profile would expose, a no-refresh-token warning when
// applicable (design task 5.6, printed here rather than logged since login
// has no logging subsystem), and the D13 restart notice.
func printLoginSuccess(
	ctx context.Context,
	cfg *config.Config,
	name string,
	srv config.MCPServerConfig,
	client *homemcp.Client,
	persisted *oauth.Credential,
	stdout io.Writer,
) {
	tools := client.Tools()

	_, _ = fmt.Fprintf(stdout, "%s: connected — %d tool(s) available:\n", name, len(tools))

	for _, t := range tools {
		info, err := t.Info(ctx)
		if err != nil {
			continue
		}

		_, _ = fmt.Fprintf(stdout, "  - %s: %s\n", info.Name, info.Desc)
	}

	if activeProfileName, _, profErr := cfg.ResolveProfile("", os.Getenv("READY_MCP_PROFILE")); profErr == nil {
		gated, _ := profile.GateOwner(ctx, name, tools, srv.Profiles, activeProfileName, false, zerolog.Nop())
		_, _ = fmt.Fprintf(stdout, "%s: %d of %d tool(s) exposed under the %q profile\n", name, len(gated), len(tools), activeProfileName)
	}

	if persisted != nil && persisted.RefreshToken == "" {
		_, _ = fmt.Fprintf(stdout,
			"%s: warning: no refresh token was issued — a new `ready login %s` will be required at every access-token expiry\n",
			name, name)
	}

	_, _ = fmt.Fprintf(stdout, "%s: if `ready serve` is already running, restart it for these tools to become available\n", name)
}
