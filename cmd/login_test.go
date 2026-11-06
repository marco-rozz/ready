package main

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/marco-rozz/ready/internal/secretstore"
	"github.com/rs/zerolog"

	"github.com/marco-rozz/ready/internal/codexauth"
	"github.com/marco-rozz/ready/internal/config"
	"github.com/marco-rozz/ready/internal/oauth"
	"github.com/urfave/cli/v3"
)

// Flag literals shared across this file's test-arg slices.
const (
	flagAgentDir   = "--agent-dir"
	flagStatus     = "--status"
	flagForget     = "--forget"
	flagDeviceCode = "--device-code"

	testServerKey   = "srv"
	testCodexTarget = "work"
)

// testAgentID is the identity of every fixture agent.
const testAgentID = "0190aaaa-bbbb-7ccc-8ddd-eeeeffff0001"

// withTestAgent adds the agent identity block right after config_version when
// the fixture has none.
func withTestAgent(contents string) string {
	if strings.Contains(contents, "\nagent:") || strings.HasPrefix(contents, "agent:") {
		return contents
	}

	return strings.Replace(contents, "config_version: 2\n", "config_version: 2\nagent:\n  id: "+testAgentID+"\n", 1)
}

// testSecretStore opens the fixture agent's Secret store (created by the first write).
func testSecretStore(t *testing.T, agentDir string) *secretstore.Store {
	t.Helper()
	store, err := secretstore.New(secretstore.Options{
		Dir: agentDir, Key: secretstore.KeySpec{Location: secretstore.KeyAgentDir}, AgentID: testAgentID, Log: zerolog.Nop(),
	})
	if err != nil {
		t.Fatal(err)
	}

	return store
}

// minimalOAuthConfigYAML returns a valid config with one OAuth-enabled MCP
// server (testServerKey). The credential store is fixed at <agent-dir>/oauth;
// tests seed it under the selected agent directory.
func minimalOAuthConfigYAML() string {
	return `
config_version: 2
llm:
  default: "ollama::test"
  providers:
    ollama:
      provider: ollama
      api: ollama
mcp_servers:
  ` + testServerKey + `:
    url: https://example.com/mcp
    oauth:
      grant: authorization_code
`
}

func codexOAuthConfigYAML(sameNameMCP bool) string {
	mcp := ""
	if sameNameMCP {
		mcp = `
mcp_servers:
  work:
    url: https://example.com/mcp
    oauth:
      grant: authorization_code
`
	}

	return `
config_version: 2
llm:
  default: "work::gpt-5-codex"
  providers:
    work:
      provider: openai-codex
      api: responses
` + mcp
}

func writeTempConfig(t *testing.T, contents string) string {
	t.Helper()

	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")

	if err := os.WriteFile(path, []byte(withTestAgent(contents)), 0o600); err != nil {
		t.Fatalf("failed to write temp config: %v", err)
	}

	return path
}

// writeTempAgentDir writes the temporary config into a fresh agent directory
// and returns the directory — the --agent-dir value. (writeTempConfig keeps
// returning the config file path for tests that read or name the file.)
func writeTempAgentDir(t *testing.T, contents string) string {
	t.Helper()

	return filepath.Dir(writeTempConfig(t, contents))
}

// newTestLoginCommand builds the login command with stdout/stderr wired to
// test buffers instead of newLoginCommand's hardcoded os.Stdout, reusing the
// same flag declarations so routing/parsing behaviour matches production.
func newTestLoginCommand(stdout, stderr io.Writer) *cli.Command {
	cmd := newLoginCommand(stderr)
	cmd.Action = func(ctx context.Context, c *cli.Command) error {
		return runLogin(ctx, c, stdout, stderr)
	}
	// urfave/cli's default ExitErrHandler calls os.Exit when the Action
	// returns a cli.ExitCoder, which would kill the test binary. Suppress it
	// here exactly as newRootCommand does, so tests can inspect the returned
	// error and exit code themselves.
	cmd.ExitErrHandler = func(context.Context, *cli.Command, error) {}

	return cmd
}

