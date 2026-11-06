package config

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// withHome points the user home at dir for the duration of t (os.UserHomeDir
// honours $HOME on unix).
func withHome(t *testing.T, dir string) {
	t.Helper()

	old, hadHome := os.LookupEnv("HOME")
	t.Setenv("HOME", dir)
	t.Cleanup(func() {
		if hadHome {
			_ = os.Setenv("HOME", old)
		}
	})
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// phys resolves a path to its physical identity so expectations match
// symlink-resolving selections on symlinked temp roots (macOS /var).
func phys(t *testing.T, p string) string {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(p)
	if err != nil {
		t.Fatal(err)
	}

	return filepath.Clean(resolved)
}

// Selection: explicit --agent-dir wins with no fallback; the default is
// unconditionally ~/.ready, independent of cwd.
func TestSelectAgentDir(t *testing.T) {
	t.Run("explicit directory wins and never falls back", func(t *testing.T) {
		home := t.TempDir()
		withHome(t, home)
		writeFile(t, filepath.Join(home, ".ready", "config.yaml"), "config_version: 2\nagent:\n  id: 0190aaaa-bbbb-7ccc-8ddd-eeeeffff0001\nchannels:")
		agentDir := filepath.Join(home, "agents", "casa")
		writeFile(t, filepath.Join(agentDir, "config.yaml"), "config_version: 2\nagent:\n  id: 0190aaaa-bbbb-7ccc-8ddd-eeeeffff0001\nchannels:")

		res, err := SelectAgentDir(context.Background(), agentDir)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if res.AgentDir != phys(t, agentDir) || res.Origin != OriginFlag {
			t.Errorf("resolution = %+v, want the flagged directory with origin flag", res)
		}
		if res.Path != filepath.Join(phys(t, agentDir), ConfigFileName) {
			t.Errorf("path = %q, want the sole config.yaml inside the agent directory", res.Path)
		}
	})

	t.Run("default is cwd independent", func(t *testing.T) {
		home := t.TempDir()
		withHome(t, home)
		writeFile(t, filepath.Join(home, ".ready", "config.yaml"), "config_version: 2\nagent:\n  id: 0190aaaa-bbbb-7ccc-8ddd-eeeeffff0001\nchannels:")
		// A cwd-local candidate must be ignored entirely.
		wd, err := os.Getwd()
		if err != nil {
			t.Fatal(err)
		}
		writeFile(t, filepath.Join(wd, ".ready", "config.yaml"), "config_version: 2\nagent:\n  id: 0190aaaa-bbbb-7ccc-8ddd-eeeeffff0001\nchannels:")
		defer os.Remove(filepath.Join(wd, ".ready", "config.yaml"))

		res, err := SelectAgentDir(context.Background(), "")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if res.Origin != OriginDefault {
			t.Errorf("origin = %q, want default", res.Origin)
		}
		if res.AgentDir != phys(t, filepath.Join(home, ".ready")) {
			t.Errorf("agent dir = %q, want %q", res.AgentDir, phys(t, filepath.Join(home, ".ready")))
		}
	})

	t.Run("missing explicit directory fails without fallback", func(t *testing.T) {
		home := t.TempDir()
		withHome(t, home)
		writeFile(t, filepath.Join(home, ".ready", "config.yaml"), "config_version: 2\nagent:\n  id: 0190aaaa-bbbb-7ccc-8ddd-eeeeffff0001\nchannels:")
		missing := filepath.Join(home, "agents", "ghost")

		_, err := SelectAgentDir(context.Background(), missing)
		if err == nil {
			t.Fatal("expected an error")
		}
		if !strings.Contains(err.Error(), missing) {
			t.Errorf("error = %v, want the missing directory named", err)
		}
		if _, serr := os.Stat(missing); !os.IsNotExist(serr) {
			t.Errorf("the failed selection must not create the directory")
		}
	})

	t.Run("explicit path that is not a directory fails", func(t *testing.T) {
		home := t.TempDir()
		withHome(t, home)
		file := filepath.Join(home, "not-a-dir")
		writeFile(t, file, "")

		_, err := SelectAgentDir(context.Background(), file)
		if err == nil {
			t.Fatal("expected an error")
		}
		if !strings.Contains(err.Error(), "not a directory") {
			t.Errorf("error = %v, want the not-a-directory wording", err)
		}
	})
}

// The unresolvable-user-home precision cases.
func TestSelectAgentDir_HomeFailures(t *testing.T) {
	t.Run("flag with no home still selects", func(t *testing.T) {
		os.Unsetenv("HOME")
		t.Cleanup(func() { _ = os.Setenv("HOME", os.Getenv("XDG_TEST_BACKUP_HOME")) })

		agentDir := t.TempDir()
		res, err := SelectAgentDir(context.Background(), agentDir)
		if err != nil {
			t.Fatalf("explicit --agent-dir must select without a user home, got %v", err)
		}
		if res.Origin != OriginFlag {
			t.Errorf("origin = %q, want flag", res.Origin)
		}
	})

	t.Run("default without home names the remedy", func(t *testing.T) {
		os.Unsetenv("HOME")

		_, err := SelectAgentDir(context.Background(), "")
		if err == nil {
			t.Fatal("expected the undeterminable-home error")
		}
		if !strings.Contains(err.Error(), "user home") || !strings.Contains(err.Error(), "--agent-dir") {
			t.Errorf("error = %v, want the user-home failure naming the --agent-dir remedy", err)
		}
	})
}

// Runtime-home derivation: the elected config's directory becomes the root of
// every runtime path default.
func TestApplyRuntimeHome_DerivesRuntimePaths(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	writeFile(t, cfgPath, "config_version: 2\nagent:\n  id: 0190aaaa-bbbb-7ccc-8ddd-eeeeffff0001\nchannels:")

	cfg, err := Load(context.Background(), cfgPath)
	if err != nil {
		t.Fatal(err)
	}

	if cfg.ElectedPath != cfgPath {
		t.Errorf("ElectedPath = %q, want %q", cfg.ElectedPath, cfgPath)
	}
	if cfg.RuntimeHome != dir {
		t.Errorf("RuntimeHome = %q, want %q", cfg.RuntimeHome, dir)
	}
	if cfg.Skills.Path != filepath.Join(dir, "skills") {
		t.Errorf("Skills.Path = %q, want under the runtime home", cfg.Skills.Path)
	}
	if cfg.Sessions.Path != filepath.Join(dir, "sessions") {
		t.Errorf("Sessions.Path = %q, want under the runtime home", cfg.Sessions.Path)
	}
}

// Explicit relative runtime paths resolve against the runtime home; absolute
// ones as-is.
func TestApplyRuntimeHome_RelativePathsResolve(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	yaml := `
config_version: 2
agent:
  id: 0190aaaa-bbbb-7ccc-8ddd-eeeeffff0001
skills:
  path: custom-skills/
sessions:
  path: data/sessions
logging:
  file_path: logs/
`
	writeFile(t, cfgPath, yaml)

	cfg, err := Load(context.Background(), cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Skills.Path != filepath.Join(dir, "custom-skills")+"/" {
		t.Errorf("Skills.Path = %q, want %q", cfg.Skills.Path, filepath.Join(dir, "custom-skills")+"/")
	}
	if cfg.Sessions.Path != filepath.Join(dir, "data", "sessions") {
		t.Errorf("Sessions.Path = %q", cfg.Sessions.Path)
	}
	if cfg.Logging.FilePath != filepath.Join(dir, "logs")+"/" {
		t.Errorf("Logging.FilePath = %q, want the trailing directory form preserved", cfg.Logging.FilePath)
	}
}

// OAuth store anchoring: the store is fixed at <agent-dir>/oauth.
// oauth.path is a removed key — any spelling is rejected by Validate.
func TestApplyRuntimeHome_OAuthStoreAnchoring(t *testing.T) {
	newYAML := func() string {
		return `
config_version: 2
agent:
  id: 0190aaaa-bbbb-7ccc-8ddd-eeeeffff0001
llm:
  default: "codex::gpt-5"
  providers:
    codex:
      provider: openai-codex
      api: responses
`
	}

	t.Run("store is anchored inside the agent directory", func(t *testing.T) {
		dir := t.TempDir()
		cfgPath := filepath.Join(dir, "config.yaml")
		writeFile(t, cfgPath, newYAML())

		cfg, err := Load(context.Background(), cfgPath)
		if err != nil {
			t.Fatal(err)
		}
		if want := filepath.Join(dir, "oauth"); cfg.OAuth.Path != want {
			t.Errorf("OAuth.Path = %q, want the agent-directory store %q", cfg.OAuth.Path, want)
		}
		if cfg.LLM.OAuthPath != cfg.OAuth.Path {
			t.Errorf("LLM.OAuthPath diverged from the store: %q vs %q — daemon and login would resolve different stores", cfg.LLM.OAuthPath, cfg.OAuth.Path)
		}
	})

	t.Run("moved agent directory keeps its store", func(t *testing.T) {
		// Offline move: same relative layout, different physical parent.
		first := t.TempDir()
		writeFile(t, filepath.Join(first, "config.yaml"), newYAML())
		cfg, err := Load(context.Background(), filepath.Join(first, "config.yaml"))
		if err != nil {
			t.Fatal(err)
		}
		second := t.TempDir()
		writeFile(t, filepath.Join(second, "config.yaml"), newYAML())
		cfg2, err := Load(context.Background(), filepath.Join(second, "config.yaml"))
		if err != nil {
			t.Fatal(err)
		}
		if cfg.OAuth.Path == cfg2.OAuth.Path {
			t.Errorf("distinct directories resolved one store: %q", cfg.OAuth.Path)
		}
	})

	t.Run("no OAuth targets still accepts and does not fail", func(t *testing.T) {
		dir := t.TempDir()
		cfgPath := filepath.Join(dir, "config.yaml")
		writeFile(t, cfgPath, "config_version: 2\nagent:\n  id: 0190aaaa-bbbb-7ccc-8ddd-eeeeffff0001\nchannels:\n  cli:\n    enabled: true\n")

		cfg, err := Load(context.Background(), cfgPath)
		if err != nil {
			t.Fatalf("a config without OAuth targets must not require a user home: %v", err)
		}
		if want := filepath.Join(dir, "oauth"); cfg.OAuth.Path != want {
			t.Errorf("OAuth.Path = %q, want %q", cfg.OAuth.Path, want)
		}
	})

	t.Run("oauth.path is a removed key", func(t *testing.T) {
		dir := t.TempDir()
		writeFile(t, filepath.Join(dir, "config.yaml"), newYAML()+"oauth:\n  path: /elsewhere\n")

		// Strict decode: the removed key is refused at Load with the same
		// operator-facing reason Validate used to produce.
		_, err := Load(context.Background(), filepath.Join(dir, "config.yaml"))
		if err == nil || !strings.Contains(err.Error(), "oauth.path") || !strings.Contains(err.Error(), "<agent-dir>/oauth") {
			t.Fatalf("load = %v, want oauth.path refusal naming the fixed store", err)
		}
	})

	t.Run("oauth.path relative to another agent is also refused", func(t *testing.T) {
		dir := t.TempDir()
		writeFile(t, filepath.Join(dir, "config.yaml"), newYAML()+"oauth:\n  path: ../other-agent/oauth\n")

		_, err := Load(context.Background(), filepath.Join(dir, "config.yaml"))
		if err == nil || !strings.Contains(err.Error(), "oauth.path") {
			t.Fatalf("load = %v, want the removed-key refusal", err)
		}
	})
}

// skills.library_path: absolute as-is, relative against the runtime home,
// default <program storage>/library/skills.
func TestSkillsLibraryPath(t *testing.T) {
	t.Run("default program storage", func(t *testing.T) {
		home := t.TempDir()
		withHome(t, home)
		writeFile(t, filepath.Join(home, ".ready", "config.yaml"), "config_version: 2\nagent:\n  id: 0190aaaa-bbbb-7ccc-8ddd-eeeeffff0001\nchannels:\n  cli:\n    enabled: true\n")

		cfg, err := Load(context.Background(), filepath.Join(home, ".ready", "config.yaml"))
		if err != nil {
			t.Fatal(err)
		}
		if want := filepath.Join(home, ".local", "share", "ready", "library", "skills"); cfg.Skills.LibraryPath != want {
			t.Errorf("LibraryPath = %q, want %q", cfg.Skills.LibraryPath, want)
		}
	})

	t.Run("relative override against the runtime home", func(t *testing.T) {
		dir := t.TempDir()
		writeFile(t, filepath.Join(dir, "config.yaml"), "config_version: 2\nagent:\n  id: 0190aaaa-bbbb-7ccc-8ddd-eeeeffff0001\nskills:\n  library_path: ../resources/skills/\n")

		cfg, err := Load(context.Background(), filepath.Join(dir, "config.yaml"))
		if err != nil {
			t.Fatal(err)
		}
		if cfg.Skills.LibraryPath != filepath.Join(filepath.Dir(dir), "resources", "skills")+"/" {
			t.Errorf("LibraryPath = %q, want resolved against the runtime home", cfg.Skills.LibraryPath)
		}
	})
}

// skills.sync validation: unset = auto; invalid values fail boot.
func TestSkillsSyncValidation(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "config.yaml"), "config_version: 2\nagent:\n  id: 0190aaaa-bbbb-7ccc-8ddd-eeeeffff0001\nskills:\n  sync: sometimes\n")

	cfg, err := Load(context.Background(), filepath.Join(dir, "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	// Load parses; Validate is the boot-time gate that fails an invalid
	// skills.sync value.
	if verr := cfg.Validate(); verr == nil {
		t.Fatal("expected skills.sync validation error from Validate")
	}

	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	writeFileT(t, cfgPath, "config_version: 2\nagent:\n  id: 0190aaaa-bbbb-7ccc-8ddd-eeeeffff0001\nchannels:\n  cli:\n    enabled: true\n")

	if _, err := Load(context.Background(), cfgPath); err != nil {
		t.Fatalf("an absent skills.sync must fail nothing: %v", err)
	}
}

// writeFileT exists so blocks that already hold their own temp path can reuse
// the shared writer without leaking a bare *testing.T into helpers.
func writeFileT(t *testing.T, path, content string) {
	t.Helper()

	writeFile(t, path, content)
}

// Physical identity: alias spellings (symlinked directory or its components)
// select the same physical agent directory and therefore the same state
// paths; a linked config leaf is rejected.
func TestSelectAgentDir_PhysicalIdentity(t *testing.T) {
	t.Run("symlinked directory alias selects one physical agent", func(t *testing.T) {
		home := t.TempDir()
		withHome(t, home)
		physicalDir := filepath.Join(home, "agents", "casa")
		writeFile(t, filepath.Join(physicalDir, "config.yaml"), "config_version: 2\nagent:\n  id: 0190aaaa-bbbb-7ccc-8ddd-eeeeffff0001\nchannels:")
		alias := filepath.Join(home, "alias")
		if err := os.Symlink(physicalDir, alias); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}

		viaPhysical, err := SelectAgentDir(context.Background(), physicalDir)
		if err != nil {
			t.Fatal(err)
		}
		viaAlias, err := SelectAgentDir(context.Background(), alias)
		if err != nil {
			t.Fatal(err)
		}
		if viaPhysical.AgentDir != viaAlias.AgentDir {
			t.Errorf("alias %q resolved to %q, physical dir %q — identities diverged", alias, viaAlias.AgentDir, viaPhysical.AgentDir)
		}
		if viaPhysical.Path != viaAlias.Path {
			t.Errorf("config paths diverged: %q vs %q", viaPhysical.Path, viaAlias.Path)
		}
	})

	t.Run("relative path resolves to the same identity as absolute", func(t *testing.T) {
		home := t.TempDir()
		withHome(t, home)
		dir := filepath.Join(home, "agents", "casa")
		writeFile(t, filepath.Join(dir, "config.yaml"), "config_version: 2\nagent:\n  id: 0190aaaa-bbbb-7ccc-8ddd-eeeeffff0001\nchannels:")

		t.Chdir(home)
		abs, err := SelectAgentDir(context.Background(), dir)
		if err != nil {
			t.Fatal(err)
		}
		rel, err := SelectAgentDir(context.Background(), "agents/casa")
		if err != nil {
			t.Fatal(err)
		}
		if abs.AgentDir != rel.AgentDir || rel.AgentDir != phys(t, dir) {
			t.Errorf("relative selection %q diverged from absolute %q", rel.AgentDir, abs.AgentDir)
		}
	})

	t.Run("default selection resolves an aliased home", func(t *testing.T) {
		realHome := t.TempDir()
		aliasHome := t.TempDir()
		alias := filepath.Join(aliasHome, "home-alias")
		if err := os.Symlink(realHome, alias); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
		withHome(t, alias)

		res, err := SelectAgentDir(context.Background(), "")
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(res.AgentDir, phys(t, realHome)) {
			t.Errorf("agent dir = %q, want inside the physical home %q", res.AgentDir, phys(t, realHome))
		}
	})

	t.Run("default selection tolerates a missing default directory", func(t *testing.T) {
		home := t.TempDir()
		withHome(t, home)

		res, err := SelectAgentDir(context.Background(), "")
		if err != nil {
			t.Fatalf("a fresh machine must still select the default directory: %v", err)
		}
		if want := filepath.Join(phys(t, home), ".ready"); res.AgentDir != want {
			t.Errorf("agent dir = %q, want %q", res.AgentDir, want)
		}
	})

	t.Run("symlinked config leaf is rejected", func(t *testing.T) {
		dir := t.TempDir()
		elsewhere := filepath.Join(t.TempDir(), "outside.yaml")
		writeFile(t, elsewhere, "config_version: 2\nagent:\n  id: 0190aaaa-bbbb-7ccc-8ddd-eeeeffff0001\nchannels:")
		cfgPath := filepath.Join(dir, "config.yaml")
		if err := os.Symlink(elsewhere, cfgPath); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}

		if err := RejectLinkedConfig(cfgPath); err == nil {
			t.Fatal("expected the linked config leaf to be rejected")
		} else if !strings.Contains(err.Error(), "regular file") {
			t.Errorf("error = %v, want the regular-file wording", err)
		}

		if err := os.Remove(cfgPath); err != nil {
			t.Fatal(err)
		}
		writeFile(t, cfgPath, "config_version: 2\nagent:\n  id: 0190aaaa-bbbb-7ccc-8ddd-eeeeffff0001\nchannels:")
		if err := RejectLinkedConfig(cfgPath); err != nil {
			t.Fatalf("a regular config must pass: %v", err)
		}
	})
}

// Login remediation: a non-default agent carries its quoted --agent-dir so
// the hint never targets another agent; the default agent keeps the short form.
func TestLoginCommand(t *testing.T) {
	home := t.TempDir()
	withHome(t, home)
	defaultDir := filepath.Join(phys(t, home), dirNameReady)
	if err := os.MkdirAll(defaultDir, 0o700); err != nil {
		t.Fatal(err)
	}

	if got := LoginCommand(defaultDir, "mcp:srv"); got != "ready login mcp:srv" {
		t.Errorf("default agent: got %q", got)
	}
	if got := LoginCommand("", "srv"); got != "ready login srv" {
		t.Errorf("unknown agent dir: got %q", got)
	}

	other := filepath.Join(phys(t, t.TempDir()), "it's casa")
	got := LoginCommand(other, "mcp:srv --forget")
	if !strings.HasPrefix(got, "ready login mcp:srv --forget --agent-dir ") || !strings.Contains(got, "casa") {
		t.Errorf("explicit agent: got %q", got)
	}
	if got != "ready login mcp:srv --forget --agent-dir "+quoteAgentDir(other) {
		t.Errorf("explicit agent must reuse the serve quoting: got %q", got)
	}
}
