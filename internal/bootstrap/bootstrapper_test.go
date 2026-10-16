package bootstrap_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	agentskillsSpec "github.com/flexigpt/agentskills-go/runtime/spec"
	"github.com/marco-rozz/ready/internal/bootstrap"
	"github.com/marco-rozz/ready/internal/config"
	"github.com/marco-rozz/ready/internal/skills"
	"github.com/rs/zerolog"
)

// directLoader satisfies bootstrap.ContentLoader by reading files directly from
// disk (no caching). Used in tests where fsnotify setup is unnecessary.
type directLoader struct{}

func (d directLoader) Load(_ context.Context, path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}

	return string(data), nil
}

// --- helpers -----------------------------------------------------------------

func newTestSkillsMgr(t *testing.T, path string) *skills.Manager {
	t.Helper()

	mgr, err := skills.NewManager(context.Background(), &config.SkillsConfig{
		Path:      path,
		HotReload: false,
	}, zerolog.Nop())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	t.Cleanup(func() { _ = mgr.Close() })

	return mgr
}

// createSkillDir creates a skill directory with a valid SKILL.md (agentskills
// frontmatter format) and optional additional asset files.
// The bodyContent is appended after the frontmatter so it appears in the static shell.
func createSkillDir(t *testing.T, base, name, bodyContent string, assets map[string]string) {
	t.Helper()

	dir := filepath.Join(base, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	// agentskills-go requires frontmatter with name/description/version.
	skillMD := "---\nname: " + name + "\ndescription: \"" + name + " skill\"\nversion: \"1.0\"\n---\n\n" + bodyContent

	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(skillMD), 0o600); err != nil {
		t.Fatalf("WriteFile SKILL.md: %v", err)
	}

	for relPath, content := range assets {
		full := filepath.Join(dir, relPath)
		if mkErr := os.MkdirAll(filepath.Dir(full), 0o755); mkErr != nil {
			t.Fatalf("MkdirAll asset dir: %v", mkErr)
		}

		if err := os.WriteFile(full, []byte(content), 0o600); err != nil {
			t.Fatalf("WriteFile asset %s: %v", relPath, err)
		}
	}
}

// --- constants ---------------------------------------------------------------

const (
	testChannelCLI    = "cli"
	testSkillTopology = "home-topology"
)

// --- tests -------------------------------------------------------------------

func TestBootstrapper_GetOrCreateSession_CreatesOnFirstCall(t *testing.T) {
	tmpDir := t.TempDir()
	createSkillDir(t, tmpDir, "home-topology", "# Home Topology\nRooms: kitchen, living room.", nil)

	mgr := newTestSkillsMgr(t, tmpDir)
	b := bootstrap.New(mgr, directLoader{}, map[string][]string{testChannelCLI: {testSkillTopology}}, zerolog.Nop())
	t.Cleanup(func() { _ = b.CloseAll(context.Background()) })

	ctx := context.Background()
	sid1, err := b.GetOrCreateSession(ctx, testChannelCLI, testChannelCLI)
	if err != nil {
		t.Fatalf("GetOrCreateSession: %v", err)
	}

	if sid1 == "" {
		t.Fatal("expected non-empty session ID")
	}

	// Second call must return the same session ID.
	sid2, err := b.GetOrCreateSession(ctx, testChannelCLI, testChannelCLI)
	if err != nil {
		t.Fatalf("GetOrCreateSession (2nd): %v", err)
	}

	if sid1 != sid2 {
		t.Errorf("expected same session ID on second call: got %q then %q", sid1, sid2)
	}
}

func TestBootstrapper_GetOrCreateSession_IsolatesChats(t *testing.T) {
	tmpDir := t.TempDir()
	createSkillDir(t, tmpDir, "home-topology", "# Home Topology", nil)

	mgr := newTestSkillsMgr(t, tmpDir)
	b := bootstrap.New(mgr, directLoader{}, map[string][]string{"telegram": {testSkillTopology}}, zerolog.Nop())
	t.Cleanup(func() { _ = b.CloseAll(context.Background()) })

	ctx := context.Background()
	sid1, _ := b.GetOrCreateSession(ctx, "telegram-111", "telegram")
	sid2, _ := b.GetOrCreateSession(ctx, "telegram-222", "telegram")

	if sid1 == sid2 {
		t.Error("different chat keys must produce different session IDs")
	}
}

