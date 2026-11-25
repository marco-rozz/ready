package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rs/zerolog"

	"github.com/marco-rozz/ready/internal/config"
	"github.com/marco-rozz/ready/internal/errdigest"
	"github.com/marco-rozz/ready/internal/llm"
	"github.com/marco-rozz/ready/internal/okf"
	"github.com/marco-rozz/ready/internal/skillcache"
	"github.com/marco-rozz/ready/internal/skills"
)

const okfProfileDefault = "default"

// okf_query refusals are PERMANENT, filesystem failures TRANSIENT (D12).
func TestErrDigestClassifierOKF(t *testing.T) {
	c := errDigestClassifier()
	for _, err := range []error{
		okf.ErrUnknownAction, okf.ErrUnknownBundle, okf.ErrUnknownConcept,
		okf.ErrInvalidReference, okf.ErrOutsideBundle, okf.ErrKnowledgeRootEscape,
		fmt.Errorf("wrapped: %w", okf.ErrUnknownBundle),
	} {
		if got := c.Classify(err); got != errdigest.Permanent {
			t.Errorf("%v classified %q, want %q", err, got, errdigest.Permanent)
		}
	}
	if got := c.Classify(fmt.Errorf("%w: boom", okf.ErrIO)); got != errdigest.Transient {
		t.Errorf("ErrIO classified %q, want %q", got, errdigest.Transient)
	}
}

func okfToolNames(t *testing.T, cfg *config.Config, profile string) map[string]bool {
	t.Helper()
	ctx := context.Background()
	skillsMgr, err := skills.NewManager(ctx, &config.SkillsConfig{Path: t.TempDir()}, zerolog.Nop())
	if err != nil {
		t.Fatalf("skills.NewManager: %v", err)
	}
	sc, err := skillcache.New()
	if err != nil {
		t.Fatalf("skillcache.New: %v", err)
	}
	defer func() { _ = sc.Close() }()

	built, rt, _, _ := buildTools(ctx, cfg, skillsMgr, sc, true, profile, nil, zerolog.Nop(), config.ProfileOverrideInputs{}, nil, nil)
	if rt != nil {
		defer func() { _ = rt.Close() }()
	}
	got := map[string]bool{}
	for _, tl := range built {
		if info, ierr := tl.Info(ctx); ierr == nil {
			got[info.Name] = true
		}
	}

	return got
}

// Gating matrix: absent builtin_tools block = allow-all default; an explicit
// entry filters okf_query by name like every builtin tool.
func TestBuildTools_OKFQueryGating(t *testing.T) {
	home := t.TempDir()
	base := func(profiles map[string]config.OwnerProfileConfig) *config.Config {
		return &config.Config{
			RuntimeHome:  home,
			BuiltinTools: config.BuiltinToolsConfig{Profiles: profiles},
			Profiles:     map[string]config.ProfileConfig{okfProfileDefault: {}, "locked": {}},
		}
	}

	// config.applyDefaults installs this entry when the block is absent
	// (denies only config-manager): okf_query must survive it.
	implicit := base(map[string]config.OwnerProfileConfig{okfProfileDefault: {Deny: []string{"config-manager"}}})
	if !okfToolNames(t, implicit, okfProfileDefault)[okf.ToolName] {
		t.Error("implicit default: okf_query must be registered")
	}
	allow := base(map[string]config.OwnerProfileConfig{okfProfileDefault: {Allow: []string{"*"}}, "locked": {Allow: []string{"wait"}}})
	if !okfToolNames(t, allow, okfProfileDefault)[okf.ToolName] {
		t.Error("allow *: okf_query must be registered")
	}
	if okfToolNames(t, allow, "locked")[okf.ToolName] {
		t.Error("profile allowing only wait must not expose okf_query")
	}
	deny := base(map[string]config.OwnerProfileConfig{okfProfileDefault: {Deny: []string{"okf_*"}}})
	if okfToolNames(t, deny, okfProfileDefault)[okf.ToolName] {
		t.Error("deny okf_*: okf_query must not be registered")
	}
	if got := okfToolNames(t, &config.Config{Profiles: map[string]config.ProfileConfig{okfProfileDefault: {}}}, okfProfileDefault); got[okf.ToolName] {
		t.Error("without a runtime home the tool is not registered")
	}
}

// The registered tool answers through the digest wrapper with the
// [PERMANENT] tag on an unknown bundle.
func TestOKFQueryObservationThroughDigest(t *testing.T) {
	tl := okf.NewTool(okf.Options{AgentDir: t.TempDir()})
	tracked := errdigest.NewTrackedTool(tl, errDigestClassifier(),
		func(context.Context) string { return testCLIChannelName }, zerolog.Nop())
	out, err := tracked.InvokableRun(t.Context(), `{"action":"scan","bundle":"ghost"}`)
	if err != nil {
		t.Fatalf("tracked tool must not fail the call: %v", err)
	}
	if !strings.Contains(out, "[PERMANENT]") || !strings.Contains(out, "unknown bundle") {
		t.Fatalf("observation = %q", out)
	}
}

