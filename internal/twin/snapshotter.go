package twin

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/marco-rozz/ready/pkg/hatwin"
	"github.com/rs/zerolog"
)

// snapshotGenerator identifies this component in machine-generated frontmatter.
const snapshotGenerator = "hatwin-snapshotter"

// snapshotSource identifies the origin of generated skill assets.
const snapshotSource = "home-assistant-twin"

// SkillInvalidator is satisfied by *bootstrap.Bootstrapper. Defined here to
// avoid a circular import between internal/twin and internal/bootstrap.
type SkillInvalidator interface {
	Invalidate(skillName string)
	Evict(path string)
}

// Snapshotter generates and refreshes the home-topology skill assets from the
// live digital twin. It writes two files to <skillDir>/generated/, each
// carrying a `generated: true` frontmatter block so skillcache knows not to
// attach a fsnotify watcher (the writer is the sole invalidation authority):
//
//   - ha-config.md  — HA instance metadata; injected into the static shell.
//   - devices.md    — full device/entity table; marked inject:false (tool-readable only).
//
// After each write it calls SkillInvalidator.Invalidate("home-topology")
// unconditionally — even on partial write failure — so the cache never
// serves a stale copy that masks what's actually on disk.
type Snapshotter struct {
	twin        *hatwin.Twin
	skillDir    string
	invalidator SkillInvalidator
	log         zerolog.Logger
}

// NewSnapshotter creates a Snapshotter. skillDir is the absolute path returned
// by skillsMgr.GetSkillLocation("home-topology"). inv is called after each
// successful write to invalidate the bootstrap static cache.
func NewSnapshotter(t *hatwin.Twin, skillDir string, inv SkillInvalidator, logger zerolog.Logger) *Snapshotter {
	return &Snapshotter{
		twin:        t,
		skillDir:    skillDir,
		invalidator: inv,
		log:         logger.With().Str("component", "twin.snapshotter").Logger(),
	}
}

// Start launches the background goroutine. It waits for the twin's initial sync,
// generates a first snapshot, then regenerates on every topology-change signal.
// The goroutine exits when ctx is cancelled. No-op when twin is nil.
func (s *Snapshotter) Start(ctx context.Context) {
	if s.twin == nil {
		return
	}
	go s.run(ctx)
}

func (s *Snapshotter) run(ctx context.Context) {
	if err := s.twin.WaitReady(ctx); err != nil {
		s.log.Warn().Msg("HA twin not ready before context cancelled — snapshots not generated")

		return
	}

	if err := s.generate(); err != nil {
		s.log.Warn().Err(err).Msg("initial snapshot generation failed")
	}

	for {
		select {
		case <-ctx.Done():
			return
		case <-s.twin.TopologyChanges():
			// ConsumeTopologyDirty CAS-loops so a burst of registry events
			// arriving faster than a single generate() cycle is not silently
			// dropped by the size-1 topologyCh: at least one extra cycle runs
			// for every distinct dirty window. Break out on error to avoid
			// busy-looping against a persistent failure (e.g. disk full).
			for s.twin.ConsumeTopologyDirty() {
				if err := s.generate(); err != nil {
					s.log.Warn().Err(err).Msg("snapshot regeneration failed after topology change")

					break
				}
			}
		}
	}
}

