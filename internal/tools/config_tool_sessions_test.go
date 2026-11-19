package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/marco-rozz/ready/internal/config"
	"github.com/marco-rozz/ready/internal/errdigest"
	"github.com/rs/zerolog"
)

func TestConfigToolIndependentDrafts(t *testing.T) {
	fs, mgr := newTestManager(t, validMiniConfig(), "a")
	tool := NewConfigManagerTool(mgr)
	ctx := context.Background()
	call := func(args string) string {
		t.Helper()
		result, err := tool.InvokableRun(ctx, args)
		if err != nil {
			t.Fatalf("%s: %v", args, err)
		}

		return result
	}
	if out := call(`{"action":"read"}`); strings.Contains(out, "stage_id") {
		t.Fatal("read allocated a draft")
	}
	var a, b stagedView
	if err := json.Unmarshal([]byte(call(`{"action":"begin"}`)), &a); err != nil {
		t.Fatal(err)
	}
	fs.sessionID = "b"
	if err := json.Unmarshal([]byte(call(`{"action":"begin"}`)), &b); err != nil {
		t.Fatal(err)
	}
	if a.StageID == b.StageID {
		t.Fatal("sessions share a draft")
	}
	foreign := fmt.Sprintf(`{"action":"discard","stage_id":%q,"expected_revision":0}`, a.StageID)
	if _, err := tool.InvokableRun(ctx, foreign); err == nil {
		t.Fatal("foreign stage accepted")
	}
	fs.sessionID = "a"
	args := fmt.Sprintf(`{"action":"update","stage_id":%q,"expected_revision":0,"edits":[{"old_text":%q,"new_text":%q}]}`, a.StageID, testZmqBind, testZmqBindChanged)
	call(args)
	if out := call(`{"action":"inspect"}`); !strings.Contains(out, testZmqBindChanged) {
		t.Fatal("owner draft lost")
	}
	if out := call(`{"action":"read"}`); strings.Contains(out, testZmqBindChanged) {
		t.Fatal("saved read exposed draft")
	}
	fs.sessionID = "b"
	if out := call(`{"action":"inspect"}`); strings.Contains(out, testZmqBindChanged) {
		t.Fatal("foreign draft exposed")
	}
	mgr.ReleaseSession("a")
	fs.sessionID = "a"
	if _, err := tool.InvokableRun(ctx, `{"action":"inspect"}`); err == nil {
		t.Fatal("ended session retained draft")
	}
}

func TestConfigToolIndependentSaveReceiptsAndConflicts(t *testing.T) {
	fs, mgr := newTestManager(t, validMiniConfig(), "a")
	tool := NewConfigManagerTool(mgr)
	ctx := context.Background()
	begin := func() stagedView {
		t.Helper()
		out, err := tool.InvokableRun(ctx, `{"action":"begin"}`)
		if err != nil {
			t.Fatal(err)
		}
		var view stagedView
		if err = json.Unmarshal([]byte(out), &view); err != nil {
			t.Fatal(err)
		}

		return view
	}
	a := begin()
	fs.sessionID = "b"
	b := begin()
	fs.sessionID = "a"
	edit := fmt.Sprintf(`{"action":"update","stage_id":%q,"expected_revision":0,"edits":[{"old_text":%q,"new_text":%q}]}`, a.StageID, testZmqBind, testZmqBindChanged)
	if _, err := tool.InvokableRun(ctx, edit); err != nil {
		t.Fatal(err)
	}
	save := fmt.Sprintf(`{"action":"save","stage_id":%q,"expected_revision":1}`, a.StageID)
	first, err := tool.InvokableRun(ctx, save)
	if err != nil {
		t.Fatal(err)
	}
	fs.sessionID = "b"
	staleSave := fmt.Sprintf(`{"action":"save","stage_id":%q,"expected_revision":0}`, b.StageID)
	if _, err = tool.InvokableRun(ctx, staleSave); !errors.Is(err, config.ErrConfigSourceConflict) {
		t.Fatalf("stale save: %v", err)
	}
	fs.sessionID = "a"
	replay, err := tool.InvokableRun(ctx, save)
	if err != nil || replay != first || fs.commits != 1 {
		t.Fatalf("receipt replay: %v, commits %d", err, fs.commits)
	}
}

