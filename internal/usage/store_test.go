package usage

import (
	"testing"

	"github.com/cloudwego/eino/schema"
)

// session-lifecycle D8b: an auxiliary call (post-step formatter, compaction
// summariser) never moves the occupancy anchor.
func TestLastMainPromptTokens_IgnoresAuxCalls(t *testing.T) {
	acc := NewStore().Accumulator("s")

	acc.AddUsage(schema.TokenUsage{PromptTokens: 22400, CompletionTokens: 100}, false)
	acc.AddUsage(schema.TokenUsage{PromptTokens: 900, CompletionTokens: 50}, true)

	got := acc.Snapshot()
	if got.LastMainPromptTokens != 22400 {
		t.Errorf("LastMainPromptTokens = %d, want 22400", got.LastMainPromptTokens)
	}

	if got.ReadTokens != 23300 || got.AuxCalls != 1 || got.LLMCalls != 1 {
		t.Errorf("totals = %+v", got)
	}
}

func TestSeed_RestoresOnlyFreshSessions(t *testing.T) {
	st := NewStore()
	st.Seed("s", Totals{ReadTokens: 100, TurnCount: 3, LLMCalls: 5, LastMainPromptTokens: 999})

	got := st.Snapshot("s")
	if got == nil || got.ReadTokens != 100 || got.TurnCount != 3 || got.LastMainPromptTokens != 0 {
		t.Fatalf("seeded totals = %+v (the measurement is never restored)", got)
	}

	st.Accumulator("s").AddTurn()
	st.Seed("s", Totals{ReadTokens: 1})

	if st.Snapshot("s").ReadTokens != 100 {
		t.Error("Seed must not overwrite live totals")
	}

	st.Drop("s")

	if st.Snapshot("s") != nil {
		t.Error("Drop must release the totals")
	}
}
