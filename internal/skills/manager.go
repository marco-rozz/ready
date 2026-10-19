// Package skills provides skill lifecycle management with progressive disclosure
// using agentskills-go runtime and filesystem provider.
package skills

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/flexigpt/agentskills-go/provider"
	fsskillprovider "github.com/flexigpt/agentskills-go/provider/fs"
	"github.com/flexigpt/agentskills-go/runtime"
	runtimeSpec "github.com/flexigpt/agentskills-go/runtime/spec"
	"github.com/fsnotify/fsnotify"
	"github.com/marco-rozz/ready/internal/config"
	"github.com/rs/zerolog"
)

const (
	skillDotMD = "SKILL.md"
)

// SkillInfo holds metadata about a discovered skill for listing purposes.
type SkillInfo struct {
	Name        string
	Description string
	Location    string
}

// ReloadCallback is invoked after a skill is successfully hot-reloaded.
// The callback receives the context and the name of the reloaded skill.
type ReloadCallback func(ctx context.Context, skillName string)

// Manager wraps the agentskills-go runtime with filesystem provider.
type Manager struct {
	runtime     *runtime.Runtime
	path        string
	libraryPath string // shipped library: the fallback for daemon-injected skills
	logger      zerolog.Logger

	mu              sync.RWMutex
	skills          []provider.SkillDef // tracked skill definitions
	contentCache    map[string]string   // skillName → SKILL.md content; invalidated by fsnotify on Write/Create
	reloadCallbacks []ReloadCallback    // notified after a skill is hot-reloaded
	watcher         *fsnotify.Watcher
	cancelFn        context.CancelFunc
}

// NewManager creates a skills manager with filesystem provider.
func NewManager(ctx context.Context, cfg *config.SkillsConfig, logger zerolog.Logger) (*Manager, error) {
	if ctx == nil {
		return nil, fmt.Errorf("failed to create skills manager: %w", context.Canceled)
	}

	select {
	case <-ctx.Done():
		return nil, fmt.Errorf("failed to create skills manager: %w", ctx.Err())
	default:
	}

	if cfg == nil {
		return nil, fmt.Errorf("failed to create skills manager: skills config is required")
	}

	skillsPath := cfg.Path
	if skillsPath == "" {
		return nil, fmt.Errorf("failed to create skills manager: skills.path is empty — the runtime home defaults it in config.Load; construct the config through config.Load or set skills.path")
	}

	// Ensure skills directory exists.
	absPath, err := filepath.Abs(skillsPath)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve skills path: %w", err)
	}

	if mkdirErr := os.MkdirAll(absPath, 0o755); mkdirErr != nil {
		return nil, fmt.Errorf("failed to create skills directory: %w", mkdirErr)
	}

	// Create filesystem provider.
	fsProvider, err := fsskillprovider.New()
	if err != nil {
		return nil, fmt.Errorf("failed to create fs skill provider: %w", err)
	}

	// Create runtime with the provider.
	rt, err := runtime.New(
		runtime.WithProvider(fsProvider),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create skills runtime: %w", err)
	}

	m := &Manager{
		runtime:      rt,
		path:         absPath,
		libraryPath:  cfg.LibraryPath,
		logger:       logger.With().Str("component", "skills").Logger(),
		contentCache: make(map[string]string),
	}

	// Discover and register existing skills.
	if err = m.discoverSkills(ctx); err != nil {
		return nil, fmt.Errorf("failed to discover skills: %w", err)
	}

	// Start hot-reload watcher if enabled.
	if cfg.HotReload {
		if err = m.startWatcher(ctx); err != nil {
			return nil, fmt.Errorf("failed to start skills watcher: %w", err)
		}
	}

	return m, nil
}

// discoverSkills scans the skills directory and registers discovered skills.
func (m *Manager) discoverSkills(ctx context.Context) error {
	entries, err := os.ReadDir(m.path)
	if err != nil {
		return fmt.Errorf("failed to read skills directory: %w", err)
	}

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}

		skillDir := filepath.Join(m.path, entry.Name())
		skillMD := filepath.Join(skillDir, skillDotMD)

		if _, err = os.Stat(skillMD); os.IsNotExist(err) {
			continue
		}

		def := provider.SkillDef{
			Type:     fsskillprovider.Type,
			Name:     entry.Name(),
			Location: skillDir,
		}

		if _, addErr := m.runtime.AddSkill(ctx, def); addErr != nil {
			m.logger.Warn().Str("skill", entry.Name()).Str("error", addErr.Error()).Msg("failed to add skill")

			continue
		}

		m.mu.Lock()
		m.skills = append(m.skills, def)

		// Pre-warm content cache: avoids a cold-miss on first ReadSkillContent call
		// (e.g. formatter pipeline on the very first message after startup).
		if data, readErr := os.ReadFile(skillMD); readErr == nil {
			m.contentCache[entry.Name()] = string(data)
		} else {
			m.logger.Warn().Str("skill", entry.Name()).Str("error", readErr.Error()).Msg("failed to pre-warm skill content cache")
		}

		m.mu.Unlock()
	}

	return nil
}

