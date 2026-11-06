//nolint:goconst // literal fixtures are clearer inline than behind constants
package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/marco-rozz/ready/internal/config"
)

// testAgentID is the identity of every fixture configuration.
const testAgentID = "0190aaaa-bbbb-7ccc-8ddd-eeeeffff0001"

// aLiteral is a synthetic literal credential used to prove literals are
// masked and refused (never a real secret).
const aLiteral = "a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4"

// testForeignSession is the stand-in identity for another Session.
const testForeignSession = "session-b"

// testZmqBind is the repeated bind line in the mini config.
const testZmqBind = "bind: tcp://127.0.0.1:7600"

// testZmqBindChanged is the bind line after the scripted edit.
const testZmqBindChanged = "bind: tcp://127.0.0.1:7601"

func validMiniConfig() string {
	return fmt.Sprintf("config_version: %d\n", config.TargetConfigVersion) +
		"agent:\n" +
		"  id: " + testAgentID + "\n" +
		"llm:\n" +
		"  default: ollama-gemma::gemma4:31b\n" +
		"  providers:\n" +
		"    ollama-gemma:\n" +
		"      provider: ollama\n" +
		"      api: ollama\n" +
		"channels:\n" +
		"  zmqcli:\n" +
		"    enabled: true\n" +
		"    bind: tcp://127.0.0.1:7600\n" +
		"    auth_token: !secret zmqcli_auth_token\n"
}

type fakeFS struct {
	mgr       *ConfigManager
	doc       string
	path      string
	sessionID string
	commits   int
	lastCand  []byte
	conflict  bool
	stageN    int
}

func newTestManager(t *testing.T, doc, initialSession string) (*fakeFS, *ConfigManager) {
	t.Helper()
	fs := &fakeFS{doc: doc, path: "/home/test/.ready/config.yaml", sessionID: initialSession}

	mgr, err := NewConfigManager(ConfigManagerOptions{
		ElectedPath:      "/home/test/.ready/config.yaml",
		ProfileOverrides: config.ProfileOverrideInputs{},
		SessionIdentity: func(_ context.Context) (string, bool) {
			return fs.sessionID, fs.sessionID != ""
		},
		SessionTransport: func(context.Context) string { return "zmq" },
		SessionLoopback:  func(context.Context) bool { return true },
		readSource: func(_ context.Context) (config.SourceState, error) {
			return config.SourceState{Path: fs.path, Data: []byte(fs.doc), Mode: 0o600}, nil
		},
		commit: func(_ context.Context, state config.SourceState, candidate []byte) (config.CommitReport, error) {
			// Emulate the real commit's source identity/bytes recheck.
			if fs.conflict || string(state.Data) != fs.doc {
				return config.CommitReport{}, config.ErrConfigSourceConflict
			}
			fs.commits++
			fs.lastCand = append([]byte(nil), candidate...)
			fs.doc = string(candidate)
			if _, statErr := os.Stat(fs.path); statErr == nil {
				if writeErr := os.WriteFile(fs.path, candidate, 0o600); writeErr != nil {
					return config.CommitReport{}, writeErr
				}
			}

			return config.CommitReport{Committed: true, BackupPath: "/backup/path.bak.2030"}, nil
		},
		nextStageID: func() string {
			fs.stageN++

			return fmt.Sprintf("stage-test-%d", fs.stageN)
		},
	})
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}
	fs.mgr = mgr

	return fs, mgr
}

func readAs(t *testing.T, mgr *ConfigManager) stagedView {
	t.Helper()

	out, err := mgr.Read(context.Background())
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var view stagedView
	if err := jsonUnmarshal(out, &view); err != nil {
		t.Fatalf("read result: %v", err)
	}

	return view
}

func updateAs(t *testing.T, mgr *ConfigManager, _ string, view stagedView, edits []editPair) (string, error) {
	t.Helper()

	return mgr.Update(context.Background(), view.StageID, view.Revision, edits)
}

