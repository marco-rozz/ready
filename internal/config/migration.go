package config

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/rs/zerolog"
	"gopkg.in/yaml.v3"

	"github.com/marco-rozz/ready/internal/secretstore"
)

// Migration transforms only the authored document. It must perform no external actions.
// Released steps are immutable; Version is the destination, not the source.
type Migration interface {
	Version() int
	Description() string
	Up(*yaml.Node) error
}

// MigrationRegistry owns the complete ordered chain from baseline 1 to target.
type MigrationRegistry struct {
	target int
	steps  []Migration
}

func NewMigrationRegistry(target int, steps ...Migration) (*MigrationRegistry, error) {
	if target < BaselineConfigVersion || len(steps) != target-BaselineConfigVersion {
		return nil, fmt.Errorf("%w: expected destinations 2 through %d", ErrInvalidMigrationRegistry, target)
	}
	for i, step := range steps {
		if step == nil || step.Version() != BaselineConfigVersion+i+1 {
			return nil, fmt.Errorf("%w: expected destination %d", ErrInvalidMigrationRegistry, BaselineConfigVersion+i+1)
		}
	}

	return &MigrationRegistry{target: target, steps: append([]Migration(nil), steps...)}, nil
}

// ProductionMigrations is explicit: version 2 moves credentials into the Secret store.
func ProductionMigrations() (*MigrationRegistry, error) {
	return NewMigrationRegistry(TargetConfigVersion, migrationV2{})
}

type MigrationStepReport struct {
	Version     int
	Description string
	Duration    time.Duration
}

// MigrationReport deliberately carries no YAML bodies or runtime configuration values.
type MigrationReport struct {
	Path       string
	From       int
	To         int
	Steps      []MigrationStepReport
	BackupPath string
	Committed  bool
	Warnings   []string
}

// CommitContext keeps the outcome visible when a later startup operation fails.
func (r MigrationReport) CommitContext() string {
	if !r.Committed {
		return ""
	}

	return fmt.Sprintf(" (config migration committed %d→%d at %s; original backup: %s)", r.From, r.To, r.Path, r.BackupPath)
}

// Prepare validates current configs read-only; only pending migrations acquire a
// lock and persist a candidate. validate adds serve's local channel/profile checks.
func (r *MigrationRegistry) Prepare(ctx context.Context, path string, validate func(*Config) error) (*Config, MigrationReport, error) {
	return r.prepare(ctx, path, validate, migrationIO{replace: replaceConfig, syncDir: syncConfigDir})
}

type migrationIO struct {
	// backup, when set, transforms the pre-commit source bytes into the stored
	// backup (encrypted for a migration that imports secrets) and names its
	// file suffix (".age"). Nil keeps the plaintext backup.
	backup  func([]byte) ([]byte, string, error)
	replace func(context.Context, string, string) error
	syncDir func(context.Context, string) error
	write   func(context.Context, string, string, []byte, os.FileMode) (string, error)
	prune   func(context.Context, string) error
}