func TestConfigToolDraftLimitKeepsExistingDrafts(t *testing.T) {
	fs, mgr := newTestManager(t, validMiniConfig(), "")
	tool := NewConfigManagerTool(mgr)
	ctx := context.Background()
	for i := 0; i < maxConfigDraftSessions; i++ {
		fs.sessionID = fmt.Sprintf("session-%d", i)
		if _, err := tool.InvokableRun(ctx, `{"action":"begin"}`); err != nil {
			t.Fatalf("begin %d: %v", i, err)
		}
	}
	fs.sessionID = "overflow"
	if _, err := tool.InvokableRun(ctx, `{"action":"begin"}`); err == nil {
		t.Fatal("capacity must refuse a new draft")
	}
	fs.sessionID = "session-0"
	if _, err := tool.InvokableRun(ctx, `{"action":"inspect"}`); err != nil {
		t.Fatalf("existing draft lost: %v", err)
	}
	mgr.ReleaseSession("session-0")
	fs.sessionID = "overflow"
	if _, err := tool.InvokableRun(ctx, `{"action":"begin"}`); err != nil {
		t.Fatalf("released capacity: %v", err)
	}
}

func TestConfigToolDraftSurvivesGenerationReplacement(t *testing.T) {
	fs, first := newTestManager(t, validMiniConfig(), "owner")
	ctx := context.Background()
	oldTool := NewConfigManagerTool(first)
	opened, err := oldTool.InvokableRun(ctx, `{"action":"begin"}`)
	if err != nil {
		t.Fatal(err)
	}
	opts := first.opts
	opts.State = first.ConfigManagerState
	second, err := NewConfigManager(opts)
	if err != nil {
		t.Fatal(err)
	}
	got, err := NewConfigManagerTool(second).InvokableRun(ctx, `{"action":"inspect"}`)
	if err != nil {
		t.Fatal(err)
	}
	var a, b stagedView
	if err = json.Unmarshal([]byte(opened), &a); err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal([]byte(got), &b); err != nil {
		t.Fatal(err)
	}
	if a.StageID != b.StageID || a.Revision != b.Revision || a.Text != b.Text {
		t.Fatal("draft did not survive generation replacement")
	}
	second.ReleaseSession(fs.sessionID)
	if _, err = oldTool.InvokableRun(ctx, `{"action":"inspect"}`); err == nil {
		t.Fatal("ended session retained access")
	}
}

func TestConfigToolShutdownReleasesDraftsAndReceipts(t *testing.T) {
	_, mgr := newTestManager(t, validMiniConfig(), "owner")
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
	save := fmt.Sprintf(`{"action":"save","stage_id":%q,"expected_revision":0}`, view.StageID)
	if _, err = tool.InvokableRun(ctx, save); err != nil {
		t.Fatal(err)
	}
	if _, err = tool.InvokableRun(ctx, `{"action":"begin"}`); err != nil {
		t.Fatal(err)
	}
	mgr.ConfigManagerState.Close()
	if _, err = tool.InvokableRun(ctx, `{"action":"inspect"}`); err == nil {
		t.Fatal("shutdown retained draft")
	}
	if _, err = tool.InvokableRun(ctx, save); err == nil {
		t.Fatal("shutdown retained receipt")
	}
}

func TestConfigToolParallelSessions(t *testing.T) {
	_, template := newTestManager(t, validMiniConfig(), "unused")
	type key struct{}
	opts := template.opts
	opts.SessionIdentity = func(ctx context.Context) (string, bool) {
		id, ok := ctx.Value(key{}).(string)

		return id, ok
	}
	var counter atomic.Int32
	opts.nextStageID = func() (string, error) { return fmt.Sprintf("stage-%d", counter.Add(1)), nil }
	mgr, err := NewConfigManager(opts)
	if err != nil {
		t.Fatal(err)
	}
	tool := NewConfigManagerTool(mgr)
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ctx := context.WithValue(context.Background(), key{}, fmt.Sprintf("session-%d", i))
			out, err := tool.InvokableRun(ctx, `{"action":"begin"}`)
			if err != nil {
				t.Errorf("begin: %v", err)

				return
			}
			var view stagedView
			if err = json.Unmarshal([]byte(out), &view); err != nil {
				t.Error(err)

				return
			}
			edit := fmt.Sprintf(`{"action":"update","stage_id":%q,"expected_revision":0,"edits":[{"old_text":%q,"new_text":%q}]}`, view.StageID, testZmqBind, testZmqBindChanged)
			if _, err = tool.InvokableRun(ctx, edit); err != nil {
				t.Errorf("update: %v", err)
			}
		}(i)
	}
	wg.Wait()
}

func TestConfigToolSameRevisionRace(t *testing.T) {
	_, mgr := newTestManager(t, validMiniConfig(), "owner")
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
	args := fmt.Sprintf(`{"action":"update","stage_id":%q,"expected_revision":0,"edits":[{"old_text":%q,"new_text":%q}]}`, view.StageID, testZmqBind, testZmqBindChanged)
	results := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func() { _, runErr := tool.InvokableRun(ctx, args); results <- runErr }()
	}
	first, second := <-results, <-results
	if (first != nil || !errors.Is(second, ErrConfigStaleState)) && (second != nil || !errors.Is(first, ErrConfigStaleState)) {
		t.Fatalf("expected one successful update and one stale revision: %v / %v", first, second)
	}
}