func TestBootstrapper_StaticShell_CacheHit(t *testing.T) {
	tmpDir := t.TempDir()
	createSkillDir(t, tmpDir, "home-topology", "# Topology\nContent here.", map[string]string{
		"assets/devices.md": "## Devices\nLight: kitchen",
	})

	mgr := newTestSkillsMgr(t, tmpDir)
	b := bootstrap.New(mgr, directLoader{}, map[string][]string{testChannelCLI: {testSkillTopology}}, zerolog.Nop())
	t.Cleanup(func() { _ = b.CloseAll(context.Background()) })

	ctx := context.Background()
	_, _ = b.GetOrCreateSession(ctx, testChannelCLI, testChannelCLI)

	shell1, _ := b.StaticShell(ctx, testChannelCLI, testChannelCLI)
	if shell1 == "" {
		t.Fatal("expected non-empty static shell")
	}

	if !strings.Contains(shell1, "Topology") {
		t.Error("static shell should contain SKILL.md content")
	}

	if !strings.Contains(shell1, "Devices") {
		t.Error("static shell should contain asset content")
	}

	// Second call must return identical string (cache hit).
	shell2, _ := b.StaticShell(ctx, testChannelCLI, testChannelCLI)
	if shell1 != shell2 {
		t.Error("expected cache hit: shells should be identical")
	}
}

func TestBootstrapper_StaticShell_NestedAssets(t *testing.T) {
	tmpDir := t.TempDir()
	createSkillDir(t, tmpDir, "home-topology", "# Topology", map[string]string{
		"assets/devices.md":       "## Devices",
		"assets/rooms/kitchen.md": "Kitchen info",
		"assets/rooms/bedroom.md": "Bedroom info",
	})

	mgr := newTestSkillsMgr(t, tmpDir)
	b := bootstrap.New(mgr, directLoader{}, map[string][]string{testChannelCLI: {testSkillTopology}}, zerolog.Nop())
	t.Cleanup(func() { _ = b.CloseAll(context.Background()) })

	ctx := context.Background()
	_, _ = b.GetOrCreateSession(ctx, testChannelCLI, testChannelCLI)

	shell, _ := b.StaticShell(ctx, testChannelCLI, testChannelCLI)

	for _, want := range []string{"Devices", "Kitchen info", "Bedroom info"} {
		if !strings.Contains(shell, want) {
			t.Errorf("static shell should contain %q", want)
		}
	}
}

func TestBootstrapper_StaticShell_ExcludesDynamicAssets(t *testing.T) {
	dynamic := "---\nsource: dynamic\nhandler: ha-device-list\n---\n"
	tmpDir := t.TempDir()
	createSkillDir(t, tmpDir, "home-topology", "# Topology", map[string]string{
		"assets/static.md":  "Static content",
		"assets/dynamic.md": dynamic + "Dynamic content",
	})

	mgr := newTestSkillsMgr(t, tmpDir)
	b := bootstrap.New(mgr, directLoader{}, map[string][]string{testChannelCLI: {testSkillTopology}}, zerolog.Nop())
	t.Cleanup(func() { _ = b.CloseAll(context.Background()) })

	ctx := context.Background()
	_, _ = b.GetOrCreateSession(ctx, testChannelCLI, testChannelCLI)

	shell, _ := b.StaticShell(ctx, testChannelCLI, testChannelCLI)

	if !strings.Contains(shell, "Static content") {
		t.Error("static content should be in shell")
	}

	if strings.Contains(shell, "Dynamic content") {
		t.Error("dynamic asset content must NOT appear in static shell")
	}
}

