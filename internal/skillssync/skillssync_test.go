package skillssync

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rs/zerolog"

	"github.com/marco-rozz/ready/internal/config"
)

type env struct {
	home    string // the runtime home (its skills/ is the runtime copy)
	runtime string
	library string
}

func newEnv(t *testing.T) env {
	t.Helper()

	home := t.TempDir()

	return env{
		home:    home,
		runtime: filepath.Join(home, "skills"),
		library: filepath.Join(t.TempDir(), "library", "skills"),
	}
}

func (e env) putLibrary(t *testing.T, rel, content string) {
	t.Helper()
	p := filepath.Join(e.library, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func (e env) putRuntime(t *testing.T, rel, content string) {
	t.Helper()
	p := filepath.Join(e.runtime, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// run executes one boot sync and returns the resulting state file entries
// (empty when none were written).
func (e env) run(t *testing.T, sync string) {
	t.Helper()

	e.runState(t, sync)
}

// runState runs one boot sync and returns the resulting state (for tests
// that assert on it directly).
func (e env) runState(t *testing.T, sync string) state {
	t.Helper()

	cfg := config.SkillsConfig{Path: e.runtime, Sync: sync, LibraryPath: e.library}
	if err := Run(t.Context(), cfg, e.home, zerolog.Nop()); err != nil {
		t.Fatalf("sync failed: %v", err)
	}

	return e.state(t)
}

func (e env) state(t *testing.T) state {
	t.Helper()

	st, err := readState(filepath.Join(e.runtime, stateFileName))
	if err != nil {
		t.Fatal(err)
	}

	return *st
}

func (e env) read(t *testing.T, rel string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(e.runtime, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatalf("runtime file %s: %v", rel, err)
	}

	return string(data)
}

func (e env) exists(rel string) bool {
	_, err := os.Stat(filepath.Join(e.runtime, filepath.FromSlash(rel)))

	return err == nil
}

// Row 1: absent | none | present → copy and record (first run populates).
func TestSync_FirstRunCopiesAndRecords(t *testing.T) {
	e := newEnv(t)
	e.putLibrary(t, "home-integration/SKILL.md", "v1")

	e.run(t, "auto")

	if got := e.read(t, "home-integration/SKILL.md"); got != "v1" {
		t.Errorf("runtime copy = %q, want the library content", got)
	}
	st := e.state(t)
	if _, ok := st.Files["home-integration/SKILL.md"]; !ok {
		t.Errorf("state file does not record the copied file: %+v", st.Files)
	}
}

// Row 2: untouched file receives a shipped fix (INFO + replace + record).
func TestSync_UntouchedFileReceivesFix(t *testing.T) {
	e := newEnv(t)
	e.putLibrary(t, "home-integration/SKILL.md", "v1")
	e.run(t, "auto")
	e.putLibrary(t, "home-integration/SKILL.md", "v2")

	e.run(t, "auto")

	if e.read(t, "home-integration/SKILL.md") != "v2" {
		t.Errorf("untouched runtime file was not updated from the library")
	}
}

// Row 3: equals state + unchanged library → nothing (idempotent run).
func TestSync_UnchangedLibraryDoesNothing(t *testing.T) {
	e := newEnv(t)
	e.putLibrary(t, "home-integration/SKILL.md", "v1")
	e.putRuntime(t, "generated/devices.md", "generated content")
	e.run(t, "auto")

	e.run(t, "auto")

	if e.read(t, "home-integration/SKILL.md") != "v1" || !e.exists("generated/devices.md") {
		t.Errorf("idempotent run disturbed the runtime copy")
	}
}

// Row 4: edited file survives + WARN (runtime differs from state, library
// changed too).
func TestSync_EditedFileSurvivesConflict(t *testing.T) {
	e := newEnv(t)
	e.putLibrary(t, "home-topology/assets/custom-notes.md", "shipped v1")
	e.run(t, "auto")
	e.putRuntime(t, "home-topology/assets/custom-notes.md", "operator edit")
	e.putLibrary(t, "home-topology/assets/custom-notes.md", "shipped v2")

	e.run(t, "auto")

	if e.read(t, "home-topology/assets/custom-notes.md") != "operator edit" {
		t.Errorf("operator edit was overwritten by the sync")
	}
}

// Per-file granularity: the edited file survives while its untouched sibling
// in the same skill receives the shipped fix.
func TestSync_PerFileGranularity(t *testing.T) {
	e := newEnv(t)
	e.putLibrary(t, "home-topology/SKILL.md", "skill v1")
	e.putLibrary(t, "home-topology/assets/custom-notes.md", "notes v1")
	e.run(t, "auto")
	e.putRuntime(t, "home-topology/assets/custom-notes.md", "operator edit")
	e.putLibrary(t, "home-topology/SKILL.md", "skill v2")
	e.putLibrary(t, "home-topology/assets/custom-notes.md", "notes v2")

	e.run(t, "auto")

	if e.read(t, "home-topology/SKILL.md") != "skill v2" {
		t.Errorf("untouched SKILL.md was not updated while a sibling was edited")
	}
	if e.read(t, "home-topology/assets/custom-notes.md") != "operator edit" {
		t.Errorf("operator-edited asset was overwritten by the sync")
	}
}

// Row 5: a deleted runtime file is respected, never resurrected — even when
// the library later changes it. State entry removal would resurrect it on the
// next boot, so the entry must persist.
func TestSync_DeletedFileNotResurrected(t *testing.T) {
	e := newEnv(t)
	e.putLibrary(t, "grill-me/SKILL.md", "v1")
	e.run(t, "auto")
	if err := os.Remove(filepath.Join(e.runtime, "grill-me", "SKILL.md")); err != nil {
		t.Fatal(err)
	}
	e.putLibrary(t, "grill-me/SKILL.md", "v2")

	e.run(t, "auto")

	if e.exists("grill-me/SKILL.md") {
		t.Errorf("the operator's deletion was resurrected by the sync")
	}
	// And again one boot later (a state cleanup would make this fail).
	e.run(t, "auto")
	if e.exists("grill-me/SKILL.md") {
		t.Errorf("the operator's deletion was resurrected by a later sync")
	}
}

// Row 6: a runtime file with no recorded state (operator's own or
// pre-existing) is untouched and stays unrecorded.
func TestSync_OperatorAddedFileLeftUnrecorded(t *testing.T) {
	e := newEnv(t)
	e.putRuntime(t, "personal/my-skill/SKILL.md", "operator's own skill")
	e.putLibrary(t, "home-integration/SKILL.md", "v1")

	e.run(t, "auto")

	if e.read(t, "personal/my-skill/SKILL.md") != "operator's own skill" {
		t.Errorf("operator-added file was disturbed")
	}
	st := e.state(t)
	for rel := range st.Files {
		if strings.HasPrefix(rel, "personal/") {
			t.Errorf("operator-added file gained a state entry: %s", rel)
		}
	}
}

// Convergence: a promoted experiment (runtime file already equal to the
// library file) records the new hash without raising a conflict.
func TestSync_PromotedExperimentConverges(t *testing.T) {
	e := newEnv(t)
	e.putLibrary(t, "home-integration/SKILL.md", "v1")
	e.run(t, "auto")
	e.putRuntime(t, "home-integration/SKILL.md", "my edit, later promoted")
	if err := os.WriteFile(filepath.Join(e.runtime, stateFileName), []byte(`{"files":{"home-integration/SKILL.md":"stale-state-hash"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	e.putLibrary(t, "home-integration/SKILL.md", "my edit, later promoted")

	e.run(t, "auto")

	if e.read(t, "home-integration/SKILL.md") != "my edit, later promoted" {
		t.Errorf("promoted file was disturbed")
	}
}

// Row 7: a recorded file the new library no longer ships is reported and left.
func TestSync_RemovedLibraryFileReportedAsOrphan(t *testing.T) {
	e := newEnv(t)
	e.putLibrary(t, "old-skill/SKILL.md", "v1")
	e.run(t, "auto")
	if err := os.RemoveAll(filepath.Join(e.library, "old-skill")); err != nil {
		t.Fatal(err)
	}
	e.putLibrary(t, "home-integration/SKILL.md", "v1")

	e.run(t, "auto")

	if !e.exists("old-skill/SKILL.md") {
		t.Errorf("the sync deleted a runtime file the library no longer ships — rm is the operator's act")
	}
}

// generated/ content never acquires a state entry — a global run with a
// library carrying generated content keeps the snapshotter's authority
// intact (and the generated runtime copy untouched).
func TestSync_GeneratedNeverRecorded(t *testing.T) {
	e := newEnv(t)
	e.putLibrary(t, "home-integration/SKILL.md", "v1")
	e.run(t, "auto")

	e.putLibrary(t, "home-topology/generated/devices.md", "shipped snapshot")
	e.run(t, "auto")

	if e.exists("home-topology/generated/devices.md") {
		t.Errorf("the sync copied machine-generated content out of the library's authority")
	}
	st := e.state(t)
	for rel := range st.Files {
		if isGenerated(rel) {
			t.Errorf("a generated path gained a state entry: %s", rel)
		}
	}
}

// notify mode applies none of the updates and writes nothing (state file
// included). A pending fresh copy (row 1) is likewise reported, not applied.
func TestSync_NotifyWritesNothing(t *testing.T) {
	e := newEnv(t)
	e.putLibrary(t, "home-integration/SKILL.md", "v1")
	e.run(t, "auto")
	e.putLibrary(t, "home-integration/SKILL.md", "v2")
	e.putLibrary(t, "new-skill/SKILL.md", "brand new")

	e.run(t, "notify")

	if e.read(t, "home-integration/SKILL.md") != "v1" {
		t.Errorf("notify mode applied a library update")
	}
	if e.exists("new-skill/SKILL.md") {
		t.Errorf("notify mode copied a pending library file")
	}
	data, err := os.ReadFile(filepath.Join(e.runtime, stateFileName))
	if err != nil {
		t.Fatal(err)
	}
	stateHash := sumHex([]byte("v2"))
	if len(stateHash) != 64 {
		t.Fatalf("sha256 hex length wrong")
	}
	if strings.Contains(string(data), stateHash) {
		t.Errorf("notify mode wrote the updated hash into the state file")
	}
}

func sumHex(b []byte) string {
	sum := sha256.Sum256(b)

	return hex.EncodeToString(sum[:])
}

// off mode skips the sync entirely (a fresh runtime copy stays empty).
func TestSync_OffSkipsEverything(t *testing.T) {
	e := newEnv(t)
	e.putLibrary(t, "home-integration/SKILL.md", "v1")

	e.run(t, "off")

	if e.exists("home-integration/SKILL.md") {
		t.Errorf("off mode still copied library files")
	}
	if _, err := os.Stat(filepath.Join(e.runtime, stateFileName)); !os.IsNotExist(err) {
		t.Errorf("off mode wrote a state file")
	}
}

// An absent library is a silent no-op even under auto.
func TestSync_AbsentLibraryIsNoOp(t *testing.T) {
	e := newEnv(t)

	if err := Run(t.Context(), config.SkillsConfig{Path: e.runtime, Sync: "auto", LibraryPath: e.library}, e.home, zerolog.Nop()); err != nil {
		t.Fatalf("absent library must be a no-op, got %v", err)
	}
	if _, err := os.Stat(e.runtime); !os.IsNotExist(err) {
		t.Errorf("the sync created the runtime skills directory")
	}
}

// An unresolvable library location (empty) behaves the same.
func TestSync_EmptyLibraryLocationIsNoOp(t *testing.T) {
	e := newEnv(t)
	if err := Run(t.Context(), config.SkillsConfig{Path: e.runtime, Sync: "auto", LibraryPath: ""}, e.home, zerolog.Nop()); err != nil {
		t.Fatalf("empty library location must be a no-op, got %v", err)
	}
}
