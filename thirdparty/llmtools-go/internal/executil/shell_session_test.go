package executil

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidateEnvMap(t *testing.T) {
	if err := ValidateEnvMap(map[string]string{"OK": "1"}); err != nil {
		t.Fatalf("expected ok, got: %v", err)
	}

	if err := ValidateEnvMap(map[string]string{"": "1"}); err == nil {
		t.Fatalf("expected error for empty key")
	}

	if err := ValidateEnvMap(map[string]string{"A=B": "1"}); err == nil {
		t.Fatalf("expected error for key containing '='")
	}

	if err := ValidateEnvMap(map[string]string{"A\x00": "1"}); err == nil {
		t.Fatalf("expected error for NUL in key")
	}

	if err := ValidateEnvMap(map[string]string{"A": "1\x00"}); err == nil {
		t.Fatalf("expected error for NUL in value")
	}
}

func TestEffectiveEnv_OrderIsDeterministicNonDecreasingByCanonicalKey(t *testing.T) {
	// We can't control all of os.Environ(), but we can assert monotonic ordering.
	t.Setenv("ZZZ_TEST_ENV", "1")
	t.Setenv("AAA_TEST_ENV", "2")
	id := newSessionID()
	s := &ShellSession{
		id:      id,
		workdir: "",
		env:     map[string]string{},
	}

	env, err := s.GetEffectiveEnv(map[string]string{"MMM_TEST_ENV": "3"})
	if err != nil {
		t.Fatalf("effectiveEnv error: %v", err)
	}
	var prev string
	for i, kv := range env {
		k, _, ok := strings.Cut(kv, "=")
		if !ok {
			t.Fatalf("bad env entry %q", kv)
		}
		ck := canonicalEnvKey(k)
		if i > 0 && ck < prev {
			t.Fatalf("env not sorted at %d: %q < %q", i, ck, prev)
		}
		prev = ck
	}
}

func TestCappedWriter_TruncatesAndCounts(t *testing.T) {
	w := newCappedWriter(1024)

	_, _ = w.Write([]byte(strings.Repeat("a", 600)))
	_, _ = w.Write([]byte(strings.Repeat("b", 600))) // total 1200 > 1024

	if w.TotalBytes() != 1200 {
		t.Fatalf("expected totalBytes 1200, got %d", w.TotalBytes())
	}
	if !w.Truncated() {
		t.Fatalf("expected truncated=true")
	}
	if got := len(w.Bytes()); got != 1024 {
		t.Fatalf("expected stored bytes len 1024, got %d", got)
	}
	b := w.Bytes()
	if len(b) == 0 || b[len(b)-1] != 'b' {
		t.Fatalf("expected tail capture ending with 'b'")
	}
}

func TestSafeUTF8_ReplacesInvalid(t *testing.T) {
	s := safeUTF8([]byte{0xff, 0xfe, 'a'})
	if !strings.Contains(s, "\uFFFD") {
		t.Fatalf("expected replacement char in %q", s)
	}
	if !strings.Contains(s, "a") {
		t.Fatalf("expected 'a' preserved in %q", s)
	}
}

