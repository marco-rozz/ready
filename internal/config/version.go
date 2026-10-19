package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"runtime"
	"strings"

	"gopkg.in/yaml.v3"
)

const (
	// BaselineConfigVersion is the first supported authored configuration.
	BaselineConfigVersion = 1
	// TargetConfigVersion changes only with a corresponding migration step.
	TargetConfigVersion = 1
	versionKey          = "config_version"
)

var (
	ErrInvalidConfigDocument    = errors.New("invalid configuration document")
	ErrMigrationRequired        = errors.New("configuration migration required")
	ErrIncompatibleVersion      = errors.New("configuration schema is newer than this binary")
	ErrInvalidMigrationRegistry = errors.New("invalid configuration migration registry")
	ErrConfigSourceConflict     = errors.New("configuration changed during migration")
)

// privateConfigError preserves error identity without rendering configuration
// values carried by third-party parsers or future migration implementations.
type privateConfigError struct {
	message string
	cause   error
}

func (e *privateConfigError) Error() string { return e.message }
func (e *privateConfigError) Unwrap() error { return e.cause }

// VersionError identifies a version mismatch without containing configuration values.
type VersionError struct {
	Path    string
	Current int
	Target  int
}

func (e *VersionError) Error() string {
	if e.Current > e.Target {
		return fmt.Sprintf("config %s has config_version %d, binary target %d: upgrade READY to a compatible binary", e.Path, e.Current, e.Target)
	}

	shellHint := ""
	if runtime.GOOS == "windows" {
		shellHint = " in PowerShell"
	}

	return fmt.Sprintf("config %s has config_version %d, binary target %d: run %s%s to migrate this file and start the daemon", e.Path, e.Current, e.Target, ServeMigrationCommand(e.Path), shellHint)
}

func (e *VersionError) Unwrap() error {
	if e.Current > e.Target {
		return ErrIncompatibleVersion
	}

	return ErrMigrationRequired
}

// ServeMigrationCommand quotes for POSIX shells or PowerShell on Windows.
func ServeMigrationCommand(path string) string {
	if runtime.GOOS == "windows" {
		return "ready serve --config '" + strings.ReplaceAll(path, "'", "''") + "'"
	}

	return "ready serve --config '" + strings.ReplaceAll(path, "'", "'\"'\"'") + "'"
}

func checkVersion(path string, current, target int) error {
	if current != target {
		return &VersionError{Path: path, Current: current, Target: target}
	}

	return nil
}

func parseVersion(data []byte, path string) (*yaml.Node, int, error) {
	bad := func(reason string) (*yaml.Node, int, error) {
		return nil, 0, fmt.Errorf("config %s: %w: %s; use an explicit positive integer config_version (first supported schema: 1); obsolete layouts require manual editing", path, ErrInvalidConfigDocument, reason)
	}
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	var doc, extra yaml.Node
	if err := decoder.Decode(&doc); err != nil {
		return bad("cannot parse YAML")
	}
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return bad("expected exactly one YAML document")
	}
	if len(doc.Content) != 1 || doc.Content[0].Kind != yaml.MappingNode {
		return bad("root must be a mapping")
	}
	root := doc.Content[0]
	var marker *yaml.Node
	for i := 0; i < len(root.Content); i += 2 {
		if root.Content[i].Value == versionKey {
			if marker != nil {
				return bad("duplicate config_version")
			}
			marker = root.Content[i+1]
		}
	}
	if marker == nil || marker.Kind != yaml.ScalarNode || marker.Tag != "!!int" {
		return bad("missing or non-integer config_version")
	}
	var version int
	if err := marker.Decode(&version); err != nil || version < BaselineConfigVersion {
		return bad("config_version is outside the supported integer range")
	}

	return &doc, version, nil
}