func (s *Snapshotter) generate() error {
	generatedDir := s.skillDir + "/generated"
	if err := os.MkdirAll(generatedDir, 0o750); err != nil {
		return fmt.Errorf("create generated dir: %w", err)
	}

	haConfigPath := generatedDir + "/ha-config.md"
	devicesPath := generatedDir + "/devices.md"

	var errs []error

	if err := s.writeHAConfig(generatedDir); err != nil {
		errs = append(errs, fmt.Errorf("write ha-config.md: %w", err))
	}

	if err := s.writeDevices(generatedDir); err != nil {
		errs = append(errs, fmt.Errorf("write devices.md: %w", err))
	}

	joined := errors.Join(errs...)

	// Invalidate unconditionally — even on partial failure — so the cache
	// reflects what's actually on disk rather than masking a fresh write
	// behind a stale cached copy.
	s.invalidator.Invalidate("home-topology")

	// criticalities-remediation (H8): evict the skillcache content entries
	// for the generated files after every write — skillcache does NOT watch
	// generated files (the writer is the sole invalidation authority), so
	// without this the rebuilt static shell keeps reading the bytes cached
	// at the first bootstrap and the twin-driven refresh is silently
	// defeated until a daemon restart.
	s.invalidator.Evict(haConfigPath)
	s.invalidator.Evict(devicesPath)

	if joined != nil {
		s.log.Warn().Err(joined).Msg("home-topology snapshot generation had partial write failures")

		return joined
	}

	s.log.Info().Str("skill_dir", s.skillDir).Msg("home-topology snapshots updated")

	return nil
}

// writeGeneratedFrontmatter emits the machine-generated marker block shared
// by every Snapshotter output file. It is orthogonal to inject: the same four
// fields appear whether or not the file is injected into the static shell.
func writeGeneratedFrontmatter(sb *strings.Builder, inject bool) {
	sb.WriteString("---\n")
	sb.WriteString("generated: true\n")
	_, _ = fmt.Fprintf(sb, "generator: %s\n", snapshotGenerator)
	_, _ = fmt.Fprintf(sb, "generated_at: %s\n", time.Now().UTC().Format(time.RFC3339))
	_, _ = fmt.Fprintf(sb, "source: %s\n", snapshotSource)

	if !inject {
		sb.WriteString("inject: false\n")
	}

	sb.WriteString("---\n\n")
}

func (s *Snapshotter) writeHAConfig(generatedDir string) error {
	cfg := s.twin.Config()

	var sb strings.Builder
	writeGeneratedFrontmatter(&sb, true)
	sb.WriteString("## Home Assistant — Instance Configuration\n\n")
	if cfg.Name != "" {
		_, _ = fmt.Fprintf(&sb, "- **Name**: %s\n", cfg.Name)
	}
	if cfg.LocationName != "" {
		_, _ = fmt.Fprintf(&sb, "- **Location**: %s\n", cfg.LocationName)
	}
	if cfg.TimeZone != "" {
		_, _ = fmt.Fprintf(&sb, "- **Timezone**: %s\n", cfg.TimeZone)
	}
	if cfg.Version != "" {
		_, _ = fmt.Fprintf(&sb, "- **HA Version**: %s\n", cfg.Version)
	}

	return os.WriteFile(generatedDir+"/ha-config.md", []byte(sb.String()), 0o600)
}

// unknownArea is rendered when an entity or device has no area assigned.
const unknownArea = "—"

// deviceRow is one line of the "Physical Devices" table.
type deviceRow struct{ area, name, model, entityList string }

// virtualRow is one line of the "Virtual and System Entities" table (entities
// not backed by a physical device).
type virtualRow struct{ area, entityID, platform string }

// entityGrouping is the result of splitting the entity registry between
// device-backed entities and virtual ones.
type entityGrouping struct {
	// byDevice maps device_id to its sorted entity_id list.
	byDevice map[string][]string
	// areaByDevice maps device_id to an area inherited from one of its entities.
	areaByDevice map[string]string
	// virtual holds entities with no device_id, already area-resolved.
	virtual []virtualRow
}

func (s *Snapshotter) writeDevices(generatedDir string) error {
	content := renderDevices(s.twin.Areas(), s.twin.Devices(), s.twin.Entities())

	return os.WriteFile(generatedDir+"/devices.md", []byte(content), 0o600)
}

