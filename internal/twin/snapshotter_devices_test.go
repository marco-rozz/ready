//nolint:goconst
package twin

import (
	"strings"
	"testing"
)

// Registry fixture identifiers, factored out to keep the literals in sync.
const (
	areaKitchenID = "area_kitchen"
	areaLivingID  = "area_living"
	areaNoNameID  = "area_noname"
	areaKitchen   = "Kitchen"
	devOvenID     = "dev_oven"
	keyModel      = "model"
	keyName       = "name"
)

// devicesFixture returns a registry snapshot exercising every branch of the
// devices.md rendering: named/unnamed areas, name_by_user override, device
// area inherited from an entity, multi-entity devices and virtual entities.
func devicesFixture() (areas, devices, entities map[string]map[string]any) {
	areas = map[string]map[string]any{
		areaKitchenID: {keyName: areaKitchen},
		areaLivingID:  {keyName: "Living Room"},
		areaNoNameID:  {},
	}

	devices = map[string]map[string]any{
		// Explicit area + name_by_user overriding name.
		"dev_lamp": {"area_id": areaLivingID, keyName: "Generic Lamp", "name_by_user": "Floor Lamp", keyModel: "L1"},
		// No area_id: must inherit from one of its entities.
		devOvenID: {keyName: "Oven", keyModel: "O2"},
		// Area id present but the area has no name: falls back to the raw id.
		"dev_probe": {"ai": areaNoNameID, keyName: "Probe", keyModel: "P3"},
		// No area at all: rendered as unknownArea.
		"dev_orphan": {keyName: "Orphan", keyModel: "X"},
	}

	entities = map[string]map[string]any{
		"light.floor_lamp":     {"di": "dev_lamp", "ai": areaLivingID},
		"sensor.oven_temp":     {"device_id": devOvenID, "area_id": areaKitchenID},
		"switch.oven_power":    {"device_id": devOvenID},
		"sensor.probe":         {"di": "dev_probe"},
		"automation.goodnight": {"pl": "automation"},
		"sensor.sun":           {"ai": areaKitchenID, "platform": "sun"},
	}

	return areas, devices, entities
}

func TestAreaResolver(t *testing.T) {
	resolve := areaResolver(map[string]map[string]any{
		"a1": {keyName: areaKitchen},
		"a2": {},
	})

	cases := map[string]string{
		"":        unknownArea,
		"a1":      areaKitchen,
		"a2":      "a2",
		"missing": "missing",
	}

	for in, want := range cases {
		if got := resolve(in); got != want {
			t.Errorf("resolve(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestGroupEntities(t *testing.T) {
	areas, _, entities := devicesFixture()
	g := groupEntities(entities, areaResolver(areas))

	if got, want := strings.Join(g.byDevice[devOvenID], ","), "sensor.oven_temp,switch.oven_power"; got != want {
		t.Errorf("dev_oven entities = %q, want %q (sorted)", got, want)
	}

	if got := g.areaByDevice[devOvenID]; got != areaKitchenID {
		t.Errorf("dev_oven inherited area = %q, want %s", got, areaKitchenID)
	}

	if _, ok := g.areaByDevice["dev_probe"]; ok {
		t.Error("dev_probe must not inherit an area: its entity declares none")
	}

	if len(g.virtual) != 2 {
		t.Fatalf("expected 2 virtual entities, got %d: %+v", len(g.virtual), g.virtual)
	}

	// Sorted by (area, entity_id): "Kitchen" < unknownArea ("—").
	if g.virtual[0].entityID != "sensor.sun" || g.virtual[0].area != areaKitchen {
		t.Errorf("first virtual row = %+v, want sensor.sun in %s", g.virtual[0], areaKitchen)
	}
	if g.virtual[1].entityID != "automation.goodnight" || g.virtual[1].area != unknownArea {
		t.Errorf("second virtual row = %+v, want automation.goodnight in %s", g.virtual[1], unknownArea)
	}
	if g.virtual[1].platform != "automation" {
		t.Errorf("platform = %q, want automation", g.virtual[1].platform)
	}
}

func TestBuildDeviceRows(t *testing.T) {
	areas, devices, entities := devicesFixture()
	resolve := areaResolver(areas)
	rows := buildDeviceRows(devices, groupEntities(entities, resolve), resolve)

	if len(rows) != len(devices) {
		t.Fatalf("expected %d rows, got %d", len(devices), len(rows))
	}

	// Sorted by (area, name): Kitchen < Living Room < area_noname < "—".
	wantOrder := []string{"Oven", "Floor Lamp", "Probe", "Orphan"}
	for i, want := range wantOrder {
		if rows[i].name != want {
			t.Errorf("row %d name = %q, want %q (rows: %+v)", i, rows[i].name, want, rows)
		}
	}

	byName := map[string]deviceRow{}
	for _, r := range rows {
		byName[r.name] = r
	}

	if got := byName["Oven"].area; got != areaKitchen {
		t.Errorf("Oven area = %q, want %s (inherited from its entity)", got, areaKitchen)
	}
	if got := byName["Oven"].entityList; got != "sensor.oven_temp<br>switch.oven_power" {
		t.Errorf("Oven entityList = %q", got)
	}
	if got := byName["Floor Lamp"].model; got != "L1" {
		t.Errorf("Floor Lamp model = %q, want L1", got)
	}
	if _, ok := byName["Generic Lamp"]; ok {
		t.Error("name_by_user must take precedence over name")
	}
	if got := byName["Probe"].area; got != areaNoNameID {
		t.Errorf("Probe area = %q, want the raw id %s", got, areaNoNameID)
	}
	if got := byName["Orphan"].area; got != unknownArea {
		t.Errorf("Orphan area = %q, want %q", got, unknownArea)
	}
}

func TestRenderDevices_Structure(t *testing.T) {
	areas, devices, entities := devicesFixture()
	out := renderDevices(areas, devices, entities)

	for _, want := range []string{
		"generated: true",
		"inject: false",
		"### Physical Devices",
		"| Area | Device | Model | Entities |",
		"| Kitchen | Oven | O2 | sensor.oven_temp<br>switch.oven_power |",
		"| Living Room | Floor Lamp | L1 | light.floor_lamp |",
		"### Virtual and System Entities",
		"| " + areaKitchen + " | sensor.sun | sun |",
		"| " + unknownArea + " | automation.goodnight | automation |",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered output missing %q:\n%s", want, out)
		}
	}

	if strings.Index(out, "### Physical Devices") > strings.Index(out, "### Virtual and System Entities") {
		t.Error("Physical Devices section must come before Virtual and System Entities")
	}
}

func TestRenderDevices_Deterministic(t *testing.T) {
	areas, devices, entities := devicesFixture()

	first := stripFrontmatter(renderDevices(areas, devices, entities))
	for i := 0; i < 20; i++ {
		if got := stripFrontmatter(renderDevices(areas, devices, entities)); got != first {
			t.Fatalf("rendering is not deterministic across map iterations:\n%s\n---\n%s", first, got)
		}
	}
}

func TestRenderDevices_EmptyRegistries(t *testing.T) {
	out := renderDevices(nil, nil, nil)

	if !strings.Contains(out, "### Physical Devices") || !strings.Contains(out, "### Virtual and System Entities") {
		t.Errorf("empty registries must still emit both table headers:\n%s", out)
	}
}

// stripFrontmatter drops the frontmatter block, whose generated_at timestamp
// is intentionally non-deterministic.
func stripFrontmatter(s string) string {
	parts := strings.SplitN(s, "---\n", 3)
	if len(parts) < 3 {
		return s
	}

	return parts[2]
}
