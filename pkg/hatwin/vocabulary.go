//nolint:goconst
package hatwin

import (
	"fmt"
	"sort"
	"strings"
)

// Intent vocabulary (twin-native-actuation D5): a static, pure
// domain→action→service table owned by pkg/hatwin. `lock` and
// `alarm_control_panel` are deliberately absent — they may require a PIN
// `code` that would land in transcripts and logs. `siren` is present but
// inactive unless the operator explicitly lists it.
//
// `set` is one parametric verb validated per domain.

// paramKind selects how a fixed-service set parameter is validated.
type paramKind int

const (
	paramNumber paramKind = iota // numeric, range-checked (strings pass through)
	paramString                  // non-empty string
	paramTuple                   // fixed-length numeric array, per-element ranges
)

// paramSpec describes one valid parameter of a parametric action.
type paramSpec struct {
	Name string
	Min  float64 // inclusive lower bound (NaN = unbounded)
	Max  float64 // inclusive upper bound (NaN = unbounded)
	Kind paramKind
	// Tuple holds the per-element ranges of a paramTuple (its length is the
	// required array length).
	Tuple [][2]float64
	// Hint, when set, replaces the rendered range in descriptions/errors.
	Hint string
}

func (p paramSpec) describe() string {
	if p.Hint != "" {
		return fmt.Sprintf("%s (%s)", p.Name, p.Hint)
	}

	switch {
	case isNaN(p.Min) && isNaN(p.Max):
		return p.Name
	case isNaN(p.Min):
		return fmt.Sprintf("%s (≤ %g)", p.Name, p.Max)
	case isNaN(p.Max):
		return fmt.Sprintf("%s (≥ %g)", p.Name, p.Min)
	default:
		return fmt.Sprintf("%s (%g–%g)", p.Name, p.Min, p.Max)
	}
}

func isNaN(f float64) bool { return f != f }

// domainVocabulary is the static action/parameter table for one domain.
type domainVocabulary struct {
	Domain  string
	Actions []string // ordered, for description rendering
	// services maps action → HA service for fixed-service actions.
	services map[string]string
	// SetParams are the parameters valid for the `set` action (empty = set
	// accepts no parameters).
	SetParams []paramSpec
}

// commandTable is the single source of the intent vocabulary.
var commandTable = []domainVocabulary{ //nolint:gochecknoglobals // pure static table (D5)
	{
		Domain:  "light",
		Actions: []string{"on", "off", "toggle", "set"},
		services: map[string]string{
			"on": "turn_on", "off": "turn_off", "toggle": "toggle", "set": "turn_on",
		},
		SetParams: []paramSpec{
			{Name: "brightness", Min: 0, Max: 255},
			{Name: "color_temp", Min: 1, Max: 500},
			{Name: "color_temp_kelvin", Min: 1000, Max: 10000},
			{Name: "rgb_color", Kind: paramTuple, Tuple: [][2]float64{{0, 255}, {0, 255}, {0, 255}}, Hint: "[r, g, b], each 0–255"},
			{Name: "hs_color", Kind: paramTuple, Tuple: [][2]float64{{0, 360}, {0, 100}}, Hint: "[hue 0–360, saturation 0–100]"},
			{Name: "color_name", Kind: paramString, Hint: "CSS colour name, e.g. green"},
			{Name: "transition", Min: 0, Max: 300},
			{Name: "flash", Min: 0, Max: 2},
		},
	},
	{
		Domain:  "switch",
		Actions: []string{"on", "off", "toggle", "set"},
		services: map[string]string{
			"on": "turn_on", "off": "turn_off", "toggle": "toggle", "set": "turn_on",
		},
		// switch.set is turn_on with no parameters: any param is invalid.
	},
	{
		Domain:  "cover",
		Actions: []string{"open", "close", "stop", "set"},
		services: map[string]string{
			"open": "open_cover", "close": "close_cover", "stop": "stop_cover", "set": "set_cover_position",
		},
		SetParams: []paramSpec{
			{Name: "position", Min: 0, Max: 100},
		},
	},
	{
		Domain:  "climate",
		Actions: []string{"set", "off"},
		services: map[string]string{
			"off": "turn_off",
		},
		SetParams: []paramSpec{
			{Name: "temperature", Min: -50, Max: 100},
			{Name: "target_temp_high", Min: -50, Max: 100},
			{Name: "target_temp_low", Min: -50, Max: 100},
			{Name: "hvac_mode", Min: 0, Max: 0}, // string; range ignored
		},
	},
	{
		Domain:  "fan",
		Actions: []string{"on", "off", "toggle", "set"},
		services: map[string]string{
			"on": "turn_on", "off": "turn_off", "toggle": "toggle",
		},
		SetParams: []paramSpec{
			{Name: "percentage", Min: 0, Max: 100},
			{Name: "preset_mode", Min: 0, Max: 0}, // string
		},
	},
	{
		Domain:  "media_player",
		Actions: []string{"on", "off", "play", "pause", "next", "previous", "set"},
		services: map[string]string{
			"on": "turn_on", "off": "turn_off",
			"play": "media_play", "pause": "media_pause",
			"next": "media_next_track", "previous": "media_previous_track",
		},
		SetParams: []paramSpec{
			{Name: "volume_level", Min: 0, Max: 1},
		},
	},
	{
		Domain:  "scene",
		Actions: []string{"activate"},
		services: map[string]string{
			"activate": "turn_on",
		},
	},
	{
		Domain:  "script",
		Actions: []string{"run"},
		services: map[string]string{
			"run": "turn_on",
		},
	},
	{
		Domain:  "button",
		Actions: []string{"press"},
		services: map[string]string{
			"press": "press",
		},
	},
	{
		Domain:  "siren",
		Actions: []string{"on", "off"},
		services: map[string]string{
			"on": "turn_on", "off": "turn_off",
		},
	},
}

