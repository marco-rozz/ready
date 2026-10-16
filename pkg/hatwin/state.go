package hatwin

import "encoding/json"

// indexStates stores the initial get_states result.
func (t *Twin) indexStates(result any) {
	arr, ok := result.([]any)
	if !ok {
		return
	}
	index := make(map[string]map[string]any, len(arr))
	for _, item := range arr {
		obj := asMap(item)
		if eid := pickStr(obj, "entity_id", "ei"); eid != "" {
			index[eid] = obj
		}
	}
	t.mu.Lock()
	t.states = index
	t.mu.Unlock()
}

// indexAreas stores the area registry.
func (t *Twin) indexAreas(result any) {
	arr, ok := result.([]any)
	if !ok {
		return
	}
	index := make(map[string]map[string]any, len(arr))
	for _, item := range arr {
		obj := asMap(item)
		if aid := pickStr(obj, "area_id", "id", "ai"); aid != "" {
			index[aid] = obj
		}
	}
	t.mu.Lock()
	t.areas = index
	t.mu.Unlock()
}

// indexDevices stores the device registry.
func (t *Twin) indexDevices(result any) {
	arr, ok := result.([]any)
	if !ok {
		return
	}
	index := make(map[string]map[string]any, len(arr))
	for _, item := range arr {
		obj := asMap(item)
		if did := pickStr(obj, "id", "device_id", "di"); did != "" {
			index[did] = obj
		}
	}
	t.mu.Lock()
	t.devices = index
	t.mu.Unlock()
}

// indexEntities stores the entity registry in list_for_display format.
// HA wraps entries in {"entities": [...]}; we also accept a flat array.
func (t *Twin) indexEntities(result any) {
	var entries []any
	if root := asMap(result); len(root) > 0 {
		if arr, ok := root["entities"].([]any); ok {
			entries = arr
		}
	}
	if entries == nil {
		if arr, ok := result.([]any); ok {
			entries = arr
		}
	}

	index := make(map[string]map[string]any, len(entries))
	for _, item := range entries {
		obj := asMap(item)
		if eid := pickStr(obj, "ei", "entity_id"); eid != "" {
			index[eid] = obj
		}
	}
	t.mu.Lock()
	t.entities = index
	t.mu.Unlock()
}

// indexConfig stores the raw HA config object.
func (t *Twin) indexConfig(result any) {
	if obj, ok := result.(map[string]any); ok {
		t.mu.Lock()
		t.haConfig = obj
		t.mu.Unlock()
	}
}

// --- package-internal helpers ---

// asMap casts v to map[string]any; returns empty map on failure.
func asMap(v any) map[string]any {
	if m, ok := v.(map[string]any); ok {
		return m
	}

	return map[string]any{}
}

// pickStr returns the first non-empty string value found for the given keys.
func pickStr(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if v, ok := m[k]; ok {
			if s := toString(v); s != "" {
				return s
			}
		}
	}

	return ""
}

// toString converts common JSON scalar types to string.
func toString(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case json.Number:
		return x.String()
	default:
		return ""
	}
}

// asInt converts common JSON numeric types to int.
func asInt(v any) int {
	switch x := v.(type) {
	case int:
		return x
	case int64:
		return int(x)
	case float64:
		return int(x)
	case json.Number:
		i, _ := x.Int64()

		return int(i)
	default:
		return 0
	}
}

// entityAreaID returns the area_id for an entity by checking the entity
// registry entry first, then falling back to the device registry.
// Must be called under at least a read lock on t.mu.
func (t *Twin) entityAreaID(entityID string) string {
	if ent, ok := t.entities[entityID]; ok {
		if ai := pickStr(ent, "ai", "area_id"); ai != "" {
			return ai
		}
		if di := pickStr(ent, "di", "device_id"); di != "" {
			if dev, ok := t.devices[di]; ok {
				return pickStr(dev, "area_id", "ai")
			}
		}
	}

	return ""
}

// areaName returns the human-readable area name for an area_id.
// Must be called under at least a read lock on t.mu.
func (t *Twin) areaName(areaID string) string {
	if a, ok := t.areas[areaID]; ok {
		return pickStr(a, "name")
	}

	return ""
}