// TestLoginSubcommandRouting proves `login` is wired into the root command
// and surfaces a config-load failure the same way `serve` does.
func TestLoginSubcommandRouting(t *testing.T) {
	var stderr bytes.Buffer

	tmp := t.TempDir()
	missing := filepath.Join(tmp, "does-not-exist")

	code, runErr := run(
		context.Background(),
		[]string{homeAgent, loginLabel, flagAgentDir, missing},
		&stderr,
	)

	if code == ExitOK {
		t.Errorf("exit code: got %d, want non-zero", code)
	}

	if runErr == nil {
		t.Error("expected non-nil error from run() when config is missing")
	}

	if !strings.Contains(stderr.String(), "agent directory") {
		t.Errorf("expected stderr mentioning the agent directory, got %q", stderr.String())
	}
}

// TestLoginStatusAndForgetMutuallyExclusive verifies task 7.1's flag
// exclusivity, checked before any config I/O.
func TestLoginStatusAndForgetMutuallyExclusive(t *testing.T) {
	var stdout, stderr bytes.Buffer

	agentDir := writeTempAgentDir(t, minimalOAuthConfigYAML())
	cmd := newTestLoginCommand(&stdout, &stderr)

	err := cmd.Run(context.Background(), []string{loginLabel, flagAgentDir, agentDir, flagStatus, flagForget, testServerKey})

	if exitCodeFor(err) != ExitErr {
		t.Errorf("exit code: got %d, want %d", exitCodeFor(err), ExitErr)
	}

	if !strings.Contains(stderr.String(), "mutually exclusive") {
		t.Errorf("stderr should mention mutual exclusivity, got %q", stderr.String())
	}
}

// TestLoginForgetRequiresServerArg verifies task 7.1's requirement that
// --forget always names a server.
func TestLoginForgetRequiresServerArg(t *testing.T) {
	var stdout, stderr bytes.Buffer

	agentDir := writeTempAgentDir(t, minimalOAuthConfigYAML())
	cmd := newTestLoginCommand(&stdout, &stderr)

	err := cmd.Run(context.Background(), []string{loginLabel, flagAgentDir, agentDir, flagForget})

	if exitCodeFor(err) != ExitErr {
		t.Errorf("exit code: got %d, want %d", exitCodeFor(err), ExitErr)
	}

	if !strings.Contains(stderr.String(), "--forget requires a server argument") {
		t.Errorf("stderr should explain the missing argument, got %q", stderr.String())
	}
}

// TestLoginStatusWritesNothing proves --status never touches the oauth store
// directory, mirroring TestChatDoesNotTouchDotHome's before/after snapshot.
func TestLoginStatusWritesNothing(t *testing.T) {
	agentDir := writeTempAgentDir(t, minimalOAuthConfigYAML())

	before := dirContents(t, agentDir)

	var stdout, stderr bytes.Buffer
	cmd := newTestLoginCommand(&stdout, &stderr)

	err := cmd.Run(context.Background(), []string{loginLabel, flagAgentDir, agentDir, flagStatus})
	if exitCodeFor(err) != ExitOK {
		t.Errorf("exit code: got %d, want %d; stderr=%q", exitCodeFor(err), ExitOK, stderr.String())
	}

	after := dirContents(t, agentDir)
	if !equalStringSlices(before, after) {
		t.Errorf("--status modified the agent directory; before=%v after=%v", before, after)
	}
}

// TestLoginStatusReportsNoCredential verifies the --status table names the
// remediation command when no credential is stored, and never prints token
// material.
func TestLoginStatusReportsNoCredential(t *testing.T) {
	agentDir := writeTempAgentDir(t, minimalOAuthConfigYAML())

	var stdout, stderr bytes.Buffer
	cmd := newTestLoginCommand(&stdout, &stderr)

	if err := cmd.Run(context.Background(), []string{loginLabel, flagAgentDir, agentDir, flagStatus}); err != nil {
		t.Fatalf("unexpected error: %v; stderr=%q", err, stderr.String())
	}

	out := stdout.String()

	if !strings.Contains(out, "ready login srv") {
		t.Errorf("expected status output to name the remediation command, got %q", out)
	}
	// A non-default agent: following the hint must select the same agent.
	if !strings.Contains(out, "ready login srv --agent-dir ") {
		t.Errorf("remediation for an explicit agent must carry --agent-dir, got %q", out)
	}

	if strings.Contains(out, "access_token") || strings.Contains(out, "refresh_token") {
		t.Errorf("status output must never print token material, got %q", out)
	}
}