// GetMetadataPrompt returns skill metadata for the system prompt (progressive disclosure).
// Only names and descriptions — NOT full content.
// Skills that are Active in the given session are excluded so already-bootstrapped
// skills do not appear in the "Available Skills" list.
func (m *Manager) GetMetadataPrompt(ctx context.Context, sessionID runtimeSpec.SessionID) (string, error) {
	if ctx == nil {
		return "", fmt.Errorf("failed to get metadata prompt: %w", context.Canceled)
	}

	prompt, err := m.runtime.SkillsPrompt(ctx, &runtime.SkillFilter{
		SessionID: sessionID,
		Activity:  runtimeSpec.SkillActivityInactive,
	})
	if err != nil {
		return "", fmt.Errorf("failed to build skills prompt: %w", err)
	}

	return prompt, nil
}

// NewSession creates a new agentskills session and returns its ID.
func (m *Manager) NewSession(ctx context.Context) (runtimeSpec.SessionID, error) {
	if ctx == nil {
		return "", fmt.Errorf("failed to create session: %w", context.Canceled)
	}

	sid, _, err := m.runtime.NewSession(ctx)
	if err != nil {
		return "", fmt.Errorf("failed to create skills session: %w", err)
	}

	return sid, nil
}

// CloseSession closes a skills session.
func (m *Manager) CloseSession(ctx context.Context, sid runtimeSpec.SessionID) error {
	return m.runtime.CloseSession(ctx, sid)
}

// LoadSkill loads full skill content into the active session.
func (m *Manager) LoadSkill(ctx context.Context, sessionID runtimeSpec.SessionID, skillName string) (string, error) {
	if ctx == nil {
		return "", fmt.Errorf("failed to load skill: %w", context.Canceled)
	}

	m.mu.RLock()
	var targetDef *provider.SkillDef
	var targetLocation string
	for i := range m.skills {
		if m.skills[i].Name == skillName {
			targetDef = &m.skills[i]
			targetLocation = m.skills[i].Location

			break
		}
	}
	m.mu.RUnlock()

	if targetDef == nil {
		// Daemon-injected skills (home-integration, formatter, Compaction)
		// fall back to the library copy when the runtime copy lacks them —
		// a missing injected skill must never fail its consumer (one WARN
		// naming the skill). The compiled Compaction fallback stays the
		// last resort when neither copy exists.
		libDir := m.librarySkillDir(skillName)
		if libDir == "" {
			return "", fmt.Errorf("failed to load skill: skill %q not found", skillName)
		}
		m.logger.Warn().Str("skill", skillName).Msg("skill not present in the runtime copy — loading the library copy")
		targetLocation = libDir
	}

	// Load the skill into the session via the registry.
	reg, err := m.runtime.NewSessionRegistry(ctx, sessionID)
	if err != nil {
		return "", fmt.Errorf("failed to get session registry: %w", err)
	}

	// Call the load tool via the registry using FuncID.
	loadInput := runtimeSpec.LoadArgs{
		Skills: []runtimeSpec.SkillHandle{{Name: skillName, Location: targetLocation}},
		Mode:   runtimeSpec.LoadModeAdd,
	}

	inputJSON, err := json.Marshal(loadInput)
	if err != nil {
		return "", fmt.Errorf("failed to marshal load args: %w", err)
	}

	if _, callErr := reg.Call(ctx, runtimeSpec.FuncIDSkillsLoad, inputJSON); callErr != nil {
		return "", fmt.Errorf("failed to load skill %q: %w", skillName, callErr)
	}

	// Now get the active skills prompt which includes the body.
	prompt, err := m.runtime.SkillsPrompt(ctx, &runtime.SkillFilter{
		SessionID: sessionID,
		Activity:  runtimeSpec.SkillActivityActive,
	})
	if err != nil {
		return "", fmt.Errorf("failed to get active skills prompt: %w", err)
	}

	return prompt, nil
}