func saveAs(t *testing.T, mgr *ConfigManager, _ string, view stagedView) (string, error) {
	t.Helper()

	return mgr.Save(context.Background(), view.StageID, view.Revision)
}

func jsonUnmarshal(text string, v any) error { return json.Unmarshal([]byte(text), v) }

func TestConfigManagerReadOpensStaging(t *testing.T) {
	t.Parallel()

	doc := validMiniConfig()
	fs, mgr := newTestManager(t, doc, "session-a")

	out, err := mgr.Read(context.Background())
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var view stagedView
	if err = json.Unmarshal([]byte(out), &view); err != nil {
		t.Fatalf("read result: %v", err)
	}
	if view.StageID == "" || view.Revision != 0 {
		t.Fatalf("staging identity wrong: %+v", view)
	}
	if !strings.Contains(view.Text, "auth_token: !secret zmqcli_auth_token") {
		t.Fatalf("staged view must show the reference:\n%s", view.Text)
	}
	_ = fs
}

func TestConfigManagerForeignSessionBusy(t *testing.T) {
	t.Parallel()

	fs, mgr := newTestManager(t, validMiniConfig(), "session-a")
	if _, err := mgr.Read(context.Background()); err != nil {
		t.Fatal(err)
	}

	// Foreign session: every action refuses.
	fs.sessionID = testForeignSession
	for _, act := range []string{"read", "update", "save", "discard"} {
		var err error
		switch act {
		case "read":
			_, err = mgr.Read(context.Background())
		case "update":
			_, err = mgr.Update(context.Background(), "stage-a", 0, []editPair{{OldText: "x", NewText: "y"}})
		case "save":
			_, err = mgr.Save(context.Background(), "stage-a", 0)
		case "discard":
			_, err = mgr.Discard(context.Background(), "stage-a", 0)
		}
		if err == nil || !strings.Contains(err.Error(), "busy") {
			t.Fatalf("foreign %s must refuse busy, got: %v", act, err)
		}
	}
}

func TestConfigManagerMissingSessionFailsClosed(t *testing.T) {
	t.Parallel()

	fs, mgr := newTestManager(t, validMiniConfig(), "session-a")
	fs.sessionID = ""
	_, err := mgr.Read(context.Background())
	if err == nil || !strings.Contains(err.Error(), "session identity") {
		t.Fatalf("missing identity must fail closed, got: %v", err)
	}
}

func TestConfigManagerUpdateWithoutStaging(t *testing.T) {
	t.Parallel()

	_, mgr := newTestManager(t, validMiniConfig(), "session-a")
	_, err := mgr.Update(context.Background(), "stage-x", 0, []editPair{{OldText: "a", NewText: "b"}})
	if err == nil || !strings.Contains(err.Error(), "read the configuration first") {
		t.Fatalf("expected no-staging refusal, got: %v", err)
	}
}

