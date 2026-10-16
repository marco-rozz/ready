// Package skillssync implements the per-file three-way library sync run at
// serve boot before the skills manager starts: it brings the runtime skill
// copy (<runtime home>/skills/) up to date with the shipped library
// (the release bundle's skills/, installed at <Install home>/library/skills/
// or skills.library_path), per file and against a recorded state, so
// untouched files receive shipped fixes while operator edits are never
// overwritten.
//
// The recorded state lives at <runtime home>/skills/.library-state.json and
// maps every synced file's relative path to the sha256 of the library version
// last synced. Machine-generated content (any generated/ directory inside the
// runtime copy) and files the operator adds never acquire a state entry, so
// they are excluded from every action by construction. The daemon never
// writes into the library.
package skillssync

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/rs/zerolog"

	"github.com/marco-rozz/ready/internal/config"
)

// stateFileName is the recorded-sync-state file inside the runtime skill
// directory. It is daemon-private operator state, never a skill file.
const stateFileName = ".library-state.json"

// generatedDir is the machine-generated directory name (the twin
// Snapshotter's generated/ assets): never recorded, never touched by the
// sync — the Snapshotter is its sole invalidation authority.
const generatedDir = "generated"

// mode is the effective sync policy.
type mode string

const (
	modeAuto   mode = "auto"
	modeNotify mode = "notify"
	modeOff    mode = "off"
)

func modeOf(s string) mode {
	switch s {
	case config.SkillsSyncNotify:
		return modeNotify
	case config.SkillsSyncOff:
		return modeOff
	case config.SkillsSyncAuto:
		return modeAuto
	}
	// Config.Validate rejects other values before this point; empty is auto.
	return modeAuto
}

// state is the recorded sync state (one entry per synced library file).
type state struct {
	// Files maps a library-relative slash path to the sha256 (hex) of the
	// library version last synced.
	Files map[string]string `json:"files"`
}

// syncLog collects the actions a run decided, for `notify` reporting and for
// the tests. Relative library paths.
type syncLog struct {
	Copied     []string
	Updated    []string
	Converged  []string
	Conflicted []string // conflicts untouched, WARN-ed (the design doc row 4)
	Orphans    []string // recorded files no longer in the library
}

// syncer executes one boot sync against concrete paths; Run is its
// constructor-free facade.
type syncer struct {
	runtimePath string
	libraryPath string
	mode        mode
	logger      zerolog.Logger
	log         syncLog
}

// Run synchronises the runtime skill copy from the library. It returns an
// error only on (unexpected) I/O failure of the sync machinery itself; every
// behavioural outcome (absent library, conflicts, orphans) is a no-op or a
// log entry. With `off` it returns nil without touching anything.
func Run(ctx context.Context, cfg config.SkillsConfig, runtimeHome string, logger zerolog.Logger) error {
	logger = logger.With().Str("component", "skillssync").Logger()

	m := modeOf(cfg.Sync)
	if m == modeOff {
		logger.Debug().Msg("skill library sync disabled (skills.sync: off)")

		return nil
	}

	if cfg.LibraryPath == "" {
		logger.Debug().Msg("no skill library location resolved — sync is a no-op")

		return nil
	}

	s := &syncer{
		runtimePath: filepath.Join(runtimeHome, "skills"),
		libraryPath: cfg.LibraryPath,
		mode:        m,
		logger:      logger,
	}

	return s.run(ctx)
}