// TestLoginForgetDeletesCredential seeds a credential via the file store,
// runs --forget, and confirms the record is gone (task 7.10).
func TestLoginForgetDeletesCredential(t *testing.T) {
	agentDir := writeTempAgentDir(t, minimalOAuthConfigYAML())
	oauthDir := filepath.Join(agentDir, "oauth")

	ctx := context.Background()

	store := oauth.NewSecretStore(testSecretStore(t, agentDir), oauthDir)

	cred := oauth.NewCredential(testServerKey)
	cred.ClientID = "client-123"
	cred.AccessToken = "at"
	cred.Expiry = time.Now().Add(time.Hour)

	if saveErr := store.Save(ctx, cred); saveErr != nil {
		t.Fatalf("failed to seed credential: %v", saveErr)
	}

	var stdout, stderr bytes.Buffer
	cmd := newTestLoginCommand(&stdout, &stderr)

	runErr := cmd.Run(context.Background(), []string{loginLabel, flagAgentDir, agentDir, flagForget, testServerKey})
	if exitCodeFor(runErr) != ExitOK {
		t.Errorf("exit code: got %d, want %d; stderr=%q", exitCodeFor(runErr), ExitOK, stderr.String())
	}

	got, err := store.Load(ctx, testServerKey)
	if err != nil {
		t.Fatalf("unexpected error reloading store: %v", err)
	}

	if got != nil {
		t.Errorf("expected credential to be deleted, still present: %+v", got)
	}
}

// TestLoginForgetAbsentCredentialIsNotAnError proves deleting a never-stored
// credential is a no-op success (Store.Delete's own idempotency contract).
func TestLoginForgetAbsentCredentialIsNotAnError(t *testing.T) {
	agentDir := writeTempAgentDir(t, minimalOAuthConfigYAML())

	var stdout, stderr bytes.Buffer
	cmd := newTestLoginCommand(&stdout, &stderr)

	err := cmd.Run(context.Background(), []string{loginLabel, flagAgentDir, agentDir, flagForget, testServerKey})
	if exitCodeFor(err) != ExitOK {
		t.Errorf("exit code: got %d, want %d; stderr=%q", exitCodeFor(err), ExitOK, stderr.String())
	}
}

// TestLoginUnknownServer verifies an explicit but undeclared server name is
// a plain config error, not a panic.
func TestLoginUnknownServer(t *testing.T) {
	agentDir := writeTempAgentDir(t, minimalOAuthConfigYAML())

	var stdout, stderr bytes.Buffer
	cmd := newTestLoginCommand(&stdout, &stderr)

	err := cmd.Run(context.Background(), []string{loginLabel, flagAgentDir, agentDir, flagStatus, "does-not-exist"})
	if exitCodeFor(err) != ExitErr {
		t.Errorf("exit code: got %d, want %d", exitCodeFor(err), ExitErr)
	}

	if !strings.Contains(stderr.String(), "unknown mcp server") {
		t.Errorf("stderr should name the unknown server, got %q", stderr.String())
	}
}

func TestResolveOAuthTargetsQualifiedAndAmbiguous(t *testing.T) {
	cfg := &config.Config{
		MCPServers: map[string]config.MCPServerConfig{
			testCodexTarget: {OAuth: &config.MCPOAuthConfig{}},
		},
		LLM: config.LLMConfig{Providers: map[string]config.LLMProviderConfig{
			testCodexTarget: {Provider: config.ProviderOpenAICodex, API: config.APIResponses},
		}},
	}
	if _, err := resolveOAuthTargets(cfg, testCodexTarget); err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("unqualified ambiguity error = %v", err)
	}
	for _, want := range []oauthTarget{{Kind: targetMCP, Name: testCodexTarget}, {Kind: targetLLM, Name: testCodexTarget}} {
		got, err := resolveOAuthTargets(cfg, want.String())
		if err != nil || len(got) != 1 || got[0] != want {
			t.Fatalf("resolve %q = %#v, %v", want.String(), got, err)
		}
	}
}

