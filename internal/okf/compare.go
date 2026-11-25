package okf

import "sort"

// Deciding steps of the conflict ordering (trust.md "Conflicts").
const (
	StepStatus    = "status"
	StepTier      = "trust tier"
	StepFreshness = "freshness"
	StepRecency   = "recency (generated.at)"
	StepUndecided = "undecided"
)

// CompareResult is the ordered candidate list with the deciding step of each
// adjacent pair (Steps[i] decides between Ordered[i] and Ordered[i+1]).
type CompareResult struct {
	Ordered []*Concept
	Steps   []string
}

// compareConcepts returns <0 when a ranks before b, >0 when after, 0 when no
// step differs, plus the deciding step name.
func compareConcepts(a, b *Concept) (int, string) {
	if a.Current != b.Current {
		if a.Current {
			return -1, StepStatus
		}

		return 1, StepStatus
	}
	if a.Tier.rank() != b.Tier.rank() {
		if a.Tier.rank() > b.Tier.rank() {
			return -1, StepTier
		}

		return 1, StepTier
	}
	if a.Stale != b.Stale {
		if !a.Stale {
			return -1, StepFreshness
		}

		return 1, StepFreshness
	}
	switch {
	case a.ChangeTime != nil && b.ChangeTime != nil:
		if a.ChangeTime.After(*b.ChangeTime) {
			return -1, StepRecency
		}
		if b.ChangeTime.After(*a.ChangeTime) {
			return 1, StepRecency
		}
	case a.ChangeTime != nil:
		return -1, StepRecency
	case b.ChangeTime != nil:
		return 1, StepRecency
	}

	return 0, StepUndecided
}

// Compare orders the concepts by OKF precedence; the first difference
// decides. Whether the concepts disagree is the caller's judgment.
func Compare(concepts []*Concept) *CompareResult {
	ordered := append([]*Concept(nil), concepts...)
	sort.SliceStable(ordered, func(i, j int) bool {
		c, _ := compareConcepts(ordered[i], ordered[j])

		return c < 0
	})
	res := &CompareResult{Ordered: ordered}
	for i := 0; i+1 < len(ordered); i++ {
		_, step := compareConcepts(ordered[i], ordered[i+1])
		res.Steps = append(res.Steps, step)
	}

	return res
}
