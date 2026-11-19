// definition.go — Subagent definitions as assets (openspec change
// subagent-runtime, task 1.4, design D3): `*.md` files with YAML frontmatter
// in <agent-dir>/subagents/, library-synced like skills. A definition's body
// is its system-prompt section; the frontmatter carries name, description,
// the tool restriction (tools XOR disallowedTools — only restricting, never
// expanding), the optional model pin (inherit or a chain member),
// max_iterations, timeout, preloaded skills, and visibility (public default,
// internal = job-only). The fields thinking/mcp_servers/isolation/memory
// are parsed and reserved: accepted, never applied.
//
// Definitions are loaded at boot and revalidated at runtime reload only —
// no fsnotify watcher — so the subagent_spawn description is stable per
// generation. A file that fails validation at boot fails boot naming the
// file; at reload it keeps its previous version with a WARN.
package subagent

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Visibility values.
const (
	VisibilityPublic   = "public"
	VisibilityInternal = "internal"
)

// ModelInherit is the default model value: the full candidate chain.
const ModelInherit = "inherit"

// DefaultMaxIterations is the iteration cap when a definition sets none
// (mirrors internal/agent.DefaultMaxIterations).
const DefaultMaxIterations = 25

// Definition is a parsed, validated Subagent definition. Tools /
// DisallowedTools are glob patterns matched against tool wire names
// (unprefixed, internal tools included) with the profile allow/deny matcher,
// applied after the profile filter; they can only restrict.
type Definition struct {
	// Name is the definition's type name (= the spawnable type).
	Name string `yaml:"name"`
	// Description is shown in the subagent_spawn type list.
	Description string `yaml:"description"`
	// Tools is the allowlist glob set (XOR DisallowedTools).
	Tools []string `yaml:"tools"`
	// DisallowedTools is the denylist glob set (XOR Tools).
	DisallowedTools []string `yaml:"disallowed_tools"`
	// Model is "inherit" (default) or a <provider>::<model> reference that
	// must be a member of the compiled candidate chain (validated against
	// the chain at boot and spawn).
	Model string `yaml:"model"`
	// MaxIterations bounds one dispatch (0 = DefaultMaxIterations).
	MaxIterations int `yaml:"max_iterations"`
	// Timeout bounds one dispatch (0 = the main agent's run deadline).
	Timeout time.Duration `yaml:"timeout"`
	// Skills are preloaded into the child prompt at spawn.
	Skills []string `yaml:"skills"`
	// Visibility hides internal definitions from the model.
	Visibility string `yaml:"visibility"`

	// Reserved fields: parsed, never applied.
	Thinking   any `yaml:"thinking"`
	MCPServers any `yaml:"mcp_servers"`
	Isolation  any `yaml:"isolation"`
	Memory     any `yaml:"memory"`

	// Body is everything after the frontmatter — the definition's
	// system-prompt section.
	Body string `yaml:"-"`

	// File is the source file path (boot errors name it).
	File string `yaml:"-"`
}

// EffectiveMaxIterations returns the iteration cap.
func (d Definition) EffectiveMaxIterations() int {
	if d.MaxIterations <= 0 {
		return DefaultMaxIterations
	}

	return d.MaxIterations
}

// EffectiveModel returns the model reference ("inherit" when unset).
func (d Definition) EffectiveModel() string {
	if d.Model == "" {
		return ModelInherit
	}

	return d.Model
}

// Internal reports a job-only definition.
func (d Definition) Internal() bool { return d.Visibility == VisibilityInternal }

// validate checks the definition invariants; the error names the file.
func (d Definition) validate() error {
	if !ValidName(d.Name) {
		return fmt.Errorf("definition %s: name %q must match [a-z0-9-] and not be reserved", d.File, d.Name)
	}

	if strings.TrimSpace(d.Description) == "" {
		return fmt.Errorf("definition %s: description is required", d.File)
	}

	if len(d.Tools) > 0 && len(d.DisallowedTools) > 0 {
		return fmt.Errorf("definition %s: tools and disallowed_tools are mutually exclusive", d.File)
	}

	for _, g := range d.Tools {
		if strings.TrimSpace(g) == "" {
			return fmt.Errorf("definition %s: empty tools entry", d.File)
		}
	}

	for _, g := range d.DisallowedTools {
		if strings.TrimSpace(g) == "" {
			return fmt.Errorf("definition %s: empty disallowed_tools entry", d.File)
		}
	}

	switch v := d.Visibility; v {
	case "", VisibilityPublic, VisibilityInternal:
	default:
		return fmt.Errorf("definition %s: unknown visibility %q", d.File, v)
	}

	if d.MaxIterations < 0 {
		return fmt.Errorf("definition %s: max_iterations must be positive", d.File)
	}

	if d.Timeout < 0 {
		return fmt.Errorf("definition %s: timeout must be positive", d.File)
	}

	if d.Model != "" && d.Model != ModelInherit {
		if !strings.Contains(d.Model, "::") {
			return fmt.Errorf("definition %s: model %q must be %q or a <provider>::<model> reference", d.File, d.Model, ModelInherit)
		}
	}

	for _, s := range d.Skills {
		if strings.TrimSpace(s) == "" {
			return fmt.Errorf("definition %s: empty skills entry", d.File)
		}
	}

	return nil
}

