package okf

import "strings"

// NoExecutionGuidance is appended to every Attested Computation read: the
// daemon never executes a computation, executor or attester.
const NoExecutionGuidance = "Execution of computations, executors and attesters is not available in this daemon, " +
	"and no substitute computation may be written or run. A value obtained any other way cannot be shown as attested."

// Param is one declared computation parameter.
type Param struct {
	Name     string
	Type     string
	Required bool
}

// ComputationInfo is the mechanical report of an Attested Computation.
type ComputationInfo struct {
	Runtime          string
	Params           []Param
	Location         string // "field <path>", "section Computation", or "" when missing
	LocationMissing  bool   // the computation field names a file that does not exist
	Block            string // the located inline block (or empty)
	BlockInfo        string // fence info string
	ExecutorResource string
	Receipt          []string
	HasAttester      bool
	AttesterResource string
	Attestable       bool
	// Notes are fixed statements derived from the contract (never attested,
	// not attestable).
	Notes []string
}

// inspectComputation locates the computation and reports the contract fields
// of an Attested Computation concept (OKF 10.2, 10.3). It never reads
// anything outside the bundle and never executes anything.
func (b *Bundle) inspectComputation(c *Concept) *ComputationInfo {
	meta := c.Meta
	info := &ComputationInfo{}
	if !blank(meta["runtime"]) {
		info.Runtime = str(meta["runtime"])
	}
	for _, e := range asList(meta["parameters"]) {
		m, ok := asMap(e)
		if !ok || blank(m["name"]) || blank(m["type"]) {
			continue
		}
		p := Param{Name: str(m["name"]), Type: str(m["type"])}
		if r, ok := m["required"].(bool); ok {
			p.Required = r
		}
		info.Params = append(info.Params, p)
	}

	if !blank(meta["computation"]) {
		raw := str(meta["computation"])
		res := b.resolve(c.Path, raw, true)
		switch res.Kind {
		case RefFound, RefFallback:
			info.Location = "file " + res.Target
		default:
			info.Location = "file " + raw
			info.LocationMissing = true
		}
	} else if blocks := computationFences(c.Body); len(blocks) > 0 {
		info.Location = "first fenced block of the Computation section"
		info.Block, info.BlockInfo = blocks[0].Text, blocks[0].Info
	}
	info.Attestable = info.Location != "" && !info.LocationMissing

	if m, ok := asMap(meta["executor"]); ok {
		if !blank(m["resource"]) {
			info.ExecutorResource = str(m["resource"])
		}
		if l, ok := m["receipt"].([]any); ok {
			for _, r := range l {
				info.Receipt = append(info.Receipt, str(r))
			}
		}
	}
	if m, ok := asMap(meta["attester"]); ok && !blank(m["resource"]) {
		info.HasAttester = true
		info.AttesterResource = str(m["resource"])
	}

	if !info.Attestable {
		info.Notes = append(info.Notes, "not attestable: the computation is missing")
	}
	if info.Runtime == "" || !info.HasAttester {
		var missing []string
		if info.Runtime == "" {
			missing = append(missing, "runtime")
		}
		if !info.HasAttester {
			missing = append(missing, "attester")
		}
		info.Notes = append(info.Notes, "no "+strings.Join(missing, " or ")+": a value from this computation can never be shown as attested")
	}

	return info
}