func (s *syncer) run(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("skill library sync: %w", err)
	}

	libInfo, err := os.Stat(s.libraryPath)
	if err != nil || !libInfo.IsDir() {
		// An absent library is a silent no-op (binary-only deployments) —
		// deliberately swallowing the stat error.
		s.logger.Debug().Str("library", s.libraryPath).Msg("skill library absent — sync is a no-op")

		//nolint:nilerr // an absent library is a documented silent no-op, not a failure
		return nil
	}

	st, err := readState(filepath.Join(s.runtimePath, stateFileName))
	if err != nil {
		// A corrupt state file falls back to empty history: untouched-file
		// detection degrades (pre-existing files stay unrecorded, operator
		// edits survive by the convergence shortcut), never a boot failure.
		s.logger.Warn().Err(err).Msg("skill library sync state unreadable — continuing with empty state")
		st = &state{Files: map[string]string{}}
	}

	libFiles, err := walkLibrary(s.libraryPath)
	if err != nil {
		return fmt.Errorf("skill library sync: %w", err)
	}
	// Machine-generated content (any generated/ directory) is the
	// Snapshotter's authority inside the runtime copy: never synced from the
	// library, never recorded.
	for rel := range libFiles {
		if isGenerated(rel) {
			delete(libFiles, rel)
		}
	}

	changed := false
	for rel := range libFiles {
		applied, err := s.syncFile(st, rel)
		if err != nil {
			return err
		}
		changed = changed || applied
	}

	// Orphans: recorded files the new library no longer ships. The runtime
	// file stays (rm is the operator's explicit act).
	for rel := range st.Files {
		if _, ok := libFiles[rel]; !ok {
			s.log.Orphans = append(s.log.Orphans, rel)
			s.logger.Warn().Str("file", rel).Msg("skill file no longer in the library — left in the runtime copy as an orphan")
		}
	}

	if !changed && len(s.log.Conflicted) == 0 && len(s.log.Orphans) == 0 && len(s.log.Copied) == 0 {
		return nil
	}

	if s.mode == modeNotify {
		s.reportPending()

		return nil
	}

	if changed {
		if err := writeState(filepath.Join(s.runtimePath, stateFileName), st); err != nil {
			return fmt.Errorf("skill library sync: %w", err)
		}
	}

	return nil
}

// syncFile applies the per-file decision rows of the sync table for one
// library file. Returns true when something was (or in notify mode would
// have been) written.
func (s *syncer) syncFile(st *state, rel string) (bool, error) {
	libPath := filepath.Join(s.libraryPath, filepath.FromSlash(rel))
	runPath := filepath.Join(s.runtimePath, filepath.FromSlash(rel))

	// Containment (defense-in-depth over a WalkDir-produced path): the
	// destination must stay inside the runtime skill directory — the
	// library could carry a hostile relative entry.
	if back, err := filepath.Rel(s.runtimePath, runPath); err != nil || back == ".." || strings.HasPrefix(back, ".."+string(os.PathSeparator)) {
		return false, fmt.Errorf("skill library sync: refusing to write outside the runtime skill directory: %s", rel)
	}

	libHash, err := sha256File(libPath)
	if err != nil {
		return false, fmt.Errorf("skill library sync: %w", err)
	}

	// Convergence shortcut fires before every other row: a runtime file the
	// operator experimented on and then promoted into the library converges
	// silently instead of raising a permanent conflict.
	runHash, runExists := sha256IfExists(runPath)
	if runExists && runHash == libHash {
		if st.Files[rel] != libHash {
			st.Files[rel] = libHash
			s.log.Converged = append(s.log.Converged, rel)
			s.logger.Debug().Str("file", rel).Msg("runtime file equals the library copy — converged, state recorded")

			return s.mode != modeNotify, nil
		}

		return false, nil
	}

	recorded, recordedOK := st.Files[rel]

	switch {
	case !runExists && !recordedOK:
		// Row 1 (absent | none | present): fresh copy of a shipped file the
		// runtime copy never had.
		if s.mode == modeNotify {
			s.log.Copied = append(s.log.Copied, rel)

			return true, nil
		}
		if err := copyFile(libPath, runPath); err != nil {
			return false, fmt.Errorf("skill library sync: %w", err)
		}
		st.Files[rel] = libHash
		s.log.Copied = append(s.log.Copied, rel)
		s.logger.Info().Str("file", rel).Msg("skill file copied from the library")

		return true, nil

	case recordedOK && recorded == libHash:
		// Row 3 (equals state | yes | unchanged): nothing to do.

		return false, nil

	case !runExists && recordedOK:
		// Row 5 (absent | yes | any): the operator deleted it — respected,
		// never resurrected (whatever the library now holds).

		return false, nil

	case recordedOK && runHash == recorded:
		// Row 2 (equals state | yes | changed): the operator never touched
		// the file, the library has a shipped fix — update and record.
		if s.mode == modeNotify {
			s.log.Updated = append(s.log.Updated, rel)

			return true, nil
		}
		if err := copyFile(libPath, runPath); err != nil {
			return false, fmt.Errorf("skill library sync: %w", err)
		}
		st.Files[rel] = libHash
		s.log.Updated = append(s.log.Updated, rel)
		s.logger.Info().Str("file", rel).Msg("skill file updated from the library")

		return true, nil

	case recordedOK:
		// Row 4 (differs from the recorded state | yes | changed): an
		// operator edit — untouched, one WARN naming both files so diffing
		// them is the operator's merge. Never a skip-side effect.
		s.log.Conflicted = append(s.log.Conflicted, rel)
		s.logger.Warn().
			Str("runtime_file", runPath).
			Str("library_file", libPath).
			Msg("skill file edited in the runtime copy while the library also changed — left untouched (diff both files to merge)")

		return false, nil

	default:
		// Row 6 (present | none | present): operator-added or pre-existing
		// file the sync never recorded — untouched, unrecorded.
		return false, nil
	}
}

