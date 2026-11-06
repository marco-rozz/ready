package secretstore

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"time"

	"filippo.io/age"
)

// PayloadFormat is the version of the plaintext payload schema.
const PayloadFormat = 1

// ErrPayloadFormat reports a payload this binary cannot interpret.
var ErrPayloadFormat = errors.New("secret store: unsupported payload format")

// Secret is one operator-entered value.
type Secret struct {
	Value     string    `json:"value"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Credential is one login-managed OAuth record. Generation increases with
// every committed change and drives per-entry compare-before-save.
type Credential struct {
	Generation uint64          `json:"generation"`
	Record     json.RawMessage `json:"record"`
}

// Payload is the decrypted content of secrets.age.
type Payload struct {
	Format  int    `json:"format"`
	AgentID string `json:"agent_id"`
	// Secrets maps a Secret name to its value.
	Secrets map[string]Secret `json:"secrets"`
	// Credentials maps a target ("mcp:<server>", "llm:<provider>") to its record.
	Credentials map[string]Credential `json:"credentials"`
	// Tombstones remembers the generation of a deleted Credential so a stale
	// writer cannot resurrect it.
	Tombstones map[string]uint64 `json:"tombstones"`
	// Cleanup lists absolute paths of plaintext files still to delete after a
	// migration import; retried at every boot.
	Cleanup []string `json:"cleanup"`
}

// NewPayload returns an empty payload bound to agentID.
func NewPayload(agentID string) *Payload {
	p := &Payload{Format: PayloadFormat, AgentID: agentID}
	p.normalize()

	return p
}

func (p *Payload) normalize() {
	if p.Secrets == nil {
		p.Secrets = map[string]Secret{}
	}
	if p.Credentials == nil {
		p.Credentials = map[string]Credential{}
	}
	if p.Tombstones == nil {
		p.Tombstones = map[string]uint64{}
	}
	if p.Cleanup == nil {
		p.Cleanup = []string{}
	}
}

// Clone returns a deep copy, so a cached view is never mutated by a writer.
func (p *Payload) Clone() *Payload {
	c := &Payload{Format: p.Format, AgentID: p.AgentID}
	c.normalize()
	maps.Copy(c.Secrets, p.Secrets)
	for k, v := range p.Credentials {
		v.Record = append(json.RawMessage(nil), v.Record...)
		c.Credentials[k] = v
	}
	maps.Copy(c.Tombstones, p.Tombstones)
	c.Cleanup = append(c.Cleanup, p.Cleanup...)

	return c
}

// seal marshals and encrypts the payload to the Store key.
func (p *Payload) seal(id *age.X25519Identity) ([]byte, error) {
	raw, err := json.Marshal(p)
	if err != nil {
		return nil, fmt.Errorf("failed to encode secret store payload: %w", err)
	}

	return encrypt(id, raw)
}

// openPayload decrypts and decodes a blob, rejecting unknown formats.
func openPayload(id *age.X25519Identity, blob []byte) (*Payload, error) {
	raw, err := decrypt(id, blob)
	if err != nil {
		return nil, err
	}
	var p Payload
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, fmt.Errorf("failed to decode secret store payload: %w", err)
	}
	if p.Format != PayloadFormat {
		return nil, fmt.Errorf("%w: format %d (this binary reads %d)", ErrPayloadFormat, p.Format, PayloadFormat)
	}
	p.normalize()

	return &p, nil
}