// ActiveSkills returns the names of the skills Active in an agentskills
// session (bootstrap, formatter and skills-load activations alike).
func (m *Manager) ActiveSkills(ctx context.Context, sessionID runtimeSpec.SessionID) ([]string, error) {
	records, err := m.runtime.ListSkills(ctx, &runtime.SkillListFilter{
		SessionID: sessionID,
		Activity:  runtimeSpec.SkillActivityActive,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to list active skills: %w", err)
	}

	names := make([]string, 0, len(records))
	for _, r := range records {
		names = append(names, r.Def.Name)
	}

	return names, nil
}

// UnloadSkill removes a skill from the active session.
func (m *Manager) UnloadSkill(ctx context.Context, sessionID runtimeSpec.SessionID, skillName string) error {
	if ctx == nil {
		return fmt.Errorf("failed to unload skill: %w", context.Canceled)
	}

	m.mu.RLock()
	var targetDef *provider.SkillDef
	for i := range m.skills {
		if m.skills[i].Name == skillName {
			targetDef = &m.skills[i]

			break
		}
	}
	m.mu.RUnlock()

	if targetDef == nil {
		return fmt.Errorf("failed to unload skill: skill %q not found", skillName)
	}

	reg, err := m.runtime.NewSessionRegistry(ctx, sessionID)
	if err != nil {
		return fmt.Errorf("failed to get session registry: %w", err)
	}

	// A library-fallback skill was Activated from the library location, so
	// unloading must use that same location for the registry to match.
	unloadLocation := targetDef.Location
	if unloadLocation == "" && m.skills == nil {
		return fmt.Errorf("failed to unload skill: skill %q not found", skillName)
	}

	unloadInput := runtimeSpec.UnloadArgs{
		Skills: []runtimeSpec.SkillHandle{{Name: skillName, Location: unloadLocation}},
	}

	inputJSON, err := json.Marshal(unloadInput)
	if err != nil {
		return fmt.Errorf("failed to marshal unload args: %w", err)
	}

	if _, callErr := reg.Call(ctx, runtimeSpec.FuncIDSkillsUnload, inputJSON); callErr != nil {
		return fmt.Errorf("failed to unload skill %q: %w", skillName, callErr)
	}

	return nil
}

// ListSkills returns available skill names and descriptions.
func (m *Manager) ListSkills(ctx context.Context) ([]SkillInfo, error) {
	if ctx == nil {
		return nil, fmt.Errorf("failed to list skills: %w", context.Canceled)
	}

	records, err := m.runtime.ListSkills(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to list skills: %w", err)
	}

	infos := make([]SkillInfo, 0, len(records))
	for _, rec := range records {
		infos = append(infos, SkillInfo{
			Name:        rec.Def.Name,
			Description: rec.Description,
			Location:    rec.Def.Location,
		})
	}

	return infos, nil
}

// ReadSkillContent returns the raw SKILL.md content for a given skill name.
// Results are cached in memory; the cache entry is automatically invalidated
// by the fsnotify watcher whenever the SKILL.md file is modified or recreated.
func (m *Manager) ReadSkillContent(ctx context.Context, skillName string) (string, error) {
	if ctx == nil {
		return "", fmt.Errorf("failed to read skill content: %w", context.Canceled)
	}

	select {
	case <-ctx.Done():
		return "", fmt.Errorf("failed to read skill content: %w", ctx.Err())
	default:
	}

	// Fast path: cache hit (read lock only).
	m.mu.RLock()
	if content, ok := m.contentCache[skillName]; ok {
		m.mu.RUnlock()

		return content, nil
	}

	var location string
	for i := range m.skills {
		if m.skills[i].Name == skillName {
			location = m.skills[i].Location

			break
		}
	}
	m.mu.RUnlock()

	if location == "" {
		return "", fmt.Errorf("failed to read skill content: skill %q not found", skillName)
	}

	// Cache miss: read from disk.
	skillMD := filepath.Join(location, skillDotMD)

	data, err := os.ReadFile(skillMD)
	if err != nil {
		return "", fmt.Errorf("failed to read skill file %q: %w", skillMD, err)
	}

	content := string(data)

	// Populate cache (write lock).
	m.mu.Lock()
	m.contentCache[skillName] = content
	m.mu.Unlock()

	return content, nil
}

// SkillFilePath returns the absolute path to the SKILL.md file for the named
// skill.  Returns an error when the skill is not registered.
// librarySkillDir returns the library copy's directory for the named skill
// when the library carries one, "" otherwise.
func (m *Manager) librarySkillDir(skillName string) string {
	if m.libraryPath == "" {
		return ""
	}

	dir := filepath.Join(m.libraryPath, skillName)
	info, err := os.Stat(filepath.Join(dir, skillDotMD))
	if err != nil || info.IsDir() {
		return ""
	}

	return dir
}

// SkillFilePath returns the absolute path to the SKILL.md file for the named
// skill. Returns an error when the skill is not registered; daemon-injected
// skills absent from the runtime copy fall back to the library copy with a
// WARN (install-home: a missing injected skill must never fail its consumer).
func (m *Manager) SkillFilePath(skillName string) (string, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	for i := range m.skills {
		if m.skills[i].Name == skillName {
			return filepath.Join(m.skills[i].Location, skillDotMD), nil
		}
	}

	if dir := m.librarySkillDir(skillName); dir != "" {
		m.logger.Warn().Str("skill", skillName).Msg("skill not present in the runtime copy — using the library copy")

		return filepath.Join(dir, skillDotMD), nil
	}

	return "", fmt.Errorf("skill %q not found", skillName)
}

// GetSkillLocation returns the absolute path to the directory containing the named skill.
// Returns an error when the skill is not registered; daemon-injected skills
// absent from the runtime copy fall back to the library copy with a WARN.
func (m *Manager) GetSkillLocation(skillName string) (string, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	for i := range m.skills {
		if m.skills[i].Name == skillName {
			return m.skills[i].Location, nil
		}
	}

	if dir := m.librarySkillDir(skillName); dir != "" {
		m.logger.Warn().Str("skill", skillName).Msg("skill not present in the runtime copy — using the library copy")

		return dir, nil
	}

	return "", fmt.Errorf("skill %q not found", skillName)
}

// Runtime returns the underlying agentskills runtime for direct access.
func (m *Manager) Runtime() *runtime.Runtime {
	return m.runtime
}

// SkillDefs returns the tracked skill definitions.
func (m *Manager) SkillDefs() []provider.SkillDef {
	m.mu.RLock()
	defer m.mu.RUnlock()

	defs := make([]provider.SkillDef, len(m.skills))
	copy(defs, m.skills)

	return defs
}

// OnReload registers a callback that will be invoked every time a skill is
// successfully hot-reloaded.  Multiple callbacks can be registered; they are
// called in registration order from the watcher goroutine.
func (m *Manager) OnReload(cb ReloadCallback) {
	m.mu.Lock()
	m.reloadCallbacks = append(m.reloadCallbacks, cb)
	m.mu.Unlock()
}

// Close stops the hot-reload watcher and cleans up.
func (m *Manager) Close() error {
	if m.cancelFn != nil {
		m.cancelFn()
	}

	if m.watcher != nil {
		return m.watcher.Close()
	}

	return nil
}

// startWatcher initializes the filesystem watcher for hot-reload.
// criticalities-remediation (LOW lifecycle): on its own error path the
// watcher is closed — an error return must not leak the created watcher
// (the watcher error path cleans up).
func (m *Manager) startWatcher(ctx context.Context) error {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return fmt.Errorf("failed to create filesystem watcher: %w", err)
	}

	m.watcher = watcher

	// Watch the skills root directory.
	if err = watcher.Add(m.path); err != nil {
		_ = watcher.Close()
		m.watcher = nil

		return fmt.Errorf("failed to watch skills directory: %w", err)
	}

	// Also watch each skill subdirectory.
	m.mu.RLock()
	for _, def := range m.skills {
		if watchErr := watcher.Add(def.Location); watchErr != nil {
			m.logger.Warn().Str("skill", def.Name).Str("error", watchErr.Error()).Msg("failed to watch skill directory")
		}
	}
	m.mu.RUnlock()

	watchCtx, cancel := context.WithCancel(ctx)
	m.cancelFn = cancel

	go m.watchLoop(watchCtx)

	return nil
}

