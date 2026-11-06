package secretstore

import (
	"bytes"
	"errors"
	"testing"
	"time"
)

func TestPayloadSealOpenRoundTrip(t *testing.T) {
	id, _ := GenerateIdentity()
	p := NewPayload("agent-1")
	p.Secrets["k"] = Secret{Value: "  spaced  ", UpdatedAt: time.Unix(1, 0).UTC()}
	p.Credentials["mcp:x"] = Credential{Generation: 3, Record: []byte(`{"a":1}`)}
	p.Tombstones["llm:y"] = 7
	p.Cleanup = append(p.Cleanup, "/tmp/old")

	blob, err := p.seal(id)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(blob, []byte("spaced")) {
		t.Fatal("ciphertext leaks a value")
	}
	got, err := openPayload(id, blob)
	if err != nil {
		t.Fatal(err)
	}
	if got.AgentID != "agent-1" || got.Secrets["k"].Value != "  spaced  " {
		t.Fatalf("secret not preserved exactly: %+v", got.Secrets["k"])
	}
	if got.Credentials["mcp:x"].Generation != 3 || got.Tombstones["llm:y"] != 7 || len(got.Cleanup) != 1 {
		t.Fatalf("payload = %+v", got)
	}
}

func TestOpenPayloadRejectsUnknownFormat(t *testing.T) {
	id, _ := GenerateIdentity()
	p := NewPayload("a")
	p.Format = 99
	blob, err := p.seal(id)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := openPayload(id, blob); !errors.Is(err, ErrPayloadFormat) {
		t.Fatalf("err = %v, want ErrPayloadFormat", err)
	}
}

func TestPayloadCloneIsDeep(t *testing.T) {
	p := NewPayload("a")
	p.Secrets["k"] = Secret{Value: "v"}
	p.Credentials["c"] = Credential{Generation: 1, Record: []byte(`{}`)}
	c := p.Clone()
	c.Secrets["k"] = Secret{Value: "changed"}
	c.Credentials["c"].Record[0] = 'X'
	if p.Secrets["k"].Value != "v" || p.Credentials["c"].Record[0] != '{' {
		t.Fatal("clone shares state with the original")
	}
}
