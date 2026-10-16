package skillcache

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// waitForEvent polls fn until it returns true or the timeout expires.
// Used to wait for fsnotify events without hard-coding a fixed sleep.
func waitForEvent(t *testing.T, fn func() bool) bool {
	t.Helper()

	const eventTimeout = 2 * time.Second

	deadline := time.Now().Add(eventTimeout)
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()

	for time.Now().Before(deadline) {
		if fn() {
			return true
		}

		<-tick.C
	}

	return false
}

// TestSkillCache_EmptyPath verifies that Load("") returns ("", nil).
func TestSkillCache_EmptyPath(t *testing.T) {
	sc, err := New()
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}
	defer func() { _ = sc.Close() }()

	content, err := sc.Load(context.Background(), "")
	if err != nil {
		t.Fatalf("Load(\"\") returned unexpected error: %v", err)
	}

	if content != "" {
		t.Errorf("Load(\"\") = %q; want empty string", content)
	}
}

// TestSkillCache_ColdLoad verifies that the first Load reads from the filesystem.
func TestSkillCache_ColdLoad(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "SKILL.md")

	if err := os.WriteFile(path, []byte("cold content"), 0o600); err != nil {
		t.Fatalf("failed to write file: %v", err)
	}

	sc, err := New()
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}
	defer func() { _ = sc.Close() }()

	got, err := sc.Load(context.Background(), path)
	if err != nil {
		t.Fatalf("Load() returned unexpected error: %v", err)
	}

	if got != "cold content" {
		t.Errorf("Load() = %q; want %q", got, "cold content")
	}
}

// TestSkillCache_WarmLoad verifies that subsequent Load calls return the cached
// value without reading from the filesystem again.
// Strategy: after the cold load, remove the file from disk; a warm hit should
// return the cached content without an error (the file is gone, so a fresh read
// would fail).  The fsnotify Remove event is delivered asynchronously, so the
// cache entry is still present immediately after os.Remove.
func TestSkillCache_WarmLoad(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "SKILL.md")

	if err := os.WriteFile(path, []byte("warm content"), 0o600); err != nil {
		t.Fatalf("failed to write file: %v", err)
	}

	sc, err := New()
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}
	defer func() { _ = sc.Close() }()

	// Cold load — populates the cache.
	if _, err = sc.Load(context.Background(), path); err != nil {
		t.Fatalf("cold Load() failed: %v", err)
	}

	// Remove the file from disk.  fsnotify delivers the Remove event
	// asynchronously, so the cache entry should still be valid right now.
	if err = os.Remove(path); err != nil {
		t.Fatalf("failed to remove file: %v", err)
	}

	// Immediate warm load — must return cached value, NOT an error.
	got, err := sc.Load(context.Background(), path)
	if err != nil {
		t.Fatalf("warm Load() returned unexpected error: %v", err)
	}

	if got != "warm content" {
		t.Errorf("warm Load() = %q; want %q", got, "warm content")
	}
}

// TestSkillCache_InvalidationOnWrite verifies that a Write event causes cache
// eviction and the next Load re-reads the updated content from disk.
func TestSkillCache_InvalidationOnWrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "SKILL.md")

	if err := os.WriteFile(path, []byte("original"), 0o600); err != nil {
		t.Fatalf("failed to write file: %v", err)
	}

	sc, err := New()
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}
	defer func() { _ = sc.Close() }()

	// Cold load — registers the watcher.
	got, err := sc.Load(context.Background(), path)
	if err != nil {
		t.Fatalf("cold Load() failed: %v", err)
	}

	if got != "original" {
		t.Errorf("cold Load() = %q; want %q", got, "original")
	}

	// Overwrite the file — triggers a Write event.
	if err = os.WriteFile(path, []byte("updated"), 0o600); err != nil {
		t.Fatalf("failed to overwrite file: %v", err)
	}

	// Wait for fsnotify to process the Write event and evict the cache entry.
	evicted := waitForEvent(t, func() bool {
		sc.mu.RLock()
		_, present := sc.entries[path]
		sc.mu.RUnlock()

		return !present
	})

	if !evicted {
		t.Fatal("cache entry was not evicted after Write event within timeout")
	}

	// Next Load must re-read updated content from disk.
	got, err = sc.Load(context.Background(), path)
	if err != nil {
		t.Fatalf("Load() after Write event failed: %v", err)
	}

	if got != "updated" {
		t.Errorf("Load() after Write = %q; want %q", got, "updated")
	}
}

