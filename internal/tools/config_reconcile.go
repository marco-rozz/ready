package tools

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/marco-rozz/ready/internal/config"
	"gopkg.in/yaml.v3"
)

func newProposalID() (string, error) {
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return "", fmt.Errorf("generate proposal identity: %w", err)
	}

	return "proposal-" + hex.EncodeToString(id[:]), nil
}

// structuralChangesDisjoint rejects overlapping YAML paths. Sequences are
// atomic because index-based merges are ambiguous when another writer edits
// the same sequence. Textual exact-match checks still apply afterwards.
func structuralChangesDisjoint(base, proposed, current []byte) bool {
	parse := func(data []byte) (map[string]string, bool) {
		var doc yaml.Node
		if yaml.Unmarshal(data, &doc) != nil || len(doc.Content) != 1 {
			return nil, false
		}
		out := make(map[string]string)
		var walk func(*yaml.Node, string) bool
		walk = func(n *yaml.Node, path string) bool {
			if n == nil {
				return false
			}
			switch n.Kind {
			case yaml.MappingNode:
				out[path] = "mapping"
				seen := make(map[string]bool)
				for i := 0; i+1 < len(n.Content); i += 2 {
					key := n.Content[i]
					if key.Kind != yaml.ScalarNode || seen[key.Value] {
						return false
					}
					seen[key.Value] = true
					segment := strings.ReplaceAll(strings.ReplaceAll(key.Value, "~", "~0"), "/", "~1")
					if !walk(n.Content[i+1], path+"/"+segment) {
						return false
					}
				}

				return true
			case yaml.ScalarNode:
				out[path] = "scalar:" + n.Tag + ":" + n.Value

				return true
			case yaml.SequenceNode:
				encoded, err := yaml.Marshal(n)
				if err != nil {
					return false
				}
				out[path] = "sequence:" + string(encoded)

				return true
			default:
				return false
			}
		}
		if !walk(doc.Content[0], "") {
			return nil, false
		}

		return out, true
	}
	b, ok := parse(base)
	if !ok {
		return false
	}
	p, ok := parse(proposed)
	if !ok {
		return false
	}
	c, ok := parse(current)
	if !ok {
		return false
	}
	changes := func(a, z map[string]string) []string {
		keys := make(map[string]bool, len(a)+len(z))
		for k := range a {
			keys[k] = true
		}
		for k := range z {
			keys[k] = true
		}
		var result []string
		for k := range keys {
			av, ao := a[k]
			zv, zo := z[k]
			if ao != zo || av != zv {
				result = append(result, k)
			}
		}

		return result
	}
	external := make(map[string]bool)
	externalParents := make(map[string]bool)
	for _, path := range changes(b, c) {
		external[path] = true
		for parent := path; parent != ""; {
			parent = parent[:strings.LastIndex(parent, "/")]
			externalParents[parent] = true
		}
	}
	for _, path := range changes(b, p) {
		if external[path] || externalParents[path] {
			return false
		}
		for parent := path; parent != ""; {
			parent = parent[:strings.LastIndex(parent, "/")]
			if external[parent] {
				return false
			}
		}
	}

	return true
}