// DefaultCommandDomains is the active set when the operator does not
// configure home_assistant.commands.domains: every table domain except siren.
func DefaultCommandDomains() []string {
	var out []string
	for _, d := range commandTable {
		if d.Domain != "siren" {
			out = append(out, d.Domain)
		}
	}

	return out
}

// KnownCommandDomains returns every domain the vocabulary table knows.
func KnownCommandDomains() []string {
	out := make([]string, 0, len(commandTable))
	for _, d := range commandTable {
		out = append(out, d.Domain)
	}

	return out
}

// vocabFor returns the table entry for a domain, or nil.
func vocabFor(domain string) *domainVocabulary {
	for i := range commandTable {
		if commandTable[i].Domain == domain {
			return &commandTable[i]
		}
	}

	return nil
}

// resolveIntent maps an intent (domain, action, params) to an HA service and
// service_data. Errors are guided: they name what the model can use instead.
func resolveIntent(domain, action string, params map[string]any) (service string, data map[string]any, err error) {
	vocab := vocabFor(domain)
	if vocab == nil {
		return "", nil, fmt.Errorf("%w: domain %q is not supported; domains without a PIN requirement only — see the tool description for the active vocabulary", ErrOutOfVocabulary, domain)
	}

	found := false
	for _, a := range vocab.Actions {
		if a == action {
			found = true

			break
		}
	}
	if !found {
		return "", nil, fmt.Errorf("%w: domain %q supports actions %s, not %q", ErrOutOfVocabulary, domain, strings.Join(vocab.Actions, ", "), action)
	}

	if action != "set" {
		if len(params) > 0 {
			return "", nil, fmt.Errorf("%w: action %q on domain %q takes no parameters", ErrInvalidParams, action, domain)
		}

		return vocab.services[action], nil, nil
	}

	return resolveSetIntent(vocab, params)
}

// resolveSetIntent validates and resolves the parametric `set` action for a
// domain.
func resolveSetIntent(vocab *domainVocabulary, params map[string]any) (service string, data map[string]any, err error) {
	domain := vocab.Domain

	var invalid []string
	for name := range params {
		valid := false
		for _, p := range vocab.SetParams {
			if p.Name == name {
				valid = true

				break
			}
		}
		if !valid {
			invalid = append(invalid, name)
		}
	}
	if len(invalid) > 0 {
		sort.Strings(invalid)

		valid := make([]string, 0, len(vocab.SetParams))
		for _, p := range vocab.SetParams {
			valid = append(valid, p.describe())
		}
		if len(valid) == 0 {
			valid = []string{"(none — this domain has no parametric set)"}
		}

		return "", nil, fmt.Errorf("%w: parameter(s) %s not valid for %s.set; valid parameters: %s",
			ErrInvalidParams, strings.Join(invalid, ", "), domain, strings.Join(valid, ", "))
	}

	// Domain-specific service resolution.
	switch domain {
	case "climate":
		_, hasTemp := params["temperature"]
		_, hasHigh := params["target_temp_high"]
		_, hasLow := params["target_temp_low"]
		if mode := pickStr(params, "hvac_mode"); mode != "" {
			if hasTemp || hasHigh || hasLow {
				return "set_temperature", params, nil
			}

			return "set_hvac_mode", map[string]any{"hvac_mode": mode}, nil
		}
		if hasTemp || hasHigh || hasLow {
			return "set_temperature", params, nil
		}

		return "", nil, fmt.Errorf("%w: climate.set needs hvac_mode or temperature/target_temp_high/target_temp_low", ErrInvalidParams)
	case "fan":
		_, hasPct := params["percentage"]
		_, hasPreset := params["preset_mode"]
		if hasPct && hasPreset {
			return "", nil, fmt.Errorf("%w: fan.set takes percentage or preset_mode, not both", ErrInvalidParams)
		}
		if hasPreset {
			return "set_preset_mode", params, nil
		}
		if hasPct {
			return "set_percentage", params, nil
		}

		return "", nil, fmt.Errorf("%w: fan.set needs percentage or preset_mode", ErrInvalidParams)
	}

	if domain == "light" {
		if err := checkExclusiveLightColor(params); err != nil {
			return "", nil, err
		}
	}

	if err := validateFixedSetParams(vocab.SetParams, params); err != nil {
		return "", nil, err
	}

	return vocab.services["set"], params, nil
}