// TestSkillCache_InvalidationOnRemove verifies that a Remove event causes cache
// eviction and the next Load returns an error (file is gone).
func TestSkillCache_InvalidationOnRemove(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "SKILL.md")

	if err := os.WriteFile(path, []byte("to be removed"), 0o600); err != nil {
		t.Fatalf("failed to write file: %v", err)
	}

	sc, err := New()
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}
	defer func() { _ = sc.Close() }()

	// Cold load — populates cache and registers watcher.
	if _, err = sc.Load(context.Background(), path); err != nil {
		t.Fatalf("cold Load() failed: %v", err)
	}

	// Remove file from disk — triggers a Remove event.
	if err = os.Remove(path); err != nil {
		t.Fatalf("failed to remove file: %v", err)
	}

	// Wait for fsnotify to process the Remove event and evict the cache entry.
	evicted := waitForEvent(t, func() bool {
		sc.mu.RLock()
		_, present := sc.entries[path]
		sc.mu.RUnlock()

		return !present
	})

	if !evicted {
		t.Fatal("cache entry was not evicted after Remove event within timeout")
	}

	// Next Load must return an error because the file is gone.
	_, err = sc.Load(context.Background(), path)
	if err == nil {
		t.Error("Load() after Remove should return an error; got nil")
	}
}

// TestSkillCache_Close verifies that Close stops the watcher without panicking
// and subsequent Load calls still return cached values.
func TestSkillCache_Close(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "SKILL.md")

	if err := os.WriteFile(path, []byte("close test"), 0o600); err != nil {
		t.Fatalf("failed to write file: %v", err)
	}

	sc, err := New()
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}

	// Populate cache.
	if _, err = sc.Load(context.Background(), path); err != nil {
		t.Fatalf("Load() failed: %v", err)
	}

	// Close watcher.
	if err = sc.Close(); err != nil {
		t.Fatalf("Close() failed: %v", err)
	}

	// Load after Close must still return cached content (map is intact).
	got, err := sc.Load(context.Background(), path)
	if err != nil {
		t.Fatalf("Load() after Close failed: %v", err)
	}

	if got != "close test" {
		t.Errorf("Load() after Close = %q; want %q", got, "close test")
	}
}

// TestSkillCache_MissingFile verifies that Load returns an error for a path
// that does not exist and was never cached.
func TestSkillCache_MissingFile(t *testing.T) {
	sc, err := New()
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}
	defer func() { _ = sc.Close() }()

	_, err = sc.Load(context.Background(), "/nonexistent/path/SKILL.md")
	if err == nil {
		t.Error("Load() for missing file should return an error; got nil")
	}
}

// TestSkillCache_WatcherReregisteredAfterRename verifies that after a Rename
// event (e.g. an atomic save: write-to-temp + rename) the watcher is
// re-registered on the next Load so that subsequent writes are still detected.
func TestSkillCache_WatcherReregisteredAfterRename(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "SKILL.md")

	if err := os.WriteFile(path, []byte("v1"), 0o600); err != nil {
		t.Fatalf("failed to write file: %v", err)
	}

	sc, err := New()
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}
	defer func() { _ = sc.Close() }()

	// Cold load — registers the watcher.
	got, err := sc.Load(context.Background(), path)
	if err != nil {
		t.Fatalf("cold Load() failed: %v", err)
	}
	if got != "v1" {
		t.Fatalf("cold Load() = %q; want %q", got, "v1")
	}

	// Simulate atomic save: write temp, rename to target.
	tmpPath := path + ".tmp"
	if writeErr := os.WriteFile(tmpPath, []byte("v2"), 0o600); writeErr != nil {
		t.Fatalf("failed to write temp file: %v", writeErr)
	}
	if renameErr := os.Rename(tmpPath, path); renameErr != nil {
		t.Fatalf("failed to rename temp file: %v", renameErr)
	}

	// Wait for fsnotify to process the Rename/Create event and evict.
	evicted := waitForEvent(t, func() bool {
		sc.mu.RLock()
		_, present := sc.entries[path]
		sc.mu.RUnlock()

		return !present
	})
	if !evicted {
		// On some OS / editors the Rename event may not be received for the
		// old path; use Evict as fallback to keep the test deterministic.
		sc.Evict(path)
	}

	// Re-load — must re-read v2 from disk AND re-register the watcher.
	got, err = sc.Load(context.Background(), path)
	if err != nil {
		t.Fatalf("Load() after rename failed: %v", err)
	}
	if got != "v2" {
		t.Errorf("Load() after rename = %q; want %q", got, "v2")
	}

	// Verify that the watcher was re-registered by checking that a
	// subsequent in-place Write still triggers eviction.
	if writeErr := os.WriteFile(path, []byte("v3"), 0o600); writeErr != nil {
		t.Fatalf("failed to write v3: %v", writeErr)
	}

	evicted = waitForEvent(t, func() bool {
		sc.mu.RLock()
		_, present := sc.entries[path]
		sc.mu.RUnlock()

		return !present
	})
	if !evicted {
		t.Fatal("cache entry was not evicted after second Write event — watcher not re-registered")
	}

	got, err = sc.Load(context.Background(), path)
	if err != nil {
		t.Fatalf("Load() after v3 write failed: %v", err)
	}
	if got != "v3" {
		t.Errorf("Load() after v3 write = %q; want %q", got, "v3")
	}
}

