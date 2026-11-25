package subagent

import (
	"context"
	"testing"

	"github.com/cloudwego/eino/components/tool"
	"github.com/rs/zerolog"
)

// say is the channel voice (say-mid-turn-notice): no dispatch ever gets it,
// whatever the definition says and at any depth.
func TestChildCatalog_SayStructurallyExcluded(t *testing.T) {
	owner := []tool.BaseTool{namedStubTool{"say"}, namedStubTool{"set_timer"}, namedStubTool{"light_turn_on"}}

	for name, def := range map[string]Definition{
		"unrestricted":      {Name: "u"},
		"allow names say":   {Name: "a", Tools: []string{"say", "set_timer"}},
		"allow star":        {Name: "s", Tools: []string{"*"}},
		"deny list not say": {Name: "d", DisallowedTools: []string{"light_turn_on"}},
	} {
		for _, atCap := range []bool{false, true} {
			got := map[string]bool{}

			for _, tl := range childCatalog(context.Background(), owner, def, atCap, zerolog.Nop()) {
				info, _ := tl.Info(context.Background())
				got[info.Name] = true
			}

			if got["say"] {
				t.Errorf("%s (atDepthCap=%v): child catalog contains say: %v", name, atCap, got)
			}

			if !got["light_turn_on"] && name != "deny list not say" && name != "allow names say" {
				t.Errorf("%s (atDepthCap=%v): other tools must still be inherited: %v", name, atCap, got)
			}
		}
	}
}
