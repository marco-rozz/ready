package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/marco-rozz/ready/internal/config"
)

func prepareConflict(t *testing.T) (*fakeFS, *ConfigManager, *ConfigManagerTool, stagedView) {
	t.Helper()
	fs, mgr := newTestManager(t, validMiniConfig(), "a")
	tool := NewConfigManagerTool(mgr)
	ctx := context.Background()
	out, err := tool.InvokableRun(ctx, `{"action":"begin"}`)
	if err != nil {
		t.Fatal(err)
	}
	var view stagedView
	if err = json.Unmarshal([]byte(out), &view); err != nil {
		t.Fatal(err)
	}
	edit := fmt.Sprintf(`{"action":"update","stage_id":%q,"expected_revision":0,"edits":[{"old_text":%q,"new_text":%q}]}`, view.StageID, testZmqBind, testZmqBindChanged)
	if _, err = tool.InvokableRun(ctx, edit); err != nil {
		t.Fatal(err)
	}
	fs.doc = strings.Replace(fs.doc, "gemma4:31b", "gemma4:27b", 1)
	save := fmt.Sprintf(`{"action":"save","stage_id":%q,"expected_revision":1}`, view.StageID)
	if _, err = tool.InvokableRun(ctx, save); !errors.Is(err, config.ErrConfigSourceConflict) {
		t.Fatalf("expected conflict: %v", err)
	}

	return fs, mgr, tool, view
}

func TestReconcileRequiresTrustedConfirmation(t *testing.T) {
	fs, mgr, tool, view := prepareConflict(t)
	ctx := context.Background()
	args := fmt.Sprintf(`{"action":"reconcile","stage_id":%q,"expected_revision":1}`, view.StageID)
	out, err := tool.InvokableRun(ctx, args)
	if err != nil {
		t.Fatal(err)
	}
	var proposal struct {
		ProposalID string `json:"proposal_id"`
		Revision   int    `json:"revision"`
		Diff       string `json:"diff"`
	}
	if err = json.Unmarshal([]byte(out), &proposal); err != nil {
		t.Fatal(err)
	}
	if proposal.ProposalID == "" || !strings.Contains(proposal.Diff, testZmqBindChanged) {
		t.Fatalf("proposal: %s", out)
	}
	save := fmt.Sprintf(`{"action":"save","stage_id":%q,"expected_revision":%d}`, view.StageID, proposal.Revision)
	if _, err = tool.InvokableRun(ctx, save); err == nil {
		t.Fatal("unconfirmed save succeeded")
	}
	forged := fmt.Sprintf(`{"action":"save","stage_id":%q,"expected_revision":%d,"confirmed":true}`, view.StageID, proposal.Revision)
	if _, err = tool.InvokableRun(ctx, forged); err == nil {
		t.Fatal("model-supplied confirmation accepted")
	}
	mgr.ConfirmConfigInput(ctx, "other", "/confirm-config "+proposal.ProposalID)
	mgr.ConfirmConfigInput(ctx, "a", "/confirm-config "+proposal.ProposalID+" yes")
	if _, err = tool.InvokableRun(ctx, save); err == nil {
		t.Fatal("foreign or ambiguous confirmation accepted")
	}
	mgr.ConfirmConfigInput(ctx, "a", "/confirm-config "+proposal.ProposalID)
	if _, err = tool.InvokableRun(ctx, save); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(fs.doc, "gemma4:27b") || !strings.Contains(fs.doc, testZmqBindChanged) {
		t.Fatal("disjoint changes not preserved")
	}
}