// watchLoop processes filesystem events for hot-reload.
func (m *Manager) watchLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case event, ok := <-m.watcher.Events:
			if !ok {
				return
			}
			m.handleWatchEvent(ctx, event)
		case err, ok := <-m.watcher.Errors:
			if !ok {
				return
			}
			m.logger.Error().Str("error", err.Error()).Msg("filesystem watcher error")
		}
	}
}

// handleWatchEvent processes a single filesystem event.
func (m *Manager) handleWatchEvent(ctx context.Context, event fsnotify.Event) {
	// We care about SKILL.md changes and new/removed directories.
	fileName := filepath.Base(event.Name)

	switch {
	case fileName == skillDotMD && (event.Has(fsnotify.Write) || event.Has(fsnotify.Create)):
		// Skill file modified — re-index.
		skillDir := filepath.Dir(event.Name)
		skillName := filepath.Base(skillDir)
		m.reindexSkill(ctx, skillName, skillDir)

	case event.Has(fsnotify.Create):
		// New directory — check if it contains a SKILL.md.
		info, err := os.Stat(event.Name)
		if err != nil || !info.IsDir() {
			return
		}
		skillMD := filepath.Join(event.Name, skillDotMD)
		if _, err = os.Stat(skillMD); err == nil {
			skillName := filepath.Base(event.Name)
			m.addNewSkill(ctx, skillName, event.Name)
		}

	case event.Has(fsnotify.Remove) || event.Has(fsnotify.Rename):
		// Directory removed — remove skill if tracked.
		skillName := filepath.Base(event.Name)
		m.removeSkill(ctx, skillName)
	}
}