func TestConfigManagerBatchAtomicRollbackAndRevision(t *testing.T) {
	t.Parallel()

	fs, mgr := newTestManager(t, validMiniConfig(), "a")
	view := readAs(t, mgr)

	// A second edit targets stale text that the first edit already replaced —
	// the whole batch must refuse with zero committed edits.
	_, err := updateAs(t, mgr, "a", view, []editPair{
		{OldText: testZmqBind, NewText: testZmqBindChanged},
		{OldText: testZmqBind, NewText: "bind: tcp://127.0.0.1:7602"},
	})
	if err == nil || !strings.Contains(err.Error(), "does not appear in the staged text") {
		t.Fatalf("dependent stale second edit must refuse atomically, got: %v", err)
	}
	// State untouched: read again, revision still 0, old text present.
	view2 := readAs(t, mgr)
	if view2.Revision != 0 || !strings.Contains(view2.Text, "tcp://127.0.0.1:7600") {
		t.Fatalf("refused batch must leave state unchanged: %+v", view2)
	}

	// A valid content-changing update increments the revision.
	out, err := updateAs(t, mgr, "a", view2, []editPair{
		{OldText: testZmqBind, NewText: testZmqBindChanged},
	})
	if err != nil {
		t.Fatalf("valid update: %v", err)
	}
	var result updateResult
	if err = json.Unmarshal([]byte(out), &result); err != nil {
		t.Fatal(err)
	}
	if result.Revision != 1 || result.Applied != 1 || result.Diff == "" {
		t.Fatalf("update result wrong: %+v", result)
	}
	if !strings.Contains(result.Diff, "7601") {
		t.Fatalf("diff must show the change:\n%s", result.Diff)
	}

	// No-op update: revision unchanged.
	view3 := readAs(t, mgr)
	out, err = updateAs(t, mgr, "a", view3, []editPair{
		{OldText: testZmqBindChanged, NewText: testZmqBindChanged},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal([]byte(out), &result); err != nil {
		t.Fatal(err)
	}
	if !result.NoOp || result.Revision != view3.Revision {
		t.Fatalf("no-op update must keep revision: %+v", result)
	}
	_ = fs
}

func TestConfigManagerStaleRevisionRefused(t *testing.T) {
	t.Parallel()

	fs, mgr := newTestManager(t, validMiniConfig(), "a")
	view := readAs(t, mgr)
	if _, err := updateAs(t, mgr, "a", view, []editPair{{OldText: testZmqBind, NewText: testZmqBindChanged}}); err != nil {
		t.Fatal(err)
	}
	// Old revision now stale.
	_, err := updateAs(t, mgr, "a", view, []editPair{{OldText: testZmqBindChanged, NewText: "bind: tcp://127.0.0.1:7602"}})
	if err == nil || !strings.Contains(err.Error(), "stage_id/expected_revision") {
		t.Fatalf("stale revision must refuse, got: %v", err)
	}
	// Wrong stage id refused as follows.
	_, err = mgr.Update(context.Background(), "stage-other", 1, []editPair{{OldText: "x", NewText: "y"}})
	if err == nil {
		t.Fatal("foreign stage id must refuse")
	}
	_ = fs
}

func TestConfigManagerConflictInvalidatesBase(t *testing.T) {
	t.Parallel()

	fs, mgr := newTestManager(t, validMiniConfig(), "a")
	view := readAs(t, mgr)
	// External edit between read and save.
	fs.doc = strings.Replace(validMiniConfig(), testZmqBind, "bind: tcp://127.0.0.1:9999", 1)

	_, err := saveAs(t, mgr, "a", view)
	if err == nil || !strings.Contains(err.Error(), "changed since the staging was opened") {
		t.Fatalf("external change at save must refuse, got: %v", err)
	}
	if fs.commits != 0 {
		t.Fatalf("a conflicted save must not replace the file, commits=%d", fs.commits)
	}

	// Owner can still inspect the conflicted staging.
	out, err := mgr.Read(context.Background())
	if err != nil {
		t.Fatalf("owner read of conflicted staging: %v", err)
	}
	var view2 stagedView
	if !strings.Contains(out, `"conflicted":true`) {
		_ = jsonUnmarshal(out, &view2)
		if !view2.Conflicted {
			t.Fatalf("conflicted state must surface: %s", out)
		}
	}
	// Further update/save refused.
	_, err = mgr.Update(context.Background(), view.StageID, view.Revision, []editPair{{OldText: "bind", NewText: "bind2"}})
	if err == nil || !strings.Contains(err.Error(), "invalidated") {
		t.Fatalf("update against invalidated base must refuse, got: %v", err)
	}

	// Recovery: discard then fresh read.
	if _, err = mgr.Discard(context.Background(), view.StageID, view.Revision); err != nil {
		t.Fatalf("discard: %v", err)
	}
	fs.doc = validMiniConfig()
	if _, err = mgr.Read(context.Background()); err != nil {
		t.Fatalf("fresh read after conflict recovery: %v", err)
	}
}

func TestConfigManagerNoOpSaveAndReplayReceipt(t *testing.T) {
	t.Parallel()

	fs, mgr := newTestManager(t, validMiniConfig(), "a")
	view := readAs(t, mgr)

	// Zero-edit save: byte-identical candidate, no commit, no backup.
	out, err := saveAs(t, mgr, "a", view)
	if err != nil {
		t.Fatalf("no-op save: %v", err)
	}
	if !strings.Contains(out, "no_changes") || fs.commits != 0 {
		t.Fatalf("zero-edit save must be a no-op:\n%s commits=%d", out, fs.commits)
	}
	if strings.Contains(out, "backup_path") {
		t.Fatalf("no-op save must not create a backup:\n%s", out)
	}

	// Staging is closed; a repeated save returns the receipt, not a second
	// commit attempt.
	out2, err := mgr.Save(context.Background(), view.StageID, view.Revision)
	if err != nil {
		t.Fatalf("replay save: %v", err)
	}
	if strings.Contains(out2, "stage-test-1") && !strings.Contains(out2, "no_changes") {
		t.Fatalf("replay must return the completed receipt:\n%s", out2)
	}
	if fs.commits != 0 {
		t.Fatalf("replay must not write again, commits=%d", fs.commits)
	}
}

func TestConfigManagerCommittedSaveReleasesAndReplays(t *testing.T) {
	t.Parallel()

	fs, mgr := newTestManager(t, validMiniConfig(), "a")
	view := readAs(t, mgr)
	next, err := updateAs(t, mgr, "a", view, []editPair{{OldText: testZmqBind, NewText: testZmqBindChanged}})
	if err != nil {
		t.Fatal(err)
	}
	var upd updateResult
	_ = jsonUnmarshal(next, &upd)

	out, err := mgr.Save(context.Background(), upd.StageID, upd.Revision)
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	if !strings.Contains(out, "committed") || !strings.Contains(out, "restart") {
		t.Fatalf("committed result must carry backup and restart guidance:\n%s", out)
	}
	if fs.commits != 1 || !strings.Contains(string(fs.lastCand), "7601") {
		t.Fatalf("commit must receive the reconstructed candidate:\n%s", fs.lastCand)
	}

	// Staging is gone; foreign session gets no receipt access.
	before := fs.commits
	if _, err = mgr.Save(context.Background(), upd.StageID, upd.Revision); err != nil {
		t.Fatalf("same-session replay expected, got: %v", err)
	}
	if fs.commits != before {
		t.Fatal("replay must not re-commit")
	}
	fs.sessionID = testForeignSession
	if _, err = mgr.Save(context.Background(), upd.StageID, upd.Revision); err == nil || !strings.Contains(err.Error(), "read the configuration first") {
		t.Fatalf("foreign replay must not surface receipt details, got: %v", err)
	}
}

func TestConfigManagerSessionEndReleasesStaging(t *testing.T) {
	t.Parallel()

	fs, mgr := newTestManager(t, validMiniConfig(), "a")
	if _, err := mgr.Read(context.Background()); err != nil {
		t.Fatal(err)
	}

	// Connection loss / reconnect: nothing released — the same Session continues.
	if _, err := mgr.Read(context.Background()); err != nil {
		t.Fatal(err)
	}

	mgr.ReleaseSession("other-session")
	viewOut, rerr := mgr.Read(context.Background())
	if rerr != nil {
		t.Fatalf("read after foreign release: %v", rerr)
	}
	if !strings.Contains(viewOut, `"stage_id":"stage-test-`) {
		t.Fatalf("foreign release must not drop the staging:\n%s", viewOut)
	}

	mgr.ReleaseSession("a")
	fs.sessionID = "a"
	if _, err := mgr.Update(context.Background(), "stage-a", 0, []editPair{{OldText: "a", NewText: "b"}}); err == nil {
		t.Fatal("staging must be released at session end")
	}

	mgr.Close()
	// After shutdown release the receipt is gone too: replay is refused.
	if _, err := mgr.Save(context.Background(), "stage-a", 0); err == nil {
		t.Fatal("shutdown must release receipts")
	}
}

// TestConfigManagerEndToEndFlow walks the acceptance flow with synthetic
// references: read, edit, dirty re-read, foreign
// refusal, external conflict recovery and save replay.
func TestConfigManagerEndToEndFlow(t *testing.T) { //nolint:paralleltest // scripted stateful flow
	ctx := context.Background()

	doc := validMiniConfig()
	fs, mgr := newTestManager(t, doc, "session-a")

	// Phases 1-4: private read, clear edit, dirty re-read, foreign refusal.
	upd := e2eEditPhase(ctx, t, fs, mgr, "")

	// Phases 5-6: external conflict recovery, fresh read, save, replay.
	e2eConflictAndSavePhase(ctx, t, fs, mgr, upd, doc)
}

// e2eEditPhase performs the read/edit/re-read/foreign-refusal steps and
// returns the committed update result.
func e2eEditPhase(ctx context.Context, t *testing.T, fs *fakeFS, mgr *ConfigManager, _ string) updateResult {
	t.Helper()

	out, err := mgr.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var view stagedView
	if err = jsonUnmarshal(out, &view); err != nil {
		t.Fatal(err)
	}

	next, err := mgr.Update(ctx, view.StageID, view.Revision, []editPair{
		{OldText: testZmqBind, NewText: testZmqBindChanged},
	})
	if err != nil {
		t.Fatal(err)
	}
	var upd updateResult
	if err = jsonUnmarshal(next, &upd); err != nil {
		t.Fatal(err)
	}

	out, err = mgr.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var dirty stagedView
	if err = jsonUnmarshal(out, &dirty); err != nil {
		t.Fatal(err)
	}
	if !dirty.Dirty || dirty.Revision != 1 || !strings.Contains(dirty.Text, "7601") {
		t.Fatalf("dirty re-read wrong: %+v", dirty)
	}

	fs.sessionID = testForeignSession
	if _, err = mgr.Save(ctx, dirty.StageID, dirty.Revision); err == nil || !strings.Contains(err.Error(), "busy") {
		t.Fatalf("foreign save must be busy, got: %v", err)
	}
	fs.sessionID = "session-a"

	return upd
}

// e2eConflictAndSavePhase performs the conflict, recovery, reapply, save and
// replay steps.
func e2eConflictAndSavePhase(ctx context.Context, t *testing.T, fs *fakeFS, mgr *ConfigManager, upd updateResult, doc string) {
	t.Helper()

	fs.doc = strings.Replace(doc, "7600", "9999", 1)
	if _, err := mgr.Save(ctx, upd.StageID, upd.Revision); err == nil || !strings.Contains(err.Error(), "changed since the staging was opened") {
		t.Fatalf("conflict expected, got: %v", err)
	}
	if _, err := mgr.Discard(ctx, upd.StageID, upd.Revision); err != nil {
		t.Fatalf("conflicted staging must be discardable: %v", err)
	}

	fs.doc = doc
	out, err := mgr.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var view stagedView
	if err = jsonUnmarshal(out, &view); err != nil {
		t.Fatal(err)
	}
	next, err := mgr.Update(ctx, view.StageID, view.Revision, []editPair{
		{OldText: testZmqBind, NewText: testZmqBindChanged},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err = jsonUnmarshal(next, &upd); err != nil {
		t.Fatal(err)
	}
	out, err = mgr.Save(ctx, upd.StageID, upd.Revision)
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	if !strings.Contains(out, `"committed"`) {
		t.Fatalf("committed expected:\n%s", out)
	}
	replay, err := mgr.Save(ctx, upd.StageID, upd.Revision)
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if replay != out {
		t.Fatal("replay must return the identical receipt")
	}
}

func TestConfigManagerMasksHandEditedLiteralAndRefusesSave(t *testing.T) {
	t.Parallel()

	doc := strings.Replace(validMiniConfig(), "auth_token: !secret zmqcli_auth_token", "auth_token: "+aLiteral, 1)
	fs, mgr := newTestManager(t, doc, "a")
	view := readAs(t, mgr)
	if strings.Contains(view.Text, aLiteral) || !strings.Contains(view.Text, maskText) {
		t.Fatalf("a literal must be masked by position:\n%s", view.Text)
	}
	// Unrelated edits stay possible while the mask rests in the text...
	next, err := updateAs(t, mgr, "a", view, []editPair{{OldText: testZmqBind, NewText: testZmqBindChanged}})
	if err != nil {
		t.Fatalf("unrelated update next to a masked literal: %v", err)
	}
	var upd updateResult
	_ = jsonUnmarshal(next, &upd)
	// ...but a save is refused until the literal is adopted.
	if _, err = mgr.Save(context.Background(), upd.StageID, upd.Revision); err == nil || !strings.Contains(err.Error(), "ready secrets adopt") {
		t.Fatalf("save with a literal must point at adopt, got: %v", err)
	}
	if fs.commits != 0 {
		t.Fatal("a literal must never be committed")
	}
}

func TestConfigManagerRefusesNewLiteralWithSuggestionAndNoEcho(t *testing.T) {
	t.Parallel()

	_, mgr := newTestManager(t, validMiniConfig(), "a")
	view := readAs(t, mgr)
	_, err := updateAs(t, mgr, "a", view, []editPair{{
		OldText: "auth_token: !secret zmqcli_auth_token", NewText: "auth_token: " + aLiteral,
	}})
	if err == nil || !strings.Contains(err.Error(), "!secret zmqcli_auth_token") {
		t.Fatalf("refusal must suggest the canonical reference, got: %v", err)
	}
	if strings.Contains(err.Error(), aLiteral) {
		t.Fatalf("refusal must not echo the literal: %v", err)
	}
	if readAs(t, mgr).Revision != 0 {
		t.Fatal("a refused update must not change the revision")
	}
}

func TestConfigManagerAcceptsNewReferenceAtSensitivePath(t *testing.T) {
	t.Parallel()

	doc := strings.Replace(validMiniConfig(), "      api: ollama\n", "      api: ollama\n      api_key: !secret ollama_gemma_api_key\n", 1)
	_, mgr := newTestManager(t, doc, "a")
	view := readAs(t, mgr)
	if !strings.Contains(view.Text, "!secret ollama_gemma_api_key") {
		t.Fatalf("reference lost:\n%s", view.Text)
	}
}

func TestConfigManagerAgentIDIsImmutable(t *testing.T) {
	t.Parallel()

	_, mgr := newTestManager(t, validMiniConfig(), "a")
	view := readAs(t, mgr)
	_, err := updateAs(t, mgr, "a", view, []editPair{{OldText: testAgentID, NewText: "0190aaaa-bbbb-7ccc-8ddd-eeeeffff0002"}})
	if err == nil || !strings.Contains(err.Error(), "agent.id is immutable") {
		t.Fatalf("agent.id change must refuse, got: %v", err)
	}
}

func TestConfigManagerZMQReferenceChangeIsZMQSessionOnly(t *testing.T) {
	t.Parallel()

	fs, mgr := newTestManager(t, validMiniConfig(), "a")
	mgr.opts.SessionTransport = func(context.Context) string { return "stdin" }
	_ = fs
	view := readAs(t, mgr)
	_, err := updateAs(t, mgr, "a", view, []editPair{{OldText: "!secret zmqcli_auth_token", NewText: "!secret other_token"}})
	if err == nil || !strings.Contains(err.Error(), "ZMQ") {
		t.Fatalf("non-ZMQ session must be refused, got: %v", err)
	}
	mgr.opts.SessionTransport = func(context.Context) string { return "zmq" }
	if _, err = updateAs(t, mgr, "a", view, []editPair{{OldText: "!secret zmqcli_auth_token", NewText: "!secret other_token"}}); err != nil {
		t.Fatalf("a ZMQ session may change it: %v", err)
	}
}

func TestConfigManagerSaveOnCommitAndChannelAwareInstructions(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	_, mgr := newTestManager(t, validMiniConfig(), "a")

	var commitCalls int
	var lastLoopback bool
	mgr.opts.OnCommit = func(_ context.Context, isLoopbackZMQTUI bool) {
		commitCalls++
		lastLoopback = isLoopbackZMQTUI
	}

	// 1. No-op save does not invoke OnCommit.
	view0 := readAs(t, mgr)
	if _, err := saveAs(t, mgr, "a", view0); err != nil {
		t.Fatalf("no-op save: %v", err)
	}
	if commitCalls != 0 {
		t.Fatalf("OnCommit called on no-op save: %d", commitCalls)
	}

	// 2. Loopback ZMQ TUI save invokes OnCommit(true) once, returns TUI
	// post-save activation instructions, and replay does not invoke OnCommit again.
	view1 := readAs(t, mgr)
	updRaw, err := updateAs(t, mgr, "a", view1, []editPair{{OldText: testZmqBind, NewText: testZmqBindChanged}})
	if err != nil {
		t.Fatal(err)
	}
	var upd updateResult
	_ = jsonUnmarshal(updRaw, &upd)

	saveOut, err := mgr.Save(ctx, upd.StageID, upd.Revision)
	if err != nil {
		t.Fatalf("loopback save: %v", err)
	}
	if commitCalls != 1 || !lastLoopback {
		t.Fatalf("OnCommit calls=%d lastLoopback=%v, want 1/true", commitCalls, lastLoopback)
	}
	if !strings.Contains(saveOut, "RED secret-entry editor") || !strings.Contains(saveOut, "OAuth logins") || !strings.Contains(saveOut, "reload the daemon runtime in-process") {
		t.Fatalf("unexpected loopback TUI save instruction:\n%s", saveOut)
	}

	replayOut, err := mgr.Save(ctx, upd.StageID, upd.Revision)
	if err != nil {
		t.Fatalf("replay save: %v", err)
	}
	if replayOut != saveOut || commitCalls != 1 {
		t.Fatalf("replay changed output or re-invoked OnCommit (calls=%d)", commitCalls)
	}

	// 3. Telegram / non-loopback save invokes OnCommit(false) and returns
	// immediate turn-end reload instructions; observer panic does not undo commit.
	mgr.opts.SessionTransport = func(context.Context) string { return "telegram" }
	mgr.opts.SessionLoopback = func(context.Context) bool { return false }
	mgr.opts.OnCommit = func(_ context.Context, isLoopbackZMQTUI bool) {
		commitCalls++
		lastLoopback = isLoopbackZMQTUI
		panic("observer boom")
	}

	view2 := readAs(t, mgr)
	updRaw2, err := updateAs(t, mgr, "a", view2, []editPair{{OldText: testZmqBindChanged, NewText: testZmqBind}})
	if err != nil {
		t.Fatal(err)
	}
	_ = jsonUnmarshal(updRaw2, &upd)

	tgOut, err := mgr.Save(ctx, upd.StageID, upd.Revision)
	if err != nil {
		t.Fatalf("telegram save with panicking observer failed: %v", err)
	}
	if commitCalls != 2 || lastLoopback {
		t.Fatalf("OnCommit calls=%d lastLoopback=%v, want 2/false", commitCalls, lastLoopback)
	}
	if !strings.Contains(tgOut, "reloads its runtime automatically at the end of this turn") || !strings.Contains(tgOut, "ready secrets set") {
		t.Fatalf("unexpected telegram save instruction:\n%s", tgOut)
	}
}
