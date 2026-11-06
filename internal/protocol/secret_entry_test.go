package protocol

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestSecretEntryMalformedCanaryNeverAppearsInDiagnostics(t *testing.T) {
	const canary = "unique-secret-canary-ü-value"
	base := `{"connection_id":"c","type":"secret_entry","timestamp":"2026-10-26T12:00:00Z","request_id":"r","secret_entry":{"action":"submit","operation_id":"o","name":"n","value":"` + canary + `"}}`
	for _, data := range []string{
		strings.Replace(base, `"submit"`, `"`+canary+`"`, 1),
		strings.Replace(base, `"value":`, `"`+canary+`":`, 1),
		strings.Replace(base, `"value":"`+canary+`"`, `"value":{"`+canary+`":true}`, 1),
		strings.Replace(base, `"timestamp":"2026-10-26T12:00:00Z"`, `"timestamp":"`+canary+`"`, 1),
		strings.Replace(base, `"type":"secret_entry"`, `"type":"`+canary+`"`, 1),
		base + `{}`, base + ` trailing`,
	} {
		_, err := UnmarshalRequest([]byte(data))
		if !errors.Is(err, ErrMalformedRequest) || strings.Contains(err.Error(), canary) {
			t.Fatalf("unsafe decode result: %v", err)
		}
	}
}

func TestSecretEntryResponseStrictShapeAndAuthoritativeEmpty(t *testing.T) {
	for _, data := range []string{
		`{"status":"ok","secret_entry":{"action":"submit","outcome":"committed","value":"canary"}}`,
		`{"status":"ok","secret_entry":{"action":"submit","outcome":"unknown"}}`,
		`{"status":"ok","pending_secrets":[{"name":"p","state":"pending","value":"canary"}]}`,
		`{"status":"ok","pending_secrets":[{"name":"p","path_id":"x"}]}`,
		`{"status":"ok"}{"status":"ok"}`,
	} {
		if _, err := UnmarshalResponse([]byte(data)); !errors.Is(err, ErrMalformedResponse) {
			t.Fatalf("invalid response accepted: %s", data)
		}
	}
	for _, available := range []bool{false, true} {
		resp := Response{Status: ResponseStatusOK}
		if available {
			resp.PendingSecrets = []PendingSecret{}
		}
		data, err := Marshal(resp)
		if err != nil {
			t.Fatal(err)
		}
		var raw map[string]json.RawMessage
		if err = json.Unmarshal(data, &raw); err != nil {
			t.Fatal(err)
		}
		_, present := raw["pending_secrets"]
		if present != available {
			t.Fatalf("pending omission lost authority: %s", data)
		}
		if available && string(raw["pending_secrets"]) != "[]" {
			t.Fatalf("empty pending set=%s", data)
		}
	}
}