func TestReconcileRejectsOverlapAndChangedSource(t *testing.T) {
	fs, mgr, tool, view := prepareConflict(t)
	ctx := context.Background()
	fs.doc = strings.Replace(fs.doc, testZmqBind, "bind: tcp://127.0.0.1:7602", 1)
	args := fmt.Sprintf(`{"action":"reconcile","stage_id":%q,"expected_revision":1}`, view.StageID)
	if _, err := tool.InvokableRun(ctx, args); !errors.Is(err, config.ErrConfigSourceConflict) {
		t.Fatalf("overlap: %v", err)
	}
	fs.doc = strings.Replace(fs.doc, "bind: tcp://127.0.0.1:7602", testZmqBind, 1)
	out, err := tool.InvokableRun(ctx, args)
	if err != nil {
		t.Fatal(err)
	}
	var proposal struct {
		ProposalID string `json:"proposal_id"`
		Revision   int    `json:"revision"`
	}
	if err = json.Unmarshal([]byte(out), &proposal); err != nil {
		t.Fatal(err)
	}
	mgr.ConfirmConfigInput(ctx, "a", "/confirm-config "+proposal.ProposalID)
	fs.doc += "\n# external change\n"
	save := fmt.Sprintf(`{"action":"save","stage_id":%q,"expected_revision":%d}`, view.StageID, proposal.Revision)
	if _, err = tool.InvokableRun(ctx, save); !errors.Is(err, config.ErrConfigSourceConflict) {
		t.Fatalf("stale confirmation: %v", err)
	}
}

func TestReconcileProposalModificationInvalidatesConfirmation(t *testing.T) {
	_, mgr, tool, view := prepareConflict(t)
	ctx := context.Background()
	args := fmt.Sprintf(`{"action":"reconcile","stage_id":%q,"expected_revision":1}`, view.StageID)
	out, err := tool.InvokableRun(ctx, args)
	if err != nil {
		t.Fatal(err)
	}
	var proposal struct {
		ProposalID string `json:"proposal_id"`
		Revision   int    `json:"revision"`
	}
	if err = json.Unmarshal([]byte(out), &proposal); err != nil {
		t.Fatal(err)
	}
	mgr.ConfirmConfigInput(ctx, "a", "/confirm-config "+proposal.ProposalID)
	update := fmt.Sprintf(`{"action":"update","stage_id":%q,"expected_revision":%d,"edits":[{"old_text":%q,"new_text":%q}]}`, view.StageID, proposal.Revision, testZmqBindChanged, "bind: tcp://127.0.0.1:7603")
	out, err = tool.InvokableRun(ctx, update)
	if err != nil {
		t.Fatal(err)
	}
	var revised updateResult
	if err = json.Unmarshal([]byte(out), &revised); err != nil {
		t.Fatal(err)
	}
	if revised.ProposalID == "" || revised.ProposalID == proposal.ProposalID {
		t.Fatal("revised draft retained proposal identity")
	}
	save := fmt.Sprintf(`{"action":"save","stage_id":%q,"expected_revision":%d}`, view.StageID, revised.Revision)
	if _, err = tool.InvokableRun(ctx, save); err == nil {
		t.Fatal("old confirmation approved modified draft")
	}
	mgr.ConfirmConfigInput(ctx, "a", "/confirm-config "+proposal.ProposalID)
	if _, err = tool.InvokableRun(ctx, save); err == nil {
		t.Fatal("old proposal accepted")
	}
	mgr.ConfirmConfigInput(ctx, "a", "/confirm-config "+revised.ProposalID)
	if _, err = tool.InvokableRun(ctx, save); err != nil {
		t.Fatal(err)
	}
}