// TestSkillCache_ExplicitEvict verifies that Evict clears the cache and allows
// a fresh Load from disk.
func TestSkillCache_ExplicitEvict(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "SKILL.md")

	if err := os.WriteFile(path, []byte("before"), 0o600); err != nil {
		t.Fatalf("failed to write file: %v", err)
	}

	sc, err := New()
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}
	defer func() { _ = sc.Close() }()

	// Cold load.
	got, err := sc.Load(context.Background(), path)
	if err != nil {
		t.Fatalf("Load() failed: %v", err)
	}
	if got != "before" {
		t.Fatalf("Load() = %q; want %q", got, "before")
	}

	// Update file and explicitly evict.
	if writeErr := os.WriteFile(path, []byte("after"), 0o600); writeErr != nil {
		t.Fatalf("failed to overwrite file: %v", writeErr)
	}
	sc.Evict(path)

	got, err = sc.Load(context.Background(), path)
	if err != nil {
		t.Fatalf("Load() after Evict failed: %v", err)
	}
	if got != "after" {
		t.Errorf("Load() after Evict = %q; want %q", got, "after")
	}
}

// TestSkillCache_GeneratedFileSkipsWatcher verifies that a file whose
// frontmatter declares `generated: true` is cached but not registered with
// the fsnotify watcher — invalidation is the writer's exclusive
// responsibility for these files.
func TestSkillCache_GeneratedFileSkipsWatcher(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "devices.md")

	content := "---\ngenerated: true\ngenerator: hatwin-snapshotter\ninject: false\n---\n\nbody"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("failed to write file: %v", err)
	}

	sc, err := New()
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}
	defer func() { _ = sc.Close() }()

	got, err := sc.Load(context.Background(), path)
	if err != nil {
		t.Fatalf("Load() failed: %v", err)
	}
	if got != content {
		t.Errorf("Load() = %q; want %q", got, content)
	}

	if !sc.IsGenerated(path) {
		t.Error("expected IsGenerated(path) to be true")
	}

	sc.mu.RLock()
	watched := sc.watched[path]
	sc.mu.RUnlock()

	if watched {
		t.Error("expected no fsnotify watcher to be registered for a generated:true file")
	}

	// Explicit Evict must still work regardless of watcher registration.
	sc.Evict(path)
	if sc.IsGenerated(path) {
		t.Error("expected generated flag to clear on Evict")
	}
}

// TestSkillCache_HandMaintainedFileRegistersWatcher verifies that a file
// without `generated: true` frontmatter (or with no frontmatter at all) keeps
// the pre-existing fsnotify-watched behavior.
func TestSkillCache_HandMaintainedFileRegistersWatcher(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "custom-notes.md")

	if err := os.WriteFile(path, []byte("hand-written notes"), 0o600); err != nil {
		t.Fatalf("failed to write file: %v", err)
	}

	sc, err := New()
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}
	defer func() { _ = sc.Close() }()

	if _, err := sc.Load(context.Background(), path); err != nil {
		t.Fatalf("Load() failed: %v", err)
	}

	if sc.IsGenerated(path) {
		t.Error("expected IsGenerated(path) to be false for a hand-maintained file")
	}

	sc.mu.RLock()
	watched := sc.watched[path]
	sc.mu.RUnlock()

	if !watched {
		t.Error("expected a fsnotify watcher to be registered for a hand-maintained file")
	}
}