func TestConfigToolSharedStateAcrossConcurrentAdapters(t *testing.T) {
	_, template := newTestManager(t, validMiniConfig(), "unused")
	type key struct{}
	opts := template.opts
	opts.SessionIdentity = func(ctx context.Context) (string, bool) {
		id, ok := ctx.Value(key{}).(string)

		return id, ok
	}
	var ids atomic.Int32
	opts.nextStageID = func() (string, error) { return fmt.Sprintf("stage-%d", ids.Add(1)), nil }
	first, err := NewConfigManager(opts)
	if err != nil {
		t.Fatal(err)
	}
	opts.State = first.ConfigManagerState
	second, err := NewConfigManager(opts)
	if err != nil {
		t.Fatal(err)
	}
	oldTool, newTool := NewConfigManagerTool(first), NewConfigManagerTool(second)
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ctx := context.WithValue(context.Background(), key{}, fmt.Sprintf("session-%d", i))
			if _, runErr := oldTool.InvokableRun(ctx, `{"action":"begin"}`); runErr != nil {
				t.Error(runErr)

				return
			}
			for n := 0; n < 5; n++ {
				if _, runErr := newTool.InvokableRun(ctx, `{"action":"inspect"}`); runErr != nil {
					t.Error(runErr)

					return
				}
			}
		}(i)
	}
	wg.Wait()
}

func TestConfigToolConcurrentCommits(t *testing.T) {
	type key struct{}
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(validMiniConfig()), 0o600); err != nil { // #nosec G703 -- path is under t.TempDir
		t.Fatal(err)
	}
	mgr, err := NewConfigManager(ConfigManagerOptions{
		ElectedPath: path,
		SessionIdentity: func(ctx context.Context) (string, bool) {
			id, ok := ctx.Value(key{}).(string)

			return id, ok
		},
		SessionTransport: func(context.Context) string { return testTransportZMQ },
	})
	if err != nil {
		t.Fatal(err)
	}
	tool := NewConfigManagerTool(mgr)
	type request struct {
		ctx  context.Context
		save string
	}
	requests := make([]request, 0, 2)
	for i := 0; i < 2; i++ {
		ctx := context.WithValue(context.Background(), key{}, fmt.Sprintf("session-%d", i))
		out, runErr := tool.InvokableRun(ctx, `{"action":"begin"}`)
		if runErr != nil {
			t.Fatal(runErr)
		}
		var view stagedView
		if runErr = json.Unmarshal([]byte(out), &view); runErr != nil {
			t.Fatal(runErr)
		}
		next := fmt.Sprintf("bind: tcp://127.0.0.1:%d", 7601+i)
		update := fmt.Sprintf(`{"action":"update","stage_id":%q,"expected_revision":0,"edits":[{"old_text":%q,"new_text":%q}]}`, view.StageID, testZmqBind, next)
		if _, runErr = tool.InvokableRun(ctx, update); runErr != nil {
			t.Fatal(runErr)
		}
		requests = append(requests, request{ctx: ctx, save: fmt.Sprintf(`{"action":"save","stage_id":%q,"expected_revision":1}`, view.StageID)})
	}
	results := make(chan error, 2)
	for _, req := range requests {
		go func(req request) { _, runErr := tool.InvokableRun(req.ctx, req.save); results <- runErr }(req)
	}
	a, b := <-results, <-results
	if (a != nil || !errors.Is(b, config.ErrConfigSourceConflict)) && (b != nil || !errors.Is(a, config.ErrConfigSourceConflict)) {
		t.Fatalf("concurrent saves: %v / %v", a, b)
	}
}

func TestConfigToolCommittedWarningReceiptReplay(t *testing.T) {
	_, template := newTestManager(t, validMiniConfig(), "owner")
	opts := template.opts
	commits := 0
	opts.commit = func(context.Context, config.SourceState, []byte) (config.CommitReport, error) {
		commits++

		return config.CommitReport{Committed: true}, config.ErrCommitPostCommit
	}
	mgr, err := NewConfigManager(opts)
	if err != nil {
		t.Fatal(err)
	}
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
	update := fmt.Sprintf(`{"action":"update","stage_id":%q,"expected_revision":0,"edits":[{"old_text":%q,"new_text":%q}]}`, view.StageID, testZmqBind, testZmqBindChanged)
	if _, err = tool.InvokableRun(ctx, update); err != nil {
		t.Fatal(err)
	}
	save := fmt.Sprintf(`{"action":"save","stage_id":%q,"expected_revision":1}`, view.StageID)
	first, err := tool.InvokableRun(ctx, save)
	if err != nil || !strings.Contains(first, "warnings") {
		t.Fatalf("committed warning: %s %v", first, err)
	}
	replay, err := tool.InvokableRun(ctx, save)
	if err != nil || replay != first || commits != 1 {
		t.Fatalf("receipt replay: %v, %d commits", err, commits)
	}
}