func TestBootstrapper_Invalidate_ClearsCache(t *testing.T) {
	tmpDir := t.TempDir()
	createSkillDir(t, tmpDir, "home-topology", "# Topology v1", nil)

	mgr := newTestSkillsMgr(t, tmpDir)
	b := bootstrap.New(mgr, directLoader{}, map[string][]string{testChannelCLI: {testSkillTopology}}, zerolog.Nop())
	t.Cleanup(func() { _ = b.CloseAll(context.Background()) })

	ctx := context.Background()
	_, _ = b.GetOrCreateSession(ctx, testChannelCLI, testChannelCLI)
	_, _ = b.StaticShell(ctx, testChannelCLI, testChannelCLI) // populate cache

	// Invalidate should clear the static cache.
	b.Invalidate("home-topology")

	// After invalidation the shell should be rebuilt on next call.
	// Update the file content to verify a rebuild happens.
	if err := os.WriteFile(filepath.Join(tmpDir, "home-topology", "SKILL.md"), []byte("# Topology v2"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	shell, _ := b.StaticShell(ctx, testChannelCLI, testChannelCLI)
	if !strings.Contains(shell, "Topology v2") {
		t.Errorf("expected rebuilt shell with v2 content, got: %q", shell)
	}
}

func TestBootstrapper_Invalidate_NoopForNonBootstrappedSkill(t *testing.T) {
	tmpDir := t.TempDir()
	createSkillDir(t, tmpDir, "home-topology", "# Topology", nil)

	mgr := newTestSkillsMgr(t, tmpDir)
	b := bootstrap.New(mgr, directLoader{}, map[string][]string{testChannelCLI: {testSkillTopology}}, zerolog.Nop())
	t.Cleanup(func() { _ = b.CloseAll(context.Background()) })

	ctx := context.Background()
	_, _ = b.GetOrCreateSession(ctx, testChannelCLI, testChannelCLI)
	shell1, _ := b.StaticShell(ctx, testChannelCLI, testChannelCLI)

	// Invalidate a skill that is NOT bootstrapped — cache must survive.
	b.Invalidate("petscii-formatter")

	shell2, _ := b.StaticShell(ctx, testChannelCLI, testChannelCLI)
	if shell1 != shell2 {
		t.Error("cache should be intact after invalidating non-bootstrapped skill")
	}
}

func TestBootstrapper_DynamicContent_EmptyWhenAllStubs(t *testing.T) {
	tmpDir := t.TempDir()
	dynamic := "---\nsource: dynamic\nhandler: ha-device-list\n---\n"
	createSkillDir(t, tmpDir, "home-topology", "# Topology", map[string]string{
		"assets/live.md": dynamic,
	})

	mgr := newTestSkillsMgr(t, tmpDir)
	b := bootstrap.New(mgr, directLoader{}, map[string][]string{testChannelCLI: {testSkillTopology}}, zerolog.Nop())
	t.Cleanup(func() { _ = b.CloseAll(context.Background()) })

	ctx := context.Background()
	_, _ = b.GetOrCreateSession(ctx, testChannelCLI, testChannelCLI)
	_, _ = b.StaticShell(ctx, testChannelCLI, testChannelCLI) // discover dynamic pointers

	dyn := b.DynamicContent(ctx, testChannelCLI)
	if dyn != "" {
		t.Errorf("expected empty dynamic content from no-op stub, got: %q", dyn)
	}
}

func TestBootstrapper_DynamicContent_NonEmptyHandler(t *testing.T) {
	tmpDir := t.TempDir()
	dynamic := "---\nsource: dynamic\nhandler: test-handler\n---\n"
	createSkillDir(t, tmpDir, "home-topology", "# Topology", map[string]string{
		"assets/live.md": dynamic,
	})

	mgr := newTestSkillsMgr(t, tmpDir)
	b := bootstrap.New(mgr, directLoader{}, map[string][]string{testChannelCLI: {testSkillTopology}}, zerolog.Nop())
	t.Cleanup(func() { _ = b.CloseAll(context.Background()) })

	b.RegisterHandler("test-handler", &fixedHandler{content: "## Live State\nLight: ON"})

	ctx := context.Background()
	_, _ = b.GetOrCreateSession(ctx, testChannelCLI, testChannelCLI)
	_, _ = b.StaticShell(ctx, testChannelCLI, testChannelCLI)

	dyn := b.DynamicContent(ctx, testChannelCLI)
	if !strings.Contains(dyn, "Live State") {
		t.Errorf("expected live state from handler, got: %q", dyn)
	}
}

func TestBootstrapper_CloseAll(t *testing.T) {
	tmpDir := t.TempDir()
	createSkillDir(t, tmpDir, "home-topology", "# Topology", nil)

	mgr := newTestSkillsMgr(t, tmpDir)
	b := bootstrap.New(mgr, directLoader{}, map[string][]string{testChannelCLI: {testSkillTopology}}, zerolog.Nop())

	ctx := context.Background()
	_, _ = b.GetOrCreateSession(ctx, testChannelCLI, testChannelCLI)

	if err := b.CloseAll(ctx); err != nil {
		t.Fatalf("CloseAll: %v", err)
	}

	// After CloseAll, requesting a session must create a new one.
	sid, err := b.GetOrCreateSession(ctx, testChannelCLI, testChannelCLI)
	if err != nil {
		t.Fatalf("GetOrCreateSession after CloseAll: %v", err)
	}

	if sid == "" {
		t.Error("expected valid session after CloseAll")
	}
}

// TestBootstrapper_GetMetadataPrompt_FiltersBootstrappedSkill verifies that
// GetMetadataPrompt excludes the bootstrapped skill from the "Available Skills"
// listing once it has been loaded (marked Active) in the session.
func TestBootstrapper_GetMetadataPrompt_FiltersBootstrappedSkill(t *testing.T) {
	tmpDir := t.TempDir()
	createSkillDir(t, tmpDir, "home-topology", "# Home Topology\nRooms: kitchen.", nil)
	createSkillDir(t, tmpDir, "other-skill", "# Other Skill\nSome other capability.", nil)

	mgr := newTestSkillsMgr(t, tmpDir)
	b := bootstrap.New(mgr, directLoader{}, map[string][]string{testChannelCLI: {testSkillTopology}}, zerolog.Nop())
	t.Cleanup(func() { _ = b.CloseAll(context.Background()) })

	ctx := context.Background()
	sid, err := b.GetOrCreateSession(ctx, testChannelCLI, testChannelCLI)
	if err != nil {
		t.Fatalf("GetOrCreateSession: %v", err)
	}

	meta, err := mgr.GetMetadataPrompt(ctx, sid)
	if err != nil {
		t.Fatalf("GetMetadataPrompt: %v", err)
	}

	// home-topology was bootstrapped (Active) → must not appear.
	if strings.Contains(meta, "home-topology") {
		t.Errorf("bootstrapped skill 'home-topology' should be excluded from metadata prompt, got:\n%s", meta)
	}

	// other-skill is still inactive → must appear.
	if !strings.Contains(meta, "other-skill") {
		t.Errorf("non-bootstrapped skill 'other-skill' should appear in metadata prompt, got:\n%s", meta)
	}
}

// TestBootstrapper_NoBuildOnChannelWithoutBootstrap verifies that a channel
// with no bootstrap config returns an empty static shell.
func TestBootstrapper_NoBuildOnChannelWithoutBootstrap(t *testing.T) {
	tmpDir := t.TempDir()
	createSkillDir(t, tmpDir, "home-topology", "# Topology", nil)

	mgr := newTestSkillsMgr(t, tmpDir)
	// "cli" has no bootstrap skills configured.
	b := bootstrap.New(mgr, directLoader{}, map[string][]string{}, zerolog.Nop())
	t.Cleanup(func() { _ = b.CloseAll(context.Background()) })

	ctx := context.Background()
	_, _ = b.GetOrCreateSession(ctx, testChannelCLI, testChannelCLI)

	shell, _ := b.StaticShell(ctx, testChannelCLI, testChannelCLI)
	if shell != "" {
		t.Errorf("expected empty static shell for channel with no bootstrap config, got: %q", shell)
	}
}

// --- test helpers ------------------------------------------------------------

// fixedHandler always returns the same content.
type fixedHandler struct{ content string }

func (h *fixedHandler) Generate(_ context.Context) (string, error) {
	return h.content, nil
}

// Compile-time check that agentskills.SessionID is a string type (used in tests above).
var _ agentskillsSpec.SessionID

// session-lifecycle 4.2: closing a Session's agentskills session drops the
// skills it loaded; the next Session starts fresh with only the bootstrap
// skills Active.
func TestBootstrapper_Close_NextSessionStartsFresh(t *testing.T) {
	tmpDir := t.TempDir()
	createSkillDir(t, tmpDir, "home-topology", "# Home Topology\nRooms: kitchen.", nil)
	createSkillDir(t, tmpDir, "home-provisioning", "# Provisioning\nSteps.", nil)

	mgr := newTestSkillsMgr(t, tmpDir)
	b := bootstrap.New(mgr, directLoader{}, map[string][]string{testChannelCLI: {testSkillTopology}}, zerolog.Nop())
	t.Cleanup(func() { _ = b.CloseAll(context.Background()) })

	ctx := context.Background()

	sid1, err := b.GetOrCreateSession(ctx, "session-1", testChannelCLI)
	if err != nil {
		t.Fatalf("GetOrCreateSession: %v", err)
	}

	if _, loadErr := mgr.LoadSkill(ctx, sid1, "home-provisioning"); loadErr != nil {
		t.Fatalf("LoadSkill: %v", loadErr)
	}

	if closeErr := b.Close(ctx, "session-1"); closeErr != nil {
		t.Fatalf("Close: %v", closeErr)
	}

	sid2, err := b.GetOrCreateSession(ctx, "session-2", testChannelCLI)
	if err != nil {
		t.Fatalf("GetOrCreateSession: %v", err)
	}

	active, err := mgr.ActiveSkills(ctx, sid2)
	if err != nil {
		t.Fatalf("ActiveSkills: %v", err)
	}

	if len(active) != 1 || active[0] != testSkillTopology {
		t.Errorf("active skills of the new session = %v, want only the bootstrap skill", active)
	}

	if err := b.Close(ctx, "unknown"); err != nil {
		t.Errorf("closing an unknown key must be a no-op, got %v", err)
	}
}
