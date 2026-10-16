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

	"github.com/marco-rozz/ready/internal/codexauth"
	"github.com/marco-rozz/ready/internal/config"
	"github.com/marco-rozz/ready/internal/oauth"
	"github.com/urfave/cli/v3"
)

// Flag literals shared across this file's test-arg slices.
const (
	flagConfig = "--config"
	flagStatus = "--status"
	flagForget = "--forget"

	testServerKey   = "srv"
	testCodexTarget = "work"
)

// minimalOAuthConfigYAML returns a valid config with one OAuth-enabled MCP
// server (testServerKey) whose credential store lives under oauthDir.
func minimalOAuthConfigYAML(oauthDir string) string {
	return `
llm:
  default: "ollama::test"
  providers:
    ollama:
      provider: ollama
      api: ollama
oauth:
  path: ` + oauthDir + `
mcp_servers:
  ` + testServerKey + `:
    url: https://example.com/mcp
    oauth:
      grant: authorization_code
`
}

func codexOAuthConfigYAML(oauthDir string, sameNameMCP bool) string {
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
llm:
  default: "work::gpt-5-codex"
  providers:
    work:
      provider: openai-codex
      api: responses
oauth:
  path: ` + oauthDir + mcp
}

func writeTempConfig(t *testing.T, contents string) string {
	t.Helper()

	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")

	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("failed to write temp config: %v", err)
	}

	return path
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
	missing := filepath.Join(tmp, "does-not-exist.yaml")

	code, runErr := run(
		context.Background(),
		[]string{homeAgent, loginLabel, flagConfig, missing},
		&stderr,
	)

	if code == ExitOK {
		t.Errorf("exit code: got %d, want non-zero", code)
	}

	if runErr == nil {
		t.Error("expected non-nil error from run() when config is missing")
	}

	if !strings.Contains(stderr.String(), "config") {
		t.Errorf("expected stderr mentioning config, got %q", stderr.String())
	}
}

// TestLoginStatusAndForgetMutuallyExclusive verifies task 7.1's flag
// exclusivity, checked before any config I/O.
func TestLoginStatusAndForgetMutuallyExclusive(t *testing.T) {
	var stdout, stderr bytes.Buffer

	configPath := writeTempConfig(t, minimalOAuthConfigYAML(t.TempDir()))
	cmd := newTestLoginCommand(&stdout, &stderr)

	err := cmd.Run(context.Background(), []string{loginLabel, flagConfig, configPath, flagStatus, flagForget, testServerKey})

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

	configPath := writeTempConfig(t, minimalOAuthConfigYAML(t.TempDir()))
	cmd := newTestLoginCommand(&stdout, &stderr)

	err := cmd.Run(context.Background(), []string{loginLabel, flagConfig, configPath, flagForget})

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
	oauthDir := t.TempDir()
	configPath := writeTempConfig(t, minimalOAuthConfigYAML(oauthDir))

	before := dirContents(t, oauthDir)

	var stdout, stderr bytes.Buffer
	cmd := newTestLoginCommand(&stdout, &stderr)

	err := cmd.Run(context.Background(), []string{loginLabel, flagConfig, configPath, flagStatus})
	if exitCodeFor(err) != ExitOK {
		t.Errorf("exit code: got %d, want %d; stderr=%q", exitCodeFor(err), ExitOK, stderr.String())
	}

	after := dirContents(t, oauthDir)
	if !equalStringSlices(before, after) {
		t.Errorf("--status modified the oauth store dir; before=%v after=%v", before, after)
	}
}

// TestLoginStatusReportsNoCredential verifies the --status table names the
// remediation command when no credential is stored, and never prints token
// material.
func TestLoginStatusReportsNoCredential(t *testing.T) {
	configPath := writeTempConfig(t, minimalOAuthConfigYAML(t.TempDir()))

	var stdout, stderr bytes.Buffer
	cmd := newTestLoginCommand(&stdout, &stderr)

	if err := cmd.Run(context.Background(), []string{loginLabel, flagConfig, configPath, flagStatus}); err != nil {
		t.Fatalf("unexpected error: %v; stderr=%q", err, stderr.String())
	}

	out := stdout.String()

	if !strings.Contains(out, "ready login srv") {
		t.Errorf("expected status output to name the remediation command, got %q", out)
	}

	if strings.Contains(out, "access_token") || strings.Contains(out, "refresh_token") {
		t.Errorf("status output must never print token material, got %q", out)
	}
}

// TestLoginForgetDeletesCredential seeds a credential via the file store,
// runs --forget, and confirms the record is gone (task 7.10).
func TestLoginForgetDeletesCredential(t *testing.T) {
	oauthDir := t.TempDir()
	configPath := writeTempConfig(t, minimalOAuthConfigYAML(oauthDir))

	ctx := context.Background()

	store, err := oauth.NewFileStore(ctx, oauthDir)
	if err != nil {
		t.Fatalf("failed to build file store: %v", err)
	}

	cred := oauth.NewCredential(testServerKey)
	cred.ClientID = "client-123"
	cred.AccessToken = "at"
	cred.Expiry = time.Now().Add(time.Hour)

	if saveErr := store.Save(ctx, cred); saveErr != nil {
		t.Fatalf("failed to seed credential: %v", saveErr)
	}

	var stdout, stderr bytes.Buffer
	cmd := newTestLoginCommand(&stdout, &stderr)

	runErr := cmd.Run(context.Background(), []string{loginLabel, flagConfig, configPath, flagForget, testServerKey})
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
	configPath := writeTempConfig(t, minimalOAuthConfigYAML(t.TempDir()))

	var stdout, stderr bytes.Buffer
	cmd := newTestLoginCommand(&stdout, &stderr)

	err := cmd.Run(context.Background(), []string{loginLabel, flagConfig, configPath, flagForget, testServerKey})
	if exitCodeFor(err) != ExitOK {
		t.Errorf("exit code: got %d, want %d; stderr=%q", exitCodeFor(err), ExitOK, stderr.String())
	}
}

// TestLoginUnknownServer verifies an explicit but undeclared server name is
// a plain config error, not a panic.
func TestLoginUnknownServer(t *testing.T) {
	configPath := writeTempConfig(t, minimalOAuthConfigYAML(t.TempDir()))

	var stdout, stderr bytes.Buffer
	cmd := newTestLoginCommand(&stdout, &stderr)

	err := cmd.Run(context.Background(), []string{loginLabel, flagConfig, configPath, flagStatus, "does-not-exist"})
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
	oauthDir := t.TempDir()
	configPath := writeTempConfig(t, codexOAuthConfigYAML(oauthDir, true))
	ctx := context.Background()
	store, err := codexauth.NewFileStore(ctx, oauthDir)
	if err != nil {
		t.Fatal(err)
	}
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
	if err := cmd.Run(ctx, []string{loginLabel, flagConfig, configPath, flagStatus, "llm:work"}); err != nil {
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
	if err := cmd.Run(ctx, []string{loginLabel, flagConfig, configPath, flagForget, "llm:work"}); err != nil {
		t.Fatalf("forget: %v; stderr=%q", err, stderr.String())
	}
	if got, err := store.Load(ctx, testCodexTarget); err != nil || got != nil {
		t.Fatalf("credential after forget = %#v, %v", got, err)
	}
}

func TestLoginDeviceCodeRejectsMCPAndMaintenanceModes(t *testing.T) {
	configPath := writeTempConfig(t, minimalOAuthConfigYAML(t.TempDir()))
	for _, args := range [][]string{
		{loginLabel, flagConfig, configPath, "--device-code", testServerKey},
		{loginLabel, flagConfig, configPath, "--device-code", flagStatus, testServerKey},
	} {
		var stdout, stderr bytes.Buffer
		cmd := newTestLoginCommand(&stdout, &stderr)
		err := cmd.Run(context.Background(), args)
		if exitCodeFor(err) != ExitErr || !strings.Contains(stderr.String(), "--device-code") {
			t.Fatalf("args=%v error=%v stderr=%q", args, err, stderr.String())
		}
	}
}

// TestLoginConnectFailureExitsErr drives the full login flow against an
// unreachable server and asserts it fails cleanly with ExitErr rather than
// hanging or crashing (task 7.8's generic-error mapping).
func TestLoginConnectFailureExitsErr(t *testing.T) {
	configPath := writeTempConfig(t, `
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

	err := cmd.Run(context.Background(), []string{loginLabel, flagConfig, configPath, testServerKey})
	if exitCodeFor(err) != ExitErr {
		t.Errorf("exit code: got %d, want %d; stderr=%q", exitCodeFor(err), ExitErr, stderr.String())
	}
}