func TestConfigToolLimitsCorrectiveAttempts(t *testing.T) {
	_, mgr := newTestManager(t, validMiniConfig(), "owner")
	tool := NewConfigManagerTool(mgr)
	ctx := errdigest.WithTurnGuard(context.Background(), 32)
	out, err := tool.InvokableRun(ctx, `{"action":"begin"}`)
	if err != nil {
		t.Fatal(err)
	}
	var view stagedView
	if err = json.Unmarshal([]byte(out), &view); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		args := fmt.Sprintf(`{"action":"update","stage_id":%q,"expected_revision":0,"edits":[{"old_text":%q,"new_text":"x"}]}`, view.StageID, fmt.Sprintf("missing-%d", i))
		_, err = tool.InvokableRun(ctx, args)
		if i < 2 && !errors.Is(err, ErrConfigExactMatch) {
			t.Fatalf("attempt %d: %v", i, err)
		}
		if i == 2 && !errors.Is(err, errdigest.ErrGuardEditRetries) {
			t.Fatalf("limit: %v", err)
		}
		if _, inspectErr := tool.InvokableRun(ctx, `{"action":"inspect"}`); inspectErr != nil {
			t.Fatal(inspectErr)
		}
	}
	out, err = tool.InvokableRun(ctx, `{"action":"inspect"}`)
	if err != nil {
		t.Fatal(err)
	}
	var after stagedView
	if err = json.Unmarshal([]byte(out), &after); err != nil {
		t.Fatal(err)
	}
	if after.Text != view.Text || after.Revision != view.Revision {
		t.Fatal("failed updates changed the draft")
	}
	good := fmt.Sprintf(`{"action":"update","stage_id":%q,"expected_revision":0,"edits":[{"old_text":%q,"new_text":%q}]}`, view.StageID, testZmqBind, testZmqBindChanged)
	if _, err = tool.InvokableRun(ctx, good); !errors.Is(err, errdigest.ErrGuardEditRetries) {
		t.Fatalf("exhausted turn accepted another edit: %v", err)
	}
}

func TestConfigToolAcceptsCorrectedEdit(t *testing.T) {
	_, mgr := newTestManager(t, validMiniConfig(), "owner")
	tool := NewConfigManagerTool(mgr)
	ctx := errdigest.WithTurnGuard(context.Background(), 32)
	out, err := tool.InvokableRun(ctx, `{"action":"begin"}`)
	if err != nil {
		t.Fatal(err)
	}
	var view stagedView
	if err = json.Unmarshal([]byte(out), &view); err != nil {
		t.Fatal(err)
	}
	bad := fmt.Sprintf(`{"action":"update","stage_id":%q,"expected_revision":0,"edits":[{"old_text":"missing","new_text":"x"}]}`, view.StageID)
	if _, err = tool.InvokableRun(ctx, bad); !errors.Is(err, ErrConfigExactMatch) {
		t.Fatalf("initial error: %v", err)
	}
	good := fmt.Sprintf(`{"action":"update","stage_id":%q,"expected_revision":0,"edits":[{"old_text":%q,"new_text":%q}]}`, view.StageID, testZmqBind, testZmqBindChanged)
	if _, err = tool.InvokableRun(ctx, good); err != nil {
		t.Fatalf("corrected update: %v", err)
	}
}

func TestConfigToolBeginRepairsMissingDraftInspect(t *testing.T) {
	_, mgr := newTestManager(t, validMiniConfig(), "owner")
	classifier := errdigest.NewClassifier().Register(ErrConfigNoStaging, errdigest.Permanent)
	tool := errdigest.NewTrackedTool(NewConfigManagerTool(mgr), classifier, func(context.Context) string { return "owner" }, zerolog.Nop())
	ctx := errdigest.WithTurnGuard(context.Background(), 32)
	if _, err := tool.InvokableRun(ctx, `{"action":"inspect"}`); err != nil {
		t.Fatal(err)
	}
	if _, err := tool.InvokableRun(ctx, `{"action":"begin"}`); err != nil {
		t.Fatal(err)
	}
	if out, err := tool.InvokableRun(ctx, `{"action":"inspect"}`); err != nil || !strings.Contains(out, `"stage_id"`) {
		t.Fatalf("inspect after begin: %s %v", out, err)
	}
}