// reportPending logs the actions notify mode did not apply.
func (s *syncer) reportPending() {
	for _, rel := range s.log.Copied {
		s.logger.Info().Str("file", rel).Msg("skill library has a new file; skills.sync is notify — not applied")
	}
	for _, rel := range s.log.Updated {
		s.logger.Info().Str("file", rel).Msg("skill library changed this file; skills.sync is notify — not applied")
	}
	for _, rel := range s.log.Conflicted {
		s.logger.Warn().Str("file", rel).Msg("skill file conflicts with the library; skills.sync is notify — not applied, diff to merge")
	}
}

// isGenerated reports whether a library-relative path carries machine-
// generated content (any generated/ directory component). Such paths never
// get a state entry, so the sync never rewrites them.
func isGenerated(rel string) bool {
	for _, part := range strings.Split(rel, "/") {
		if part == generatedDir {
			return true
		}
	}

	return false
}

// walkLibrary returns the library's regular files as relative slash paths,
// including nested directories; the state file never lives in the library.
func walkLibrary(libraryPath string) (map[string]struct{}, error) {
	files := map[string]struct{}{}
	err := filepath.WalkDir(libraryPath, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !d.Type().IsRegular() {
			return nil
		}

		rel, err := filepath.Rel(libraryPath, path)
		if err != nil {
			return err
		}

		files[filepath.ToSlash(rel)] = struct{}{}

		return nil
	})
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}

	return files, nil
}

func readState(path string) (*state, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &state{Files: map[string]string{}}, nil
		}

		return nil, err
	}

	var st state
	if err := json.Unmarshal(data, &st); err != nil {
		return nil, fmt.Errorf("corrupt state file %s: %w", path, err)
	}
	if st.Files == nil {
		st.Files = map[string]string{}
	}

	return &st, nil
}

func writeState(path string, st *state) error {
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}

	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}

	return os.Rename(tmp, path)
}

func sha256File(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)

	return hex.EncodeToString(sum[:]), nil
}

// sha256IfExists returns (hash, true) for an existing regular file and
// ("", false) when absent; any other stat/read error surfaces.
func sha256IfExists(path string) (string, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", false
		}

		return "", true
	}
	sum := sha256.Sum256(data)

	return hex.EncodeToString(sum[:]), true
}

func copyFile(src, dst string) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}

	//nolint:gosec // G703: containment was verified by the caller (the destination was checked to stay inside the runtime skill directory)
	return os.WriteFile(dst, data, 0o600)
}
