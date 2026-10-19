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

// Chain precedence: flag > local > global.
func TestElect_ChainPrecedence(t *testing.T) {
	t.Run("explicit flag wins and never falls back", func(t *testing.T) {
		home := t.TempDir()
		withHome(t, home)
		// Local AND global candidates exist: the flag must win and the
		// missing-flag failure never applies.
		writeFile(t, filepath.Join(".ready", "config.yaml"), "config_version: 1\nchannels:")
		writeFile(t, filepath.Join(home, ".ready", "config.yaml"), "config_version: 1\nchannels:")

		res, err := Elect(context.Background(), "/custom/config.yaml")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if res.Path != "/custom/config.yaml" || res.Origin != OriginFlag {
			t.Errorf("resolution = %+v, want the flagged path with origin flag", res)
		}
	})

	t.Run("local preferred over global", func(t *testing.T) {
		home := t.TempDir()
		withHome(t, home)
		wd, err := os.Getwd()
		if err != nil {
			t.Fatal(err)
		}
		writeFile(t, filepath.Join(wd, ".ready", "config.yaml"), "config_version: 1\nchannels:")
		writeFile(t, filepath.Join(home, ".ready", "config.yaml"), "config_version: 1\nchannels:")
		defer os.Remove(filepath.Join(wd, ".ready", "config.yaml"))

		res, err := Elect(context.Background(), "")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if res.Origin != OriginLocal {
			t.Errorf("origin = %q, want local", res.Origin)
		}
	})

	t.Run("global fallback when no local config", func(t *testing.T) {
		home := t.TempDir()
		withHome(t, home)
		writeFile(t, filepath.Join(home, ".ready", "config.yaml"), "config_version: 1\nchannels:")

		res, err := Elect(context.Background(), "")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if res.Origin != OriginGlobal {
			t.Errorf("origin = %q, want global", res.Origin)
		}
		if res.Path != filepath.Join(home, ".ready", "config.yaml") {
			t.Errorf("path = %q, want the global candidate", res.Path)
		}
	})

	t.Run("no config at all names both candidates", func(t *testing.T) {
		home := t.TempDir()
		withHome(t, home)

		_, err := Elect(context.Background(), "")
		if err == nil {
			t.Fatal("expected an error")
		}
		local := "./" + LocalConfigRel
		global := filepath.Join(home, ".ready", "config.yaml")
		if !strings.Contains(err.Error(), local) || !strings.Contains(err.Error(), global) {
			t.Errorf("error = %v, want both candidate paths %q and %q", err, local, global)
		}
	})
}

// The unresolvable-user-home precision cases.
func TestElect_HomeFailures(t *testing.T) {
	t.Run("flag with no home still elects", func(t *testing.T) {
		os.Unsetenv("HOME")
		t.Cleanup(func() { _ = os.Setenv("HOME", os.Getenv("XDG_TEST_BACKUP_HOME")) })

		res, err := Elect(context.Background(), "/etc/ready/config.yaml")
		if err != nil {
			t.Fatalf("explicit --config must elect without a user home, got %v", err)
		}
		if res.Origin != OriginFlag {
			t.Errorf("origin = %q, want flag", res.Origin)
		}
	})

	t.Run("chain without home names the problem", func(t *testing.T) {
		os.Unsetenv("HOME")

		_, err := Elect(context.Background(), "")
		if err == nil {
			t.Fatal("expected the undeterminable-home error")
		}
		if !strings.Contains(err.Error(), "undeterminable") {
			t.Errorf("error = %v, want the undeterminable-user-home wording", err)
		}
		if !strings.Contains(err.Error(), ".ready/config.yaml") {
			t.Errorf("error = %v, want the searched local candidate named", err)
		}
	})
}

