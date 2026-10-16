package twin

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rs/zerolog"
	"gopkg.in/yaml.v3"
)

// fakeInvalidator counts Invalidate calls for assertions.
type fakeInvalidator struct {
	calls   int
	names   []string
	evicted []string
}

func (f *fakeInvalidator) Invalidate(skillName string) {
	f.calls++
	f.names = append(f.names, skillName)
}

func (f *fakeInvalidator) Evict(path string) {
	f.evicted = append(f.evicted, path)
}

// extractFrontmatter parses the YAML frontmatter block (between the first two
// "---" delimiters) into a map for assertions.
func extractFrontmatter(t *testing.T, data []byte) map[string]any {
	t.Helper()

	parts := strings.SplitN(string(data), "---\n", 3)
	if len(parts) < 3 {
		t.Fatalf("expected a frontmatter block, got: %s", data)
	}

	var fm map[string]any
	if err := yaml.Unmarshal([]byte(parts[1]), &fm); err != nil {
		t.Fatalf("failed to parse frontmatter: %v", err)
	}

	return fm
}

func TestSnapshotter_WriteHAConfig_Frontmatter(t *testing.T) {
	tw, closer := newTestTwinReady(t)
	defer closer()

	dir := t.TempDir()
	generatedDir := filepath.Join(dir, "generated")
	if err := os.MkdirAll(generatedDir, 0o755); err != nil {
		t.Fatalf("setup: %v", err)
	}

	snap := NewSnapshotter(tw, dir, &fakeInvalidator{}, zerolog.Nop())
	if err := snap.writeHAConfig(generatedDir); err != nil {
		t.Fatalf("writeHAConfig: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(generatedDir, "ha-config.md"))
	if err != nil {
		t.Fatalf("read ha-config.md: %v", err)
	}

	fm := extractFrontmatter(t, data)

	if fm["generated"] != true {
		t.Errorf("expected generated: true, got %#v", fm["generated"])
	}
	if fm["generator"] != snapshotGenerator {
		t.Errorf("expected generator: %s, got %#v", snapshotGenerator, fm["generator"])
	}
	if fm["source"] != snapshotSource {
		t.Errorf("expected source: %s, got %#v", snapshotSource, fm["source"])
	}
	if _, ok := fm["generated_at"]; !ok {
		t.Error("expected generated_at to be present")
	}
	if _, ok := fm["inject"]; ok {
		t.Error("expected no inject key for ha-config.md (default true)")
	}
}

func TestSnapshotter_WriteDevices_FrontmatterPreservesInjectFalse(t *testing.T) {
	tw, closer := newTestTwinReady(t)
	defer closer()

	dir := t.TempDir()
	generatedDir := filepath.Join(dir, "generated")
	if err := os.MkdirAll(generatedDir, 0o755); err != nil {
		t.Fatalf("setup: %v", err)
	}

	snap := NewSnapshotter(tw, dir, &fakeInvalidator{}, zerolog.Nop())
	if err := snap.writeDevices(generatedDir); err != nil {
		t.Fatalf("writeDevices: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(generatedDir, "devices.md"))
	if err != nil {
		t.Fatalf("read devices.md: %v", err)
	}

	fm := extractFrontmatter(t, data)

	if fm["generated"] != true {
		t.Errorf("expected generated: true, got %#v", fm["generated"])
	}
	if fm["inject"] != false {
		t.Errorf("expected inject: false to be preserved, got %#v", fm["inject"])
	}
}

func TestSnapshotter_PartialWriteStillInvalidates(t *testing.T) {
	tw, closer := newTestTwinReady(t)
	defer closer()

	dir := t.TempDir()
	generatedDir := filepath.Join(dir, "generated")

	// Pre-create devices.md as a directory so os.WriteFile fails for it while
	// ha-config.md still succeeds — simulating a partial-write failure.
	if err := os.MkdirAll(filepath.Join(generatedDir, "devices.md"), 0o755); err != nil {
		t.Fatalf("setup: %v", err)
	}

	inv := &fakeInvalidator{}
	snap := NewSnapshotter(tw, dir, inv, zerolog.Nop())

	err := snap.generate()
	if err == nil {
		t.Fatal("expected an error from the partial write failure")
	}

	if inv.calls == 0 {
		t.Error("expected Invalidate to be called even on partial write failure")
	}

	if _, statErr := os.Stat(filepath.Join(generatedDir, "ha-config.md")); statErr != nil {
		t.Errorf("expected ha-config.md to have been written despite devices.md failing: %v", statErr)
	}
}

// TestSnapshotter_Generate_EvictsContentCacheForGeneratedAssets pins H8
// (criticalities-remediation): after every generate() the SkillInvalidator
// receives Evict for both generated asset paths — the skillcache does not
// watch generated files, so this is the only invalidation for their cached
// content.
func TestSnapshotter_Generate_EvictsContentCacheForGeneratedAssets(t *testing.T) {
	tw, closer := newTestTwinReady(t)
	defer closer()

	dir := t.TempDir()

	inv := &fakeInvalidator{}
	snap := NewSnapshotter(tw, dir, inv, zerolog.Nop())

	if err := snap.generate(); err != nil {
		t.Fatalf("generate: %v", err)
	}

	want := []string{
		filepath.Join(dir, "generated", "ha-config.md"),
		filepath.Join(dir, "generated", "devices.md"),
	}

	if len(inv.evicted) != len(want) {
		t.Fatalf("evicted = %v, want %v", inv.evicted, want)
	}

	for i, w := range want {
		if inv.evicted[i] != w {
			t.Errorf("evicted[%d] = %q, want %q", i, inv.evicted[i], w)
		}
	}
}