// okf_write refusals are PERMANENT, a stale base_digest is the only
// TRANSIENT write failure (okf-authoring).
func TestErrDigestClassifierOKFWrite(t *testing.T) {
	c := errDigestClassifier()
	for _, err := range []error{
		okf.ErrReservedField, okf.ErrUnmergeable, okf.ErrSymlinkTarget, okf.ErrAlreadyExists,
		okf.ErrInvalidCreationPath, okf.ErrReservedName, okf.ErrUnresolvedSuperseded,
		okf.ErrGateRefused, okf.ErrOversize, okf.ErrTypeProtected, okf.ErrInvalidWriteArgs,
		fmt.Errorf("wrapped: %w", okf.ErrReservedField),
	} {
		if got := c.Classify(err); got != errdigest.Permanent {
			t.Errorf("%v classified %q, want %q", err, got, errdigest.Permanent)
		}
	}
	if got := c.Classify(fmt.Errorf("%w (current digest x)", okf.ErrConflict)); got != errdigest.Transient {
		t.Errorf("ErrConflict classified %q, want %q", got, errdigest.Transient)
	}
}

// Gating matrix: okf_query allowed, okf_write denied is a read-only profile;
// the implicit default (denies only config-manager) keeps both.
func TestBuildTools_OKFWriteGating(t *testing.T) {
	home := t.TempDir()
	base := func(profiles map[string]config.OwnerProfileConfig) *config.Config {
		return &config.Config{
			RuntimeHome:  home,
			BuiltinTools: config.BuiltinToolsConfig{Profiles: profiles},
			Profiles:     map[string]config.ProfileConfig{okfProfileDefault: {}},
		}
	}
	implicit := okfToolNames(t, base(map[string]config.OwnerProfileConfig{okfProfileDefault: {Deny: []string{"config-manager"}}}), okfProfileDefault)
	if !implicit[okf.ToolName] || !implicit[okf.WriteToolName] {
		t.Errorf("implicit default must register both tools: %v", implicit)
	}
	readOnly := okfToolNames(t, base(map[string]config.OwnerProfileConfig{okfProfileDefault: {Deny: []string{okf.WriteToolName}}}), okfProfileDefault)
	if !readOnly[okf.ToolName] || readOnly[okf.WriteToolName] {
		t.Errorf("read-only profile: %v", readOnly)
	}
	if got := okfToolNames(t, &config.Config{Profiles: map[string]config.ProfileConfig{okfProfileDefault: {}}}, okfProfileDefault); got[okf.WriteToolName] {
		t.Error("without a runtime home okf_write is not registered")
	}
}

// The registered writer stamps the attempting model's reference and answers
// through the digest wrapper with the [PERMANENT] tag on a reserved field.
func TestOKFWriteThroughDigest(t *testing.T) {
	agent := t.TempDir()
	if err := os.MkdirAll(filepath.Join(agent, okf.KnowledgeDirName, "home"), 0o700); err != nil {
		t.Fatal(err)
	}
	w := okf.NewWriter(okf.WriterOptions{AgentDir: agent, Version: "test"})
	tl := okf.NewWriteTool(okf.WriteToolOptions{Writer: w, ModelFrom: func(ctx context.Context) string {
		ref, _ := llm.AttemptReference(ctx)

		return ref
	}})
	tracked := errdigest.NewTrackedTool(tl, errDigestClassifier(),
		func(context.Context) string { return testCLIChannelName }, zerolog.Nop())

	ctx := llm.WithAttemptReference(t.Context(), "or::model-x")
	out, err := tracked.InvokableRun(ctx, `{"action":"create","bundle":"home","path":"a/b","fields":{"type":"Metric"},"body":"hi"}`)
	if err != nil || !strings.Contains(out, "created") {
		t.Fatalf("create: %v %q", err, out)
	}
	data, rerr := os.ReadFile(filepath.Join(agent, okf.KnowledgeDirName, "home", "a", "b.md"))
	if rerr != nil || !strings.Contains(string(data), `x-ready-model: "or::model-x"`) {
		t.Fatalf("concept: %v\n%s", rerr, data)
	}

	out, err = tracked.InvokableRun(t.Context(), `{"action":"create","bundle":"home","path":"a/c","fields":{"type":"Metric","verified":[]}}`)
	if err != nil || !strings.Contains(out, "[PERMANENT]") || !strings.Contains(out, "/verify") {
		t.Fatalf("reserved field observation = %q (%v)", out, err)
	}
}