func (r *MigrationRegistry) prepare(ctx context.Context, path string, validate func(*Config) error, fs migrationIO) (*Config, MigrationReport, error) {
	report := MigrationReport{Path: path}
	if err := ctx.Err(); err != nil {
		return nil, report, fmt.Errorf("prepare config: %w", err)
	}
	if r == nil {
		return nil, report, ErrInvalidMigrationRegistry
	}
	// Validate again: registry integrity is enforced independently of the source version.
	if _, err := NewMigrationRegistry(r.target, r.steps...); err != nil {
		return nil, report, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, report, fmt.Errorf("read config %s: %w", path, err)
	}
	_, version, err := parseVersion(data, path)
	if err != nil {
		return nil, report, err
	}
	report.From, report.To = version, r.target
	if version > r.target {
		return nil, report, checkVersion(path, version, r.target)
	}
	if version == r.target {
		cfg, loadErr := validatedConfig(ctx, data, path, r.target, validate)

		return cfg, report, loadErr
	}
	// Resolve directory aliases but do not follow a symlink leaf that replacement
	// would sever. Keep path as the elected runtime-home input to decoding.
	dir, err := filepath.EvalSymlinks(filepath.Dir(path))
	if err != nil {
		return nil, report, fmt.Errorf("resolve migration directory: %w", err)
	}
	dir, err = filepath.Abs(dir)
	if err != nil {
		return nil, report, fmt.Errorf("resolve migration directory: %w", err)
	}
	targetPath := filepath.Join(dir, filepath.Base(path))
	lock, err := acquireMigrationLock(ctx, targetPath+".migration.lock")
	if err != nil {
		return nil, report, err
	}
	defer lock.Close()
	data, info, err := readMigrationSource(ctx, targetPath)
	if err != nil {
		return nil, report, err
	}
	doc, version, err := parseVersion(data, path)
	if err != nil {
		return nil, report, err
	}
	report.From = version
	if version >= r.target {
		cfg, loadErr := validatedConfig(ctx, data, path, r.target, validate)

		return cfg, report, loadErr
	}
	var plans []ImportPlan
	for i, step := range r.steps {
		destination := BaselineConfigVersion + i + 1
		if destination <= version {
			continue
		}
		if err = ctx.Err(); err != nil {
			return nil, report, fmt.Errorf("migrate config: %w", err)
		}
		start := time.Now()
		if importer, ok := step.(SecretImporter); ok {
			existing, perr := probeExistingAgentID(ctx, dir)
			if perr != nil {
				return nil, report, perr
			}
			plan, perr := importer.PlanImport(doc, ImportContext{AgentDir: dir, ExistingAgentID: existing})
			if perr != nil {
				return nil, report, fmt.Errorf("config %s migration to %d refused: %w", path, destination, perr)
			}
			plans = append(plans, plan)
		}
		if err = step.Up(doc); err != nil {
			// Do not render step errors: they may include a configuration value.
			return nil, report, fmt.Errorf("config %s migration to %d failed: %w", path, destination,
				&privateConfigError{message: "check the step's input requirements", cause: err})
		}
		if err = setDocumentVersion(doc, destination); err != nil {
			return nil, report, err
		}
		report.Steps = append(report.Steps, MigrationStepReport{Version: destination, Description: step.Description(), Duration: time.Since(start)})
	}
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err = enc.Encode(doc); err != nil {
		return nil, report, fmt.Errorf("encode migrated config: %w: %w", ErrInvalidConfigDocument, &privateConfigError{message: "YAML encoding failed", cause: err})
	}
	if err = enc.Close(); err != nil {
		return nil, report, fmt.Errorf("close YAML encoder: %w", err)
	}
	candidate := buf.Bytes()
	cfg, err := validatedConfig(ctx, candidate, path, r.target, validate)
	if err != nil {
		return nil, report, err
	}
	// Secret-import phases (one importer step at most): the values enter the
	// Secret store BEFORE the commit and are idempotent, so a crash here leaves
	// the v1 file and its plaintext files intact and the next boot repeats the
	// work. The backup is sealed to the new Store key.
	committedStore, ierr := importPlans(ctx, dir, plans, &fs)
	if ierr != nil {
		return nil, report, fmt.Errorf("config %s migration refused: %w", path, ierr)
	}
	err = commitMigration(ctx, targetPath, data, candidate, info, &report, fs)
	if err != nil {
		return nil, report, fmt.Errorf("migrate config %s%s: %w", path, report.CommitContext(), err)
	}
	if committedStore != nil && report.Committed {
		// Point of no return passed: delete the recorded plaintext files.
		// Failures only WARN and are retried at every boot.
		RunSecretCleanup(ctx, committedStore, path, zerolog.Nop())
	}

	return cfg, report, nil
}

func validatedConfig(ctx context.Context, data []byte, path string, target int, validate func(*Config) error) (*Config, error) {
	cfg, err := decodeConfig(ctx, data, path, target)
	if err != nil {
		return nil, err
	}
	if err = cfg.Validate(); err != nil {
		return nil, fmt.Errorf("invalid config %s: %w", path, err)
	}
	if validate != nil {
		if err = validate(cfg); err != nil {
			return nil, fmt.Errorf("invalid config %s: %w", path, err)
		}
	}

	return cfg, nil
}

func setDocumentVersion(doc *yaml.Node, version int) error {
	if doc.Kind != yaml.DocumentNode || len(doc.Content) != 1 || doc.Content[0].Kind != yaml.MappingNode {
		return fmt.Errorf("migration changed document root: %w", ErrInvalidConfigDocument)
	}
	root := doc.Content[0]
	for i := 0; i+1 < len(root.Content); i += 2 {
		if root.Content[i].Value != versionKey {
			continue
		}
		key, value := root.Content[i], root.Content[i+1]
		value.Kind, value.Tag, value.Value, value.Style = yaml.ScalarNode, "!!int", fmt.Sprint(version), 0
		root.Content = append([]*yaml.Node{key, value}, append(root.Content[:i:i], root.Content[i+2:]...)...)

		return nil
	}

	return fmt.Errorf("migration removed version marker: %w", ErrInvalidConfigDocument)
}

// importPlans runs the store-write phase of every planned import and, when one
// ran, seals the config backup to the new Store key. It returns the store (nil
// when no step imported anything).
func importPlans(ctx context.Context, dir string, plans []ImportPlan, fs *migrationIO) (*secretstore.Store, error) {
	var last *secretstore.Store
	for _, plan := range plans {
		store, err := importIntoStore(ctx, dir, plan)
		if err != nil {
			return nil, err
		}
		last = store
	}
	if last != nil {
		store := last
		fs.backup = encryptedBackup(func(b []byte) ([]byte, error) { return store.Seal(ctx, b) })
	}

	return last, nil
}