// reindexSkill re-registers a modified skill.
func (m *Manager) reindexSkill(ctx context.Context, name, location string) {
	def := provider.SkillDef{
		Type:     fsskillprovider.Type,
		Name:     name,
		Location: location,
	}

	// Remove and re-add to refresh metadata.
	if _, err := m.runtime.RemoveSkill(ctx, def); err != nil {
		m.logger.Warn().Str("skill", name).Str("error", err.Error()).Msg("failed to remove skill for re-index")
	}

	if _, err := m.runtime.AddSkill(ctx, def); err != nil {
		m.logger.Warn().Str("skill", name).Str("error", err.Error()).Msg("failed to re-add skill after modification")

		return
	}

	// Invalidate content cache: the next ReadSkillContent call will re-read from disk.
	m.mu.Lock()
	delete(m.contentCache, name)
	m.mu.Unlock()

	m.logger.Info().Str("skill", name).Msg("skill reloaded")

	// Notify registered callbacks so that dependent components (e.g. formatter,
	// agent system prompt) can react to the change.
	m.mu.RLock()
	callbacks := make([]ReloadCallback, len(m.reloadCallbacks))
	copy(callbacks, m.reloadCallbacks)
	m.mu.RUnlock()

	for _, cb := range callbacks {
		cb(ctx, name)
	}
}

// addNewSkill registers a newly discovered skill.
func (m *Manager) addNewSkill(ctx context.Context, name, location string) {
	def := provider.SkillDef{
		Type:     fsskillprovider.Type,
		Name:     name,
		Location: location,
	}

	if _, err := m.runtime.AddSkill(ctx, def); err != nil {
		m.logger.Warn().Str("skill", name).Str("error", err.Error()).Msg("failed to add new skill")

		return
	}

	m.mu.Lock()
	m.skills = append(m.skills, def)
	m.mu.Unlock()

	// Watch the new directory.
	if m.watcher != nil {
		if err := m.watcher.Add(location); err != nil {
			m.logger.Warn().Str("skill", name).Str("error", err.Error()).Msg("failed to watch new skill directory")
		}
	}

	m.logger.Info().Str("skill", name).Msg("new skill discovered")
}

// removeSkill removes a skill from tracking.
//
// criticalities-remediation (H2): the definition is COPIED by value while the
// lock is held — a pointer into m.skills would be silently re-based by the
// append(m.skills[:idx], m.skills[idx+1:]...) shift below (the element at idx
// is overwritten by the next definition), making the runtime removal target a
// still-valid neighbouring skill.
func (m *Manager) removeSkill(ctx context.Context, name string) {
	m.mu.Lock()
	var found provider.SkillDef
	var idx int
	exists := false
	for i := range m.skills {
		if m.skills[i].Name == name {
			found = m.skills[i] // copy, not pointer
			idx = i
			exists = true

			break
		}
	}

	if !exists {
		m.mu.Unlock()

		return
	}

	// Remove from tracked list and evict from content cache.
	m.skills = append(m.skills[:idx], m.skills[idx+1:]...)
	delete(m.contentCache, name)
	m.mu.Unlock()

	if _, err := m.runtime.RemoveSkill(ctx, found); err != nil {
		m.logger.Warn().Str("skill", name).Str("error", err.Error()).Msg("failed to remove skill from runtime")
	}

	m.logger.Info().Str("skill", name).Msg("skill removed")
}