// Runtime-home derivation: the elected config's directory becomes the root of
// every runtime path default.
func TestApplyRuntimeHome_DerivesRuntimePaths(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	writeFile(t, cfgPath, "config_version: 1\nchannels:")

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
config_version: 1
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

// OAuth store anchoring: the default lands in the Install home,
// an explicit relative path against the runtime home, an absolute one as-is.
func TestApplyRuntimeHome_OAuthStoreAnchoring(t *testing.T) {
	newYAML := func(oauthPath string) string {
		return `
config_version: 1
llm:
  default: "codex::gpt-5"
  providers:
    codex:
      provider: openai-codex
      api: responses
oauth:
  token_store: file
` + oauthPath
	}

	codexTarget := func(t *testing.T, yaml string) string {
		t.Helper()

		dir := t.TempDir()
		cfgPath := filepath.Join(dir, "config.yaml")
		writeFile(t, cfgPath, yaml)

		return cfgPath
	}

	t.Run("default anchors to the Install home", func(t *testing.T) {
		home := t.TempDir()
		withHome(t, home)
		cfgPath := codexTarget(t, newYAML(""))

		cfg, err := Load(context.Background(), cfgPath)
		if err != nil {
			t.Fatal(err)
		}
		if want := filepath.Join(home, ".ready", "oauth"); cfg.OAuth.Path != want {
			t.Errorf("OAuth.Path = %q, want the Install-home default %q", cfg.OAuth.Path, want)
		}
		if cfg.LLM.OAuthPath != cfg.OAuth.Path {
			t.Errorf("LLM.OAuthPath diverged from the store: %q vs %q — daemon and login would resolve different stores", cfg.LLM.OAuthPath, cfg.OAuth.Path)
		}
	})

	t.Run("explicit relative follows the runtime home", func(t *testing.T) {
		cfgPath := codexTarget(t, newYAML("  path: secrets/oauth"))

		cfg, err := Load(context.Background(), cfgPath)
		if err != nil {
			t.Fatal(err)
		}
		if want := filepath.Join(filepath.Dir(cfgPath), "secrets", "oauth"); cfg.OAuth.Path != want {
			t.Errorf("OAuth.Path = %q, want %q", cfg.OAuth.Path, want)
		}
	})

	t.Run("explicit absolute wins as-is", func(t *testing.T) {
		abs := t.TempDir()
		cfgPath := codexTarget(t, newYAML("  path: "+abs))

		cfg, err := Load(context.Background(), cfgPath)
		if err != nil {
			t.Fatal(err)
		}
		if cfg.OAuth.Path != abs {
			t.Errorf("OAuth.Path = %q, want %q", cfg.OAuth.Path, abs)
		}
	})

	t.Run("no OAuth targets needs no home", func(t *testing.T) {
		home := t.TempDir()
		withHome(t, home)
		writeFile(t, filepath.Join(home, ".ready", "nonexistent"), "")

		yaml := `
config_version: 1
channels:
  cli:
    enabled: true
`
		cfgPath := filepath.Join(home, ".ready", "config.yaml")
		writeFile(t, cfgPath, yaml)

		cfg, err := Load(context.Background(), cfgPath)
		if err != nil {
			t.Fatalf("a config without OAuth targets must not require the Install home: %v", err)
		}
		if cfg.OAuth.Path != "" {
			t.Errorf("OAuth.Path = %q, want empty (no consumer)", cfg.OAuth.Path)
		}
	})

	t.Run("OAuth default with no home fails naming oauth.path", func(t *testing.T) {
		dir := t.TempDir()
		cfgPath := filepath.Join(dir, "config.yaml")
		writeFile(t, cfgPath, newYAML(""))
		withHome(t, "")
		os.Unsetenv("HOME")

		_, err := Load(context.Background(), cfgPath)
		if err == nil {
			t.Fatal("expected the oauth.path remedy error")
		}
		if !strings.Contains(err.Error(), "oauth.path") {
			t.Errorf("error = %v, want oauth.path as the remedy", err)
		}
	})
}

// skills.library_path: absolute as-is, relative against the runtime home,
// default <Install home>/library/skills.
func TestSkillsLibraryPath(t *testing.T) {
	t.Run("default Install home", func(t *testing.T) {
		home := t.TempDir()
		withHome(t, home)
		writeFile(t, filepath.Join(home, ".ready", "config.yaml"), "config_version: 1\nchannels:\n  cli:\n    enabled: true\n")

		cfg, err := Load(context.Background(), filepath.Join(home, ".ready", "config.yaml"))
		if err != nil {
			t.Fatal(err)
		}
		if want := filepath.Join(home, ".ready", "library", "skills"); cfg.Skills.LibraryPath != want {
			t.Errorf("LibraryPath = %q, want %q", cfg.Skills.LibraryPath, want)
		}
	})

	t.Run("relative override against the runtime home", func(t *testing.T) {
		dir := t.TempDir()
		writeFile(t, filepath.Join(dir, "config.yaml"), "config_version: 1\nskills:\n  library_path: ../resources/skills/\n")

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
	writeFile(t, filepath.Join(dir, "config.yaml"), "config_version: 1\nskills:\n  sync: sometimes\n")

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
	writeFileT(t, cfgPath, "config_version: 1\nchannels:\n  cli:\n    enabled: true\n")

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