// renderDevices builds the full devices.md body (frontmatter included) from the
// three registry snapshots. Pure function: no twin, no filesystem.
func renderDevices(areas, devices, entities map[string]map[string]any) string {
	resolveArea := areaResolver(areas)
	grouping := groupEntities(entities, resolveArea)
	rows := buildDeviceRows(devices, grouping, resolveArea)

	var sb strings.Builder
	writeGeneratedFrontmatter(&sb, false)
	sb.WriteString("### Physical Devices\n")
	sb.WriteString("| Area | Device | Model | Entities |\n")
	sb.WriteString("| :--- | :--- | :--- | :--- |\n")
	for _, r := range rows {
		_, _ = fmt.Fprintf(&sb, "| %s | %s | %s | %s |\n", r.area, r.name, r.model, r.entityList)
	}
	sb.WriteString("\n### Virtual and System Entities\n")
	sb.WriteString("| Area | Entity | Platform |\n")
	sb.WriteString("| :--- | :--- | :--- |\n")
	for _, v := range grouping.virtual {
		_, _ = fmt.Fprintf(&sb, "| %s | %s | %s |\n", v.area, v.entityID, v.platform)
	}

	return sb.String()
}

// areaResolver returns a lookup that maps an area_id to its human-readable
// name, falling back to the raw id and finally to unknownArea.
func areaResolver(areas map[string]map[string]any) func(string) string {
	return func(areaID string) string {
		if areaID == "" {
			return unknownArea
		}
		if name := snapPickStr(areas[areaID], "name"); name != "" {
			return name
		}

		return areaID
	}
}

// groupEntities splits the entity registry into device-backed entities and
// virtual ones. Virtual rows are returned sorted by (area, entity_id).
func groupEntities(entities map[string]map[string]any, resolveArea func(string) string) entityGrouping {
	g := entityGrouping{
		byDevice:     map[string][]string{},
		areaByDevice: map[string]string{},
	}

	for eid, ent := range entities {
		areaID := snapPickStr(ent, "ai", "area_id")

		did := snapPickStr(ent, "di", "device_id")
		if did == "" {
			g.virtual = append(g.virtual, virtualRow{
				area:     resolveArea(areaID),
				entityID: eid,
				platform: snapPickStr(ent, "pl", "platform"),
			})

			continue
		}

		g.byDevice[did] = append(g.byDevice[did], eid)
		if areaID != "" {
			g.areaByDevice[did] = areaID
		}
	}

	for did := range g.byDevice {
		sort.Strings(g.byDevice[did])
	}

	sort.Slice(g.virtual, func(i, j int) bool {
		if g.virtual[i].area != g.virtual[j].area {
			return g.virtual[i].area < g.virtual[j].area
		}

		return g.virtual[i].entityID < g.virtual[j].entityID
	})

	return g
}

// buildDeviceRows renders one row per registry device, sorted by (area, name).
// A device without its own area_id inherits the area of one of its entities.
func buildDeviceRows(
	devices map[string]map[string]any,
	grouping entityGrouping,
	resolveArea func(string) string,
) []deviceRow {
	rows := make([]deviceRow, 0, len(devices))

	for did, dev := range devices {
		areaID := snapFirstNonEmpty(snapPickStr(dev, "area_id", "ai"), grouping.areaByDevice[did])
		rows = append(rows, deviceRow{
			area:       resolveArea(areaID),
			name:       snapFirstNonEmpty(snapPickStr(dev, "name_by_user"), snapPickStr(dev, "name")),
			model:      snapPickStr(dev, "model"),
			entityList: strings.Join(grouping.byDevice[did], "<br>"),
		})
	}

	sort.Slice(rows, func(i, j int) bool {
		if rows[i].area != rows[j].area {
			return rows[i].area < rows[j].area
		}

		return rows[i].name < rows[j].name
	})

	return rows
}

// snapPickStr picks the first non-empty string value from a map by trying each key.
func snapPickStr(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if v, ok := m[k]; ok {
			if s, ok2 := v.(string); ok2 && s != "" {
				return s
			}
		}
	}

	return ""
}

// snapFirstNonEmpty returns the first non-blank string from the given values.
func snapFirstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}

	return ""
}
