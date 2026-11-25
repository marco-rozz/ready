package subagent

import (
	"context"
	"testing"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
	"github.com/rs/zerolog"
)

type namedStubTool struct{ name string }

func (s namedStubTool) Info(context.Context) (*schema.ToolInfo, error) {
	return &schema.ToolInfo{Name: s.name}, nil
}

// okf_write is gated like any builtin: the shipped general-purpose
// definition (no restriction) inherits it, the read-only explore allowlist
// never lets it in.
func TestShippedDefinitionsOKFWriteInheritance(t *testing.T) {
	defs, err := LoadDefinitions("../../resources/subagents")
	if err != nil {
		t.Fatal(err)
	}
	idx := NewDefinitions(defs)
	owner := []tool.BaseTool{namedStubTool{"okf_query"}, namedStubTool{"okf_write"}, namedStubTool{"ha_state_query"}}

	names := func(def Definition) map[string]bool {
		out := map[string]bool{}
		for _, tl := range childCatalog(context.Background(), owner, def, true, zerolog.Nop()) {
			info, _ := tl.Info(context.Background())
			out[info.Name] = true
		}

		return out
	}
	gp, _ := idx.Get("general-purpose")
	if got := names(gp); !got["okf_write"] || !got["okf_query"] {
		t.Errorf("general-purpose catalog = %v, want okf_write and okf_query inherited", got)
	}
	ex, _ := idx.Get("explore")
	if got := names(ex); got["okf_write"] {
		t.Errorf("explore catalog = %v, must not contain okf_write", got)
	}
}