// validateFixedSetParams validates (and normalises in place) the parameters
// of a fixed-service parametric set (light, cover, media_player) per kind.
func validateFixedSetParams(specs []paramSpec, params map[string]any) error {
	for _, p := range specs {
		v, ok := params[p.Name]
		if !ok {
			continue
		}
		switch p.Kind {
		case paramString:
			if str, isStr := v.(string); !isStr || strings.TrimSpace(str) == "" {
				return fmt.Errorf("%w: %s must be a non-empty string (%s)", ErrInvalidParams, p.Name, p.Hint)
			}
		case paramTuple:
			tuple, err := validateTuple(p, v)
			if err != nil {
				return err
			}
			params[p.Name] = tuple
		default:
			num, isNum, err := validateNumber(p, v)
			if err != nil {
				return err
			}
			if isNum {
				params[p.Name] = num
			}
		}
	}

	return nil
}

// lightColorParams are mutually exclusive in HA's light.turn_on (one colour
// mode per call); HA rejects a call carrying two of them.
var lightColorParams = []string{"color_temp", "color_temp_kelvin", "rgb_color", "hs_color", "color_name"} //nolint:gochecknoglobals // pure static table (D5)

func checkExclusiveLightColor(params map[string]any) error {
	var given []string
	for _, name := range lightColorParams {
		if _, ok := params[name]; ok {
			given = append(given, name)
		}
	}
	if len(given) > 1 {
		return fmt.Errorf("%w: light.set takes at most one colour parameter (got %s); choose one of %s",
			ErrInvalidParams, strings.Join(given, ", "), strings.Join(lightColorParams, ", "))
	}

	return nil
}

// validateTuple checks a fixed-length numeric array against per-element
// ranges, returning it as []float64.
func validateTuple(p paramSpec, v any) ([]float64, error) {
	items, ok := v.([]any)
	if !ok || len(items) != len(p.Tuple) {
		return nil, fmt.Errorf("%w: %s must be an array %s", ErrInvalidParams, p.Name, p.Hint)
	}

	out := make([]float64, len(items))
	for i, item := range items {
		num, isNum := item.(float64)
		if !isNum || num < p.Tuple[i][0] || num > p.Tuple[i][1] {
			return nil, fmt.Errorf("%w: %s must be an array %s (element %d = %v)", ErrInvalidParams, p.Name, p.Hint, i, item)
		}
		out[i] = num
	}

	return out, nil
}

// validateNumber coerces JSON numeric values and enforces a paramSpec range.
func validateNumber(p paramSpec, v any) (float64, bool, error) {
	switch x := v.(type) {
	case float64:
		if !isNaN(p.Min) && x < p.Min || !isNaN(p.Max) && x > p.Max {
			return 0, false, fmt.Errorf("%w: %s = %g out of range %s", ErrInvalidParams, p.Name, x, p.describe())
		}

		return x, true, nil
	case int:
		return float64(x), true, nil
	case string:
		// String values pass through (e.g. preset modes, effects) — range
		// specs apply to numeric parameters only.
		return 0, false, nil
	default:
		return 0, false, nil
	}
}

// RenderVocabularyDescription renders the active domains and their actions
// for the tool description (static for the process lifetime — prompt-cache
// safe). An empty active set renders an explicit "no domains" notice.
func RenderVocabularyDescription(active []string) string {
	activeSet := make(map[string]bool, len(active))
	for _, d := range active {
		activeSet[d] = true
	}

	var b strings.Builder
	b.WriteString("Active domains and actions:")
	var listed bool
	for _, d := range commandTable {
		if !activeSet[d.Domain] {
			continue
		}
		listed = true
		actions := append([]string(nil), d.Actions...)
		sort.Strings(actions)
		fmt.Fprintf(&b, "\n- %s: %s", d.Domain, strings.Join(actions, ", "))
		if len(d.SetParams) > 0 {
			var ps []string
			for _, p := range d.SetParams {
				ps = append(ps, p.describe())
			}
			fmt.Fprintf(&b, " (set params: %s)", strings.Join(ps, ", "))
		}
	}
	if !listed {
		b.WriteString(" none (the operator disabled every command domain — this tool cannot serve any request)")
	}

	return b.String()
}
