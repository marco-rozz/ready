package initwizard

import (
	"strings"
	"testing"
)

func TestSetReferenceReplacesAndInsertsRaw(t *testing.T) {
	doc := []byte("llm:\n  providers:\n    p:\n      provider: openrouter\n      api: chat_completions\n      api_key: !secret old_name\n    q:\n      provider: openrouter\n      api: chat_completions\n")
	got, err := setReference(doc, []string{"llm", "providers", "p", "api_key"}, "!secret new_name", "api")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), "api_key: !secret new_name\n") || strings.Contains(string(got), "old_name") {
		t.Fatalf("replace:\n%s", got)
	}
	got, err = setReference(doc, []string{"llm", "providers", "q", "api_key"}, "!secret q_api_key", "api")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), "      api_key: !secret q_api_key\n") {
		t.Fatalf("insert:\n%s", got)
	}
}