// Reconcile replays recorded exact edits only when every old text remains
// uniquely present in the current source. It never writes the elected file.
func (m *ConfigManager) Reconcile(ctx context.Context, stageID string, revision int) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.checkStageIdentity(ctx, stageID, revision); err != nil {
		return "", err
	}
	if !m.stage.conflicted {
		return "", fmt.Errorf("%w: the draft has no source conflict", ErrConfigInvalidCandidate)
	}
	current, err := m.opts.readSource(ctx)
	if err != nil {
		return "", wrapUncommittedIO("reading the elected configuration failed", err)
	}
	if len(current.Data) > maxConfigDraftBytes {
		return "", fmt.Errorf("%w: configuration exceeds draft size limit", ErrConfigInvalidCandidate)
	}
	base, literals, err := maskLiterals(current.Data)
	if err != nil {
		return "", wrapCensusRefusal(err)
	}
	if literals != 0 {
		return "", fmt.Errorf("%w: adopt literal secrets before reconciling; %s", ErrLiteralSecret, adoptGuidance)
	}
	original, originalLiterals, err := maskLiterals(m.stage.source.Data)
	if err != nil {
		return "", wrapCensusRefusal(err)
	}
	if originalLiterals != 0 || !structuralChangesDisjoint(original, m.stage.text, base) {
		return "", fmt.Errorf("%w: overlapping or ambiguous configuration changes; review both versions with the user", config.ErrConfigSourceConflict)
	}
	working := cloneBytes(base)
	for _, edit := range m.stage.edits {
		pos, count := locateAll(working, edit.OldText)
		if count != 1 {
			return "", fmt.Errorf("%w: overlapping or ambiguous external changes; review the saved configuration and draft with the user", config.ErrConfigSourceConflict)
		}
		next := make([]byte, 0, len(working)-len(edit.OldText)+len(edit.NewText))
		next = append(next, working[:pos[0]]...)
		next = append(next, edit.NewText...)
		working = append(next, working[pos[0]+len(edit.OldText):]...)
		if len(working) > maxConfigDraftBytes {
			return "", fmt.Errorf("%w: reconciled draft exceeds the size limit", ErrConfigInvalidCandidate)
		}
	}
	if paths, litErr := newLiterals(working); litErr != nil {
		return "", wrapBatchRefusal(litErr)
	} else if len(paths) > 0 {
		return "", wrapBatchRefusal(literalRefusal(paths))
	}
	if referenceChanged(base, working, "agent", "id") {
		return "", fmt.Errorf("%w: agent.id is immutable", ErrConfigInvalidEdit)
	}
	if !m.isZMQSession(ctx) && referenceChanged(base, working, "channels", "zmqcli", "auth_token") {
		return "", fmt.Errorf("%w: channels.zmqcli.auth_token changes require the ZMQ TUI", ErrConfigInvalidEdit)
	}
	if _, err = config.ValidateCandidate(ctx, working, m.opts.ElectedPath, m.opts.ProfileOverrides, m.opts.ServeLocalChecks); err != nil {
		return "", fmt.Errorf("%w: %w", ErrConfigInvalidCandidate, err)
	}
	proposalID, err := newProposalID()
	if err != nil {
		return "", err
	}
	m.stage.source = current
	m.stage.text = working
	m.stage.revision++
	m.stage.conflicted = false
	m.stage.proposalID = proposalID
	m.stage.confirmed = false
	m.stage.validation = candidateValidationRef{ran: true}

	return marshalJSON(map[string]any{
		configActionKey: "reconcile", "stage_id": stageID, "revision": m.stage.revision,
		"proposal_id": m.stage.proposalID, "diff": unifiedDiff(base, working),
		"instruction": "Show the complete diff to the user. To approve this exact proposal, the user must send /confirm-config " + m.stage.proposalID + " in a new message before save.",
	}), nil
}

// ConfirmConfigInput is called only by the dispatcher with the actual user
// message and trusted Session ID. Model tool arguments cannot invoke it.
func (m *ConfigManager) ConfirmConfigInput(ctx context.Context, sessionID, input string) {
	fields := strings.Fields(input)
	if len(fields) != 2 || fields[0] != "/confirm-config" {
		return
	}
	m.mu.Lock()
	child := m.sessions[sessionID]
	if child == nil {
		m.mu.Unlock()

		return
	}
	child.mu.Lock()
	m.mu.Unlock()
	defer child.mu.Unlock()
	st := child.stage
	if st == nil || st.conflicted || st.proposalID == "" || fields[1] != st.proposalID {
		return
	}
	current, err := m.opts.readSource(ctx)
	if err != nil || !st.source.Matches(current) {
		st.confirmed = false
		if err == nil {
			st.conflicted = true
		}

		return
	}
	st.confirmed = true
}