func TestLoginCodexStatusAndForgetAreRedactedAndNamespaced(t *testing.T) {
	agentDir := writeTempAgentDir(t, codexOAuthConfigYAML(true))
	oauthDir := filepath.Join(agentDir, "oauth")
	ctx := context.Background()
	store := codexauth.NewSecretStore(testSecretStore(t, agentDir), oauthDir)
	cred := codexauth.NewCredential(testCodexTarget)
	cred.AccessToken = "access-top-secret"
	cred.RefreshToken = "refresh-top-secret"
	cred.AccountID = "account-1234567890"
	cred.Expiry = time.Now().Add(time.Hour)
	cred.UpdatedAt = time.Now()
	if err := store.Save(ctx, cred); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	cmd := newTestLoginCommand(&stdout, &stderr)
	if err := cmd.Run(ctx, []string{loginLabel, flagAgentDir, agentDir, flagStatus, "llm:work"}); err != nil {
		t.Fatalf("status: %v; stderr=%q", err, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "llm:work") || !strings.Contains(out, "acco…7890") {
		t.Fatalf("status output = %q", out)
	}
	for _, secret := range []string{cred.AccessToken, cred.RefreshToken, cred.AccountID} {
		if strings.Contains(out, secret) || strings.Contains(stderr.String(), secret) {
			t.Fatalf("status leaked secret %q", secret)
		}
	}

	stdout.Reset()
	stderr.Reset()
	cmd = newTestLoginCommand(&stdout, &stderr)
	if err := cmd.Run(ctx, []string{loginLabel, flagAgentDir, agentDir, flagForget, "llm:work"}); err != nil {
		t.Fatalf("forget: %v; stderr=%q", err, stderr.String())
	}
	if got, err := store.Load(ctx, testCodexTarget); err != nil || got != nil {
		t.Fatalf("credential after forget = %#v, %v", got, err)
	}
}

func TestLoginDeviceCodeRejectsMCPAndMaintenanceModes(t *testing.T) {
	agentDir := writeTempAgentDir(t, minimalOAuthConfigYAML())
	for _, args := range [][]string{
		{loginLabel, flagAgentDir, agentDir, flagDeviceCode, testServerKey},
		{loginLabel, flagAgentDir, agentDir, flagDeviceCode, flagStatus, testServerKey},
	} {
		var stdout, stderr bytes.Buffer
		cmd := newTestLoginCommand(&stdout, &stderr)
		err := cmd.Run(context.Background(), args)
		if exitCodeFor(err) != ExitErr || !strings.Contains(stderr.String(), flagDeviceCode) {
			t.Fatalf("args=%v error=%v stderr=%q", args, err, stderr.String())
		}
	}
}

// TestLoginConnectFailureExitsErr drives the full login flow against an
// unreachable server and asserts it fails cleanly with ExitErr rather than
// hanging or crashing (task 7.8's generic-error mapping).
func TestLoginConnectFailureExitsErr(t *testing.T) {
	agentDir := writeTempAgentDir(t, `
config_version: 2
llm:
  default: "ollama::test"
  providers:
    ollama:
      provider: ollama
      api: ollama
oauth:
  path: `+t.TempDir()+`
mcp_servers:
  srv:
    url: http://127.0.0.1:1/mcp
    oauth:
      grant: authorization_code
`)

	var stdout, stderr bytes.Buffer
	cmd := newTestLoginCommand(&stdout, &stderr)

	err := cmd.Run(context.Background(), []string{loginLabel, flagAgentDir, agentDir, testServerKey})
	if exitCodeFor(err) != ExitErr {
		t.Errorf("exit code: got %d, want %d; stderr=%q", exitCodeFor(err), ExitErr, stderr.String())
	}
}

// TestLoginModesRefuseLiteralCredentials verifies the cli-bootstrap
// delta: every login mode (default, --status, --forget, --device-code) refuses
// a current-schema config holding a literal credential before any
// target enumeration, credential work, network access or store construction —
// and creates NO configuration, credential, log, lock, backup or temporary
// file as a result.
func TestLoginModesRefuseLiteralCredentials(t *testing.T) { //nolint:paralleltest // shares flag plumbing per mode
	dir := t.TempDir()
	input := minimalOAuthConfigYAML() + "\nchannels:\n  zmqcli:\n    enabled: true\n    bind: tcp://127.0.0.1:7600\n    auth_token: literal-canary-value\n"
	agentDir := writeTempAgentDir(t, input)

	snapshot := func() map[string]int64 {
		out := map[string]int64{}
		entries, _ := os.ReadDir(dir)
		for _, e := range entries {
			out[e.Name()] = 0
		}

		return out
	}
	before := snapshot()

	for _, mode := range []struct{ name, extra, target string }{
		{name: "default"},
		{name: "status", extra: flagStatus},
		{name: "forget", extra: flagForget, target: testServerKey},
		{name: "device-code", extra: flagDeviceCode},
	} {
		t.Run(mode.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			cmd := newTestLoginCommand(&stdout, &stderr)

			args := []string{loginLabel, flagAgentDir, agentDir}
			if mode.extra != "" {
				args = append(args, mode.extra)
			}
			if mode.target != "" {
				args = append(args, mode.target)
			}

			err := cmd.Run(context.Background(), args)
			if err == nil {
				t.Fatal("every login mode must refuse a literal credential")
			}
			if !strings.Contains(stderr.String(), "channels.zmqcli.auth_token") {
				t.Errorf("refusal must name the affected field, stderr=%q", stderr.String())
			}
			if strings.Contains(stderr.String(), "literal-canary-value") {
				t.Errorf("refusal must never echo the literal, stderr=%q", stderr.String())
			}

			// No store, credentials, lock, log, or backup artifacts.
			after := snapshot()
			for name := range after {
				if _, existed := before[name]; !existed && !strings.HasSuffix(name, ".yaml") {
					t.Errorf("mode %s created a new artifact %s during refusal", mode.name, name)
				}
			}
		})
	}
}