func TestShellSession_AddToEnv_ClosedSessionErrors(t *testing.T) {
	cases := []struct {
		name          string
		closeFirst    bool
		add           map[string]string
		wantErrSubstr string
	}{
		{name: "no_additions_ok", closeFirst: false, add: nil},
		{name: "closed_errors", closeFirst: true, add: map[string]string{"A": "1"}, wantErrSubstr: "closed"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := &ShellSession{id: "x", env: map[string]string{}}
			if tc.closeFirst {
				s.mu.Lock()
				s.closed = true
				s.mu.Unlock()
			}
			err := s.AddToEnv(tc.add)
			if tc.wantErrSubstr != "" {
				if err == nil {
					t.Fatalf("expected error")
				}
				if !strings.Contains(strings.ToLower(err.Error()), strings.ToLower(tc.wantErrSubstr)) {
					t.Fatalf("error %q does not contain %q", err.Error(), tc.wantErrSubstr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestShellSession_GetEffectiveWorkdir_FallbackOrder(t *testing.T) {
	td := t.TempDir()
	td2 := t.TempDir()

	s := &ShellSession{id: "x", env: map[string]string{}}
	s.SetWorkDir(td)

	cases := []struct {
		name          string
		input         string
		def           string
		roots         []string
		wantSameAs    string
		wantErrSubstr string
	}{
		{name: "input_overrides_session", input: td2, def: "", roots: nil, wantSameAs: td2},
		{name: "session_used_when_no_input", input: "", def: "", roots: nil, wantSameAs: td},
		{
			name:          "default_used_when_no_input_no_session",
			input:         "",
			def:           td2,
			roots:         nil,
			wantSameAs:    td2,
			wantErrSubstr: "",
		},
	}

	// Need a fresh session for "no session" scenario.
	sNoWD := &ShellSession{id: "y", env: map[string]string{}}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			use := s
			if tc.name == "default_used_when_no_input_no_session" {
				use = sNoWD
			}

			got, err := use.GetEffectiveWorkdir(tc.input, tc.def)
			if tc.wantErrSubstr != "" {
				if err == nil {
					t.Fatalf("expected error")
				}
				if !strings.Contains(strings.ToLower(err.Error()), strings.ToLower(tc.wantErrSubstr)) {
					t.Fatalf("error %q does not contain %q", err.Error(), tc.wantErrSubstr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			mustSameDir(t, tc.wantSameAs, got)
		})
	}
}

func TestEffectiveEnv_NilReceiver_WorksAndAppliesOverrides(t *testing.T) {
	t.Setenv("EFFECTIVE_ENV_TEST_A", "from_process")

	cases := []struct {
		name      string
		overrides map[string]string
		wantKey   string
		wantVal   string
	}{
		{
			name:      "override_applied",
			overrides: map[string]string{"EFFECTIVE_ENV_TEST_A": "override"},
			wantKey:   "EFFECTIVE_ENV_TEST_A",
			wantVal:   "override",
		},
		{
			name:      "new_key_added",
			overrides: map[string]string{"EFFECTIVE_ENV_TEST_B": "b"},
			wantKey:   "EFFECTIVE_ENV_TEST_B",
			wantVal:   "b",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env, err := EffectiveEnv(tc.overrides)
			if err != nil {
				t.Fatalf("EffectiveEnv: %v", err)
			}
			m := make(map[string]string, len(env))
			for _, kv := range env {
				k, v, ok := strings.Cut(kv, "=")
				if ok {
					m[k] = v
				}
			}
			if m[tc.wantKey] != tc.wantVal {
				t.Fatalf("env[%q] got %q want %q", tc.wantKey, m[tc.wantKey], tc.wantVal)
			}
		})
	}
}

func TestShellSession_GetEffectiveWorkdir_RejectsInvalidSession(t *testing.T) {
	td := t.TempDir()

	cases := []struct {
		name          string
		sess          *ShellSession
		input         string
		def           string
		wantErrSubstr string
	}{
		{name: "nil_session_errors", sess: nil, input: td, def: "", wantErrSubstr: "invalid session"},
		{name: "closed_session_errors", sess: &ShellSession{id: "z"}, input: td, def: "", wantErrSubstr: "closed"},
	}

	closed := &ShellSession{id: "z", env: map[string]string{}}
	closed.mu.Lock()
	closed.closed = true
	closed.mu.Unlock()
	cases[1].sess = closed

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := tc.sess.GetEffectiveWorkdir(tc.input, tc.def)
			if err == nil {
				t.Fatalf("expected error")
			}
			if !strings.Contains(strings.ToLower(err.Error()), strings.ToLower(tc.wantErrSubstr)) {
				t.Fatalf("error %q does not contain %q", err.Error(), tc.wantErrSubstr)
			}
		})
	}
}

func TestShellSession_GetEffectiveWorkdir_DefaultsToCWDWhenAllEmpty(t *testing.T) {
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	s := &ShellSession{id: "x", env: map[string]string{}}

	cases := []struct {
		name string
	}{
		{name: "falls_back_to_cwd"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := s.GetEffectiveWorkdir("", "")
			if err != nil {
				t.Fatalf("GetEffectiveWorkdir: %v", err)
			}
			// SameFile works even if got is absolute/canonicalized.
			mustSameDir(t, filepath.Clean(cwd), got)
		})
	}
}

func mustSameDir(t *testing.T, a, b string) {
	t.Helper()
	sa, err := os.Stat(a)
	if err != nil {
		t.Fatalf("stat(%q): %v", a, err)
	}
	sb, err := os.Stat(b)
	if err != nil {
		t.Fatalf("stat(%q): %v", b, err)
	}
	if !os.SameFile(sa, sb) {
		t.Fatalf("expected same dir:\n  a=%q\n  b=%q", a, b)
	}
}
