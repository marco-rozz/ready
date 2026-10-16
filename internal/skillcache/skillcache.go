// Package skillcache provides an in-memory, fsnotify-backed content cache for
// SKILL.md files. It is keyed by absolute file path and invalidated automatically
// when the underlying file is written, removed, or renamed.
//
// The cache is initialized once at startup and shared by callers (e.g. Formatter).
// Call [SkillCache.Close] during graceful shutdown to stop the watcher goroutine
// and release OS resources.
package skillcache

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"

	"github.com/fsnotify/fsnotify"
	"github.com/rs/zerolog"
)

// SkillCache caches file content by absolute path and registers a fsnotify
// watcher on each path on first access.  Concurrent reads are served from the
// in-memory map; on a Write / Remove / Rename event the corresponding entry is
// evicted so the next [Load] call re-reads from disk.
//
// Files whose frontmatter declares `generated: true` (written by an internal
// generator such as the Twin's Snapshotter) do NOT get a fsnotify watcher:
// invalidation for those files is the writer's exclusive responsibility (it
// calls Evict/Invalidate after each write cycle), which eliminates a race
// where fsnotify and the writer's own signal both fire for the same write.
type SkillCache struct {
	mu        sync.RWMutex
	entries   map[string]string // absolute path → content
	watched   map[string]bool   // paths currently registered with the watcher
	generated map[string]bool   // paths whose frontmatter declared generated: true

	watcher *fsnotify.Watcher
	logger  zerolog.Logger // component-scoped; Nop by default, wired by the daemon
}

// Compile-time assertion: SkillCache satisfies the full surface expected by
// callers — Load for content retrieval and Close for graceful shutdown.
var (
	_ interface {
		Load(context.Context, string) (string, error)
	} = (*SkillCache)(nil)
	_ interface{ Close() error } = (*SkillCache)(nil)
)

// New creates and returns a SkillCache with an active fsnotify watcher goroutine.
// Returns an error only if the underlying watcher cannot be created.
func New() (*SkillCache, error) {
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, fmt.Errorf("skillcache: failed to create watcher: %w", err)
	}

	sc := &SkillCache{
		entries:   make(map[string]string),
		watched:   make(map[string]bool),
		generated: make(map[string]bool),
		watcher:   w,
		logger:    zerolog.Nop(),
	}

	go sc.watchLoop()

	return sc, nil
}

// Load returns the content for the given absolute file path.
//
//   - Empty path → ("", nil) immediately.
//   - Cache hit   → returns the cached string without touching the filesystem.
//   - Cache miss  → reads the file from disk, stores the content, and registers
//     a per-file fsnotify watcher so future Write / Remove / Rename events
//     automatically evict the entry.
//
// Thread-safe; may be called concurrently from multiple goroutines.
func (c *SkillCache) Load(ctx context.Context, path string) (string, error) {
	if ctx == nil {
		ctx = context.Background()
	}

	if path == "" {
		return "", nil
	}

	// Check for early cancellation.
	if err := ctx.Err(); err != nil {
		return "", err
	}

	// Fast path: cache hit (shared read lock only).
	c.mu.RLock()
	if content, ok := c.entries[path]; ok {
		c.mu.RUnlock()

		return content, nil
	}
	c.mu.RUnlock()

	// Cache miss: read from disk before acquiring the write lock so we don't
	// hold the lock during I/O.
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("skillcache: failed to read %q: %w", path, err)
	}

	content := string(data)

	c.mu.Lock()
	// Double-check: another goroutine may have populated the entry while we
	// were reading the file.
	if existing, ok := c.entries[path]; ok {
		c.mu.Unlock()

		return existing, nil
	}

	c.entries[path] = content

	if isGeneratedFrontmatter(data) {
		c.generated[path] = true
	} else if !c.watched[path] {
		// criticalities-remediation (M17): a failed fsnotify registration is
		// observable (WARN naming the path) and retried — the watched flag
		// stays unset, so the next Load of this path tries again. A cache
		// entry is never left permanently un-watched in silence.
		if watchErr := c.watcher.Add(path); watchErr == nil {
			c.watched[path] = true
		} else {
			c.logger.Warn().
				Err(watchErr).
				Str("path", path).
				Msg("skillcache: fsnotify watcher registration failed — will retry on next load")
		}
	}

	c.mu.Unlock()

	return content, nil
}

// WithLogger attaches a component-scoped logger (called by the daemon wiring;
// the default is a Nop logger so tests and direct uses stay silent).
func (c *SkillCache) WithLogger(logger zerolog.Logger) *SkillCache {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.logger = logger

	return c
}

// Evict explicitly removes a cached entry by absolute path and clears the
// watched flag so the next [Load] call re-reads from disk and re-registers the
// watcher.  This is useful when the caller knows the file has changed (e.g.
// after a hot-reload notification) and cannot wait for the asynchronous
// fsnotify event.
func (c *SkillCache) Evict(path string) {
	c.evict(path)
}

// Close stops the fsnotify watcher goroutine and releases all OS resources.
// It is safe to call Close multiple times; subsequent calls are no-ops.
// After Close, Load will still return cached values but will no longer
// receive invalidation events.
func (c *SkillCache) Close() error {
	if c.watcher == nil {
		return nil
	}

	return c.watcher.Close()
}

// watchLoop receives fsnotify events and evicts cache entries on mutation.
// It exits when the watcher's Events channel is closed (i.e. after Close).
func (c *SkillCache) watchLoop() {
	for {
		select {
		case event, ok := <-c.watcher.Events:
			if !ok {
				return
			}

			if event.Has(fsnotify.Write) || event.Has(fsnotify.Remove) || event.Has(fsnotify.Rename) {
				c.evict(event.Name)
			}

		case _, ok := <-c.watcher.Errors:
			if !ok {
				return
			}
			// Watcher errors are non-fatal; the cache self-heals on the next Load call.
		}
	}
}

// evict removes a single path from the in-memory map and clears the watched
// flag so that the next [Load] call re-registers a fresh fsnotify watcher.
// This is essential for editors that perform atomic saves (write-to-temp +
// rename): the original inode's watch becomes stale after rename, so we must
// allow [Load] to call watcher.Add again for the new inode.
func (c *SkillCache) evict(path string) {
	c.mu.Lock()
	delete(c.entries, path)
	delete(c.watched, path)
	delete(c.generated, path)
	c.mu.Unlock()
}

// IsGenerated reports whether the cached entry at path was last loaded with a
// `generated: true` frontmatter marker (and therefore has no fsnotify
// watcher). Returns false for paths not currently cached.
func (c *SkillCache) IsGenerated(path string) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()

	return c.generated[path]
}

// isGeneratedFrontmatter reports whether data begins with a YAML-ish
// frontmatter block (delimited by "---" lines) containing `generated: true`.
// Best-effort: any parse ambiguity (missing/unclosed block) is treated as
// generated: false, preserving the existing fsnotify behavior.
func isGeneratedFrontmatter(data []byte) bool {
	lines := strings.Split(string(data), "\n")
	if len(lines) == 0 || strings.TrimSpace(lines[0]) != "---" {
		return false
	}

	for _, line := range lines[1:] {
		trimmed := strings.TrimSpace(line)
		if trimmed == "---" {
			return false
		}

		key, value, found := strings.Cut(line, ":")
		if !found {
			continue
		}

		if strings.TrimSpace(key) == "generated" && strings.TrimSpace(value) == "true" {
			return true
		}
	}

	return false
}