func TestStructuralChangesDisjointRejectsOverlaps(t *testing.T) {
	base := []byte("a:\n  x: 1\n  y: 2\nitems: [a, b]\n")
	const proposedX = "a:\n  x: 3\n  y: 2\nitems: [a, b]\n"
	for _, tc := range []struct {
		name, proposed, current string
		want                    bool
	}{
		{"different keys", proposedX, "a:\n  x: 1\n  y: 4\nitems: [a, b]\n", true},
		{"same key", proposedX, "a:\n  x: 4\n  y: 2\nitems: [a, b]\n", false},
		{"removed parent", proposedX, "items: [a, b]\n", false},
		{"same sequence", "a:\n  x: 1\n  y: 2\nitems: [a, c]\n", "a:\n  x: 1\n  y: 2\nitems: [a, d]\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := structuralChangesDisjoint(base, []byte(tc.proposed), []byte(tc.current)); got != tc.want {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestReconcileRefusesLiteralSecretsWithoutChangingDraft(t *testing.T) {
	fs, _, tool, view := prepareConflict(t)
	ctx := context.Background()
	before, err := tool.InvokableRun(ctx, `{"action":"inspect"}`)
	if err != nil {
		t.Fatal(err)
	}
	fs.doc = strings.Replace(fs.doc, "auth_token: !secret zmqcli_auth_token", "auth_token: "+aLiteral, 1)
	args := fmt.Sprintf(`{"action":"reconcile","stage_id":%q,"expected_revision":1}`, view.StageID)
	_, err = tool.InvokableRun(ctx, args)
	if !errors.Is(err, ErrLiteralSecret) || strings.Contains(err.Error(), aLiteral) {
		t.Fatalf("literal refusal: %v", err)
	}
	after, err := tool.InvokableRun(ctx, `{"action":"inspect"}`)
	if err != nil || after != before {
		t.Fatalf("draft changed on refusal: %v", err)
	}
}

func TestReconcileValidationFailureKeepsDraft(t *testing.T) {
	_, mgr, tool, view := prepareConflict(t)
	ctx := context.Background()
	before, err := tool.InvokableRun(ctx, `{"action":"inspect"}`)
	if err != nil {
		t.Fatal(err)
	}
	refused := errors.New("new runtime validation refused")
	mgr.opts.ServeLocalChecks = func(*config.Config) error { return refused }
	args := fmt.Sprintf(`{"action":"reconcile","stage_id":%q,"expected_revision":1}`, view.StageID)
	if _, err = tool.InvokableRun(ctx, args); !errors.Is(err, refused) {
		t.Fatalf("validation: %v", err)
	}
	after, err := tool.InvokableRun(ctx, `{"action":"inspect"}`)
	if err != nil || after != before {
		t.Fatalf("validation refusal changed draft: %v", err)
	}
}

func TestReconcileDoesNotApplyPartialProposal(t *testing.T) {
	fs, mgr := newTestManager(t, validMiniConfig(), "a")
	tool := NewConfigManagerTool(mgr)
	ctx := context.Background()
	opened, err := tool.InvokableRun(ctx, `{"action":"begin"}`)
	if err != nil {
		t.Fatal(err)
	}
	var view stagedView
	if err = json.Unmarshal([]byte(opened), &view); err != nil {
		t.Fatal(err)
	}
	args := fmt.Sprintf(`{"action":"update","stage_id":%q,"expected_revision":0,"edits":[{"old_text":%q,"new_text":%q},{"old_text":"gemma4:31b","new_text":"gemma4:27b"}]}`, view.StageID, testZmqBind, testZmqBindChanged)
	if _, err = tool.InvokableRun(ctx, args); err != nil {
		t.Fatal(err)
	}
	fs.doc = strings.Replace(fs.doc, testZmqBind, "bind: tcp://127.0.0.1:7602", 1)
	save := fmt.Sprintf(`{"action":"save","stage_id":%q,"expected_revision":1}`, view.StageID)
	if _, err = tool.InvokableRun(ctx, save); !errors.Is(err, config.ErrConfigSourceConflict) {
		t.Fatalf("conflict: %v", err)
	}
	before, err := tool.InvokableRun(ctx, `{"action":"inspect"}`)
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal([]byte(before), &view); err != nil {
		t.Fatal(err)
	}
	args = fmt.Sprintf(`{"action":"reconcile","stage_id":%q,"expected_revision":%d}`, view.StageID, view.Revision)
	if _, err = tool.InvokableRun(ctx, args); !errors.Is(err, config.ErrConfigSourceConflict) {
		t.Fatalf("overlap: %v", err)
	}
	after, err := tool.InvokableRun(ctx, `{"action":"inspect"}`)
	if err != nil || after != before {
		t.Fatalf("partial proposal changed draft: %v", err)
	}
}