// LoadDefinitions parses every *.md under dir. A missing directory loads as
// empty; any invalid file is a validation error naming the file (boot is
// fatal; reload callers keep the previous set and WARN).
func LoadDefinitions(dir string) ([]Definition, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}

		return nil, fmt.Errorf("failed to read subagent definitions %q: %w", dir, err)
	}

	var out []Definition

	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") || strings.HasPrefix(e.Name(), ".") {
			continue
		}

		path := filepath.Join(dir, e.Name())

		d, derr := LoadDefinition(path)
		if derr != nil {
			return nil, derr
		}

		out = append(out, d)
	}

	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })

	return out, nil
}

// LoadDefinition parses and validates one definition file.
func LoadDefinition(path string) (Definition, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Definition{}, fmt.Errorf("failed to read definition %s: %w", path, err)
	}

	fm, body, err := splitFrontmatter(raw)
	if err != nil {
		return Definition{}, fmt.Errorf("definition %s: %w", path, err)
	}

	var d Definition
	if err := yaml.Unmarshal(fm, &d); err != nil {
		return Definition{}, fmt.Errorf("definition %s: frontmatter: %w", path, err)
	}

	d.Body = string(body)
	d.File = path

	if err := d.validate(); err != nil {
		return Definition{}, err
	}

	return d, nil
}

// splitFrontmatter splits a leading `---\n...\n---\n` YAML block from the
// body. Both `---` fences are required.
func splitFrontmatter(raw []byte) (front, body []byte, err error) {
	nl := bytes.IndexByte(raw, '\n')
	if nl < 0 || strings.TrimSpace(string(raw[:nl])) != "---" {
		return nil, nil, fmt.Errorf("missing YAML frontmatter (leading ---)")
	}

	end := bytes.Index(raw[nl+1:], []byte("\n---"))
	if end < 0 {
		return nil, nil, fmt.Errorf("frontmatter is not closed with ---")
	}

	front = raw[nl+1 : nl+1+end]

	// rest starts at the closing fence's "\n---".
	rest := raw[nl+1+end:]
	if bytes.HasPrefix(rest, []byte("\n---\n")) {
		return front, rest[len("\n---\n"):], nil
	}

	if bytes.Equal(rest, []byte("\n---")) {
		return front, nil, nil
	}

	return nil, nil, fmt.Errorf("frontmatter closing fence must be exactly ---")
}

// Definitions holds the loaded set with lookup helpers.
type Definitions struct {
	byName map[string]Definition
	order  []string
}

// NewDefinitions indexes the parsed set (invalid files must already have
// been refused by LoadDefinitions).
func NewDefinitions(defs []Definition) *Definitions {
	d := &Definitions{byName: map[string]Definition{}}
	for _, def := range defs {
		if _, dup := d.byName[def.Name]; !dup {
			d.order = append(d.order, def.Name)
		}

		d.byName[def.Name] = def
	}

	return d
}

// Get returns the definition by type name.
func (d *Definitions) Get(name string) (Definition, bool) {
	def, ok := d.byName[name]

	return def, ok
}

// PublicTypes returns the public definitions in name order — the
// subagent_spawn type list (internal ones are job-only).
func (d *Definitions) PublicTypes() []Definition {
	out := make([]Definition, 0, len(d.order))
	for _, n := range d.order {
		if def := d.byName[n]; !def.Internal() {
			out = append(out, def)
		}
	}

	return out
}

// DefaultType returns the default spawn type (the shipped general-purpose
// definition; the first public definition otherwise).
func (d *Definitions) DefaultType() (Definition, bool) {
	for _, def := range d.PublicTypes() {
		if def.Name == "general-purpose" {
			return def, true
		}
	}

	pub := d.PublicTypes()
	if len(pub) > 0 {
		return pub[0], true
	}

	return Definition{}, false
}