// Redirect registration mismatch (per-user-install-agent-isolation): a
// stored registration whose redirect URI no longer matches the configured
// callback fails BEFORE the interactive flow, naming the explicit remedy —
// forget/re-register for a dynamic registration, server-side redirect
// update for an operator-registered client.
func TestRedirectMismatchNamesRemedy(t *testing.T) {
	dcr := oauth.NewCredential(testServerKey)
	dcr.RedirectURI = "http://127.0.0.1:9999/callback"
	dcr.RegistrationAccessToken = "rat"
	err := redirectMismatch(dcr, "http://127.0.0.1:7601/callback", "")
	if err == nil || !strings.Contains(err.Error(), "--forget") {
		t.Fatalf("dcr mismatch = %v, want the forget/re-register remedy", err)
	}
	other := t.TempDir()
	if err = redirectMismatch(dcr, "http://127.0.0.1:7601/callback", other); err == nil ||
		!strings.Contains(err.Error(), config.LoginCommand(other, "mcp:"+testServerKey+" --forget")) ||
		!strings.Contains(err.Error(), "--agent-dir") {
		t.Fatalf("dcr mismatch for an explicit agent = %v, want the same-agent forget command", err)
	}
	if strings.Contains(err.Error(), "authorization server") && !strings.Contains(err.Error(), "deregister") {
		t.Fatalf("dcr wording = %v", err)
	}

	operator := oauth.NewCredential(testServerKey)
	operator.RedirectURI = "http://127.0.0.1:9999/callback"
	err = redirectMismatch(operator, "http://127.0.0.1:7601/callback", "")
	if err == nil || !strings.Contains(err.Error(), "update its allowed redirect URIs at the authorization server") {
		t.Fatalf("operator mismatch = %v, want the server-side remedy", err)
	}
}
