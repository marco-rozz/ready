package gittool

import (
	"context"
	"errors"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/flexigpt/llmtools-go/internal/toolutil"
	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
)

const (
	testAuthorName           = "Test Author"
	testAuthorEmail          = "author@example.invalid"
	testCommitMessage        = "test commit"
	testRepoDirName          = "repo"
	testBareRepoDirName      = "bare-repo"
	testNestedRepoDirName    = "nested/repo"
	testFileName             = "file.txt"
	testFileContent          = "alpha\nbeta\n"
	testBinaryFileName       = "bin.dat"
	testBinaryContent        = "\xff\xfe\x00"
	testOriginRemoteName     = "origin"
	testFeatureBranchName    = "feature"
	testSecondFileName       = "new.txt"
	testSecondFileContents   = "new content\n"
	testModifiedFileContents = "alpha\nbeta modified\n"
)

func TestMustInitRepoAndCommit(t *testing.T) {
	root := t.TempDir()
	repo := mustInitRepo(t, root, false)

	mustWriteFile(t, root, "nested/subdir/"+testFileName, testFileContent)
	hash := mustCommitAll(t, repo, testCommitMessage)

	opened, err := git.PlainOpen(root)
	if err != nil {
		t.Fatalf("PlainOpen(%q) error = %v", root, err)
	}
	commit, err := opened.CommitObject(hash)
	if err != nil {
		t.Fatalf("CommitObject(%s) error = %v", hash, err)
	}

	info := commitInfo(commit)
	if info.Subject != testCommitMessage {
		t.Fatalf("commit subject = %q, want %q", info.Subject, testCommitMessage)
	}
	if info.AuthorName != testAuthorName {
		t.Fatalf("commit author name = %q, want %q", info.AuthorName, testAuthorName)
	}
	if info.AuthorEmail != testAuthorEmail {
		t.Fatalf("commit author email = %q, want %q", info.AuthorEmail, testAuthorEmail)
	}
}

func TestNormalizeRepoRelativePath(t *testing.T) {
	absPath := filepath.Join(t.TempDir(), "absolute.txt")

	tests := []struct {
		name    string
		input   string
		want    string
		wantErr bool
	}{
		{name: "dot", input: ".", want: "."},
		{name: "nested", input: "dir/./file.txt", want: "dir/file.txt"},
		{name: "windows separators", input: "dir\\file.txt", want: "dir/file.txt"},
		{name: "collapse parent", input: "dir/sub/../file.txt", want: "dir/file.txt"},
		{name: "absolute", input: absPath, wantErr: true},
		{name: "escape", input: "../escape", wantErr: true},
		{name: "drive letter", input: "C:repo", wantErr: true},
		{name: "empty", input: "", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := normalizeRepoRelativePath(tt.input)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("normalizeRepoRelativePath(%q) error = nil, want error", tt.input)
				}
				return
			}
			if err != nil {
				t.Fatalf("normalizeRepoRelativePath(%q) error = %v", tt.input, err)
			}
			if got != tt.want {
				t.Fatalf("normalizeRepoRelativePath(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestNormalizeRepoRelativePaths(t *testing.T) {
	got, err := normalizeRepoRelativePaths([]string{"b", "a/./c"}, true)
	if err != nil {
		t.Fatalf("normalizeRepoRelativePaths() error = %v", err)
	}
	want := []string{"a/c", "b"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("normalizeRepoRelativePaths() = %#v, want %#v", got, want)
	}

	got, err = normalizeRepoRelativePaths(nil, true)
	if err != nil {
		t.Fatalf("normalizeRepoRelativePaths(nil, true) error = %v", err)
	}
	if got != nil {
		t.Fatalf("normalizeRepoRelativePaths(nil, true) = %#v, want nil", got)
	}

	got, err = normalizeRepoRelativePaths(nil, false)
	if err != nil {
		t.Fatalf("normalizeRepoRelativePaths(nil, false) error = %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("normalizeRepoRelativePaths(nil, false) = %#v, want empty slice", got)
	}

	tooMany := make([]string, hardStagePaths+1)
	for i := range tooMany {
		tooMany[i] = "a"
	}
	if _, err := normalizeRepoRelativePaths(tooMany, true); err == nil {
		t.Fatal("normalizeRepoRelativePaths(tooMany, true) error = nil, want error")
	}
}

func TestValidateIdentifiers(t *testing.T) {
	tooLongRef := strings.Repeat("a", hardRefNameLength+1)
	tooLongRevision := strings.Repeat("b", hardRevisionLength+1)

	tests := []struct {
		name    string
		fn      func(string) error
		value   string
		wantErr bool
	}{
		{name: "branch valid", fn: validateLocalBranchName, value: "feature/test"},
		{name: "branch invalid ref", fn: validateLocalBranchName, value: "refs/heads/main", wantErr: true},
		{name: "branch invalid dotlock", fn: validateLocalBranchName, value: "topic.lock", wantErr: true},
		{name: "branch invalid too long", fn: validateLocalBranchName, value: tooLongRef, wantErr: true},
		{name: "tag valid", fn: validateTagName, value: "v1.2.3"},
		{name: "tag invalid space", fn: validateTagName, value: "bad tag", wantErr: true},
		{name: "tag invalid too long", fn: validateTagName, value: tooLongRef, wantErr: true},
		{name: "revision valid", fn: validateRevision, value: "HEAD"},
		{name: "revision invalid prefix", fn: validateRevision, value: "-bad", wantErr: true},
		{name: "revision invalid too long", fn: validateRevision, value: tooLongRevision, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.fn(tt.value)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("%s(%q) error = nil, want error", tt.name, tt.value)
				}
				return
			}
			if err != nil {
				t.Fatalf("%s(%q) error = %v", tt.name, tt.value, err)
			}
		})
	}
}

func TestParseTimeAndSplitCommitMessage(t *testing.T) {
	got, err := parseTime("since", "2024-01-02T03:04:05Z")
	if err != nil {
		t.Fatalf("parseTime() error = %v", err)
	}
	want := time.Date(2024, time.January, 2, 3, 4, 5, 0, time.UTC)
	if !got.Equal(want) {
		t.Fatalf("parseTime() = %v, want %v", got, want)
	}

	got, err = parseTime("since", "2024-01-02")
	if err != nil {
		t.Fatalf("parseTime(date) error = %v", err)
	}
	want = time.Date(2024, time.January, 2, 0, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Fatalf("parseTime(date) = %v, want %v", got, want)
	}

	if _, err := parseTime("since", "-2024-01-02"); err == nil {
		t.Fatal("parseTime(negative) error = nil, want error")
	}

	subject, body := splitCommitMessage("subject\r\nbody text\r\n")
	if subject != "subject" || body != "body text" {
		t.Fatalf("splitCommitMessage() = (%q, %q), want (%q, %q)", subject, body, "subject", "body text")
	}

	subject, body = splitCommitMessage("")
	if subject != "" || body != "" {
		t.Fatalf("splitCommitMessage(empty) = (%q, %q), want empty strings", subject, body)
	}
}

func TestLimitStringBytesReadLimitedAndBinaryDetection(t *testing.T) {
	data, truncated, err := readLimited(strings.NewReader("abcdef"), 3)
	if err != nil {
		t.Fatalf("readLimited() error = %v", err)
	}
	if string(data) != "abc" || !truncated {
		t.Fatalf("readLimited() = (%q, %v), want (%q, %v)", string(data), truncated, "abc", true)
	}

	limited, truncated := limitStringBytes("αβγ", 3)
	if !truncated {
		t.Fatal("limitStringBytes() truncated = false, want true")
	}
	if !utf8.ValidString(limited) {
		t.Fatalf("limitStringBytes() returned invalid UTF-8: %q", limited)
	}
	if limited != "α\n[truncated]" {
		t.Fatalf("limitStringBytes() = %q, want %q", limited, "α\n[truncated]")
	}

	if !reflect.DeepEqual(blameSplitLines("one\ntwo\n"), []string{"one", "two"}) {
		t.Fatalf("blameSplitLines() did not split trailing newline correctly")
	}
	if !reflect.DeepEqual(grepSplitLines("one\n\ntwo"), []string{"one", "", "two"}) {
		t.Fatalf("grepSplitLines() did not preserve empty line")
	}

	if isBinaryData([]byte("text")) {
		t.Fatal("isBinaryData(text) = true, want false")
	}
	if !isBinaryData([]byte(testBinaryContent)) {
		t.Fatal("isBinaryData(binary) = false, want true")
	}
	if isBinaryData(nil) {
		t.Fatal("isBinaryData(nil) = true, want false")
	}
}

func assertSamePath(t *testing.T, got, want string) {
	t.Helper()
	gotNorm := normalizePathForCompare(canonForPolicyExpectations(got))
	wantNorm := normalizePathForCompare(canonForPolicyExpectations(want))
	if gotNorm != wantNorm {
		t.Fatalf("path = %q, want %q", got, want)
	}
}

func normalizePathForCompare(pathValue string) string {
	pathValue = strings.TrimSpace(pathValue)
	if pathValue == "" {
		return ""
	}

	pathValue = strings.ReplaceAll(pathValue, "\\", "/")
	preserveUNC := runtime.GOOS == toolutil.GOOSWindows && strings.HasPrefix(pathValue, "//")
	if preserveUNC {
		pathValue = "//" + collapseRepeatedSlashes(strings.TrimLeft(pathValue[2:], "/"))
	} else {
		pathValue = collapseRepeatedSlashes(pathValue)
	}

	pathValue = path.Clean(pathValue)
	if preserveUNC && strings.HasPrefix(pathValue, "/") && !strings.HasPrefix(pathValue, "//") {
		pathValue = "/" + pathValue
	}
	if pathValue == "." {
		pathValue = ""
	}

	if runtime.GOOS == toolutil.GOOSWindows {
		pathValue = strings.ToLower(pathValue)
	}

	return pathValue
}

func newTestGitTool(t *testing.T, base string) *GitTool {
	t.Helper()

	tool, err := NewGitTool(
		WithWorkBaseDir(base),
		WithAllowedRoots([]string{base}),
	)
	if err != nil {
		t.Fatalf("NewGitTool() error = %v", err)
	}
	return tool
}

func canonForPolicyExpectations(p string) string {
	p = filepath.Clean(p)
	p = evalTestSymlinksBestEffort(p)
	if runtime.GOOS != toolutil.GOOSDarwin {
		return p
	}

	aliases := map[string]string{
		"/var":  "/private/var",
		"/tmp":  "/private/tmp",
		"/etc":  "/private/etc",
		"/bin":  "/usr/bin",
		"/sbin": "/usr/sbin",
		"/lib":  "/usr/lib",
	}
	sep := string(os.PathSeparator)
	for from, to := range aliases {
		if p == from {
			return to
		}
		if strings.HasPrefix(p, from+sep) {
			return to + p[len(from):]
		}
	}
	return p
}

func evalTestSymlinksBestEffort(p string) string {
	p = filepath.Clean(p)
	tried := p
	remainder := ""

	for range 64 {
		if resolved, err := filepath.EvalSymlinks(tried); err == nil && resolved != "" {
			resolved = filepath.Clean(resolved)
			if remainder == "" {
				return resolved
			}
			return filepath.Join(resolved, remainder)
		}

		parent := filepath.Dir(tried)
		if parent == tried {
			return p
		}

		base := filepath.Base(tried)
		if remainder == "" {
			remainder = base
		} else {
			remainder = filepath.Join(base, remainder)
		}
		tried = parent
	}
	return p
}

func mustWriteFile(t *testing.T, root, rel, contents string) {
	t.Helper()

	full := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
		t.Fatalf("MkdirAll(%q) error = %v", filepath.Dir(full), err)
	}
	if err := os.WriteFile(full, []byte(contents), 0o600); err != nil {
		t.Fatalf("WriteFile(%q) error = %v", full, err)
	}
}

func mustWriteBinaryFile(t *testing.T, root, rel string, contents []byte) {
	t.Helper()

	full := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
		t.Fatalf("MkdirAll(%q) error = %v", filepath.Dir(full), err)
	}
	if err := os.WriteFile(full, contents, 0o600); err != nil {
		t.Fatalf("WriteFile(%q) error = %v", full, err)
	}
}

func mustInitRepo(t *testing.T, pathIn string, bare bool) *git.Repository {
	t.Helper()

	repo, err := git.PlainInitWithOptions(pathIn, &git.PlainInitOptions{Bare: bare})
	if err != nil {
		t.Fatalf("PlainInitWithOptions(%q) error = %v", pathIn, err)
	}
	return repo
}

func mustCommitAll(t *testing.T, repo *git.Repository, message string) plumbing.Hash {
	t.Helper()
	return mustCommitAllAt(t, repo, message, time.Date(2024, time.January, 2, 3, 4, 5, 0, time.UTC))
}

func mustCommitAllAt(t *testing.T, repo *git.Repository, message string, when time.Time) plumbing.Hash {
	t.Helper()

	wt, err := repo.Worktree()
	if err != nil {
		t.Fatalf("Worktree() error = %v", err)
	}
	if _, err := wt.Add("."); err != nil {
		t.Fatalf("Add(\".\") error = %v", err)
	}

	hash, err := wt.Commit(message, &git.CommitOptions{
		Author: &object.Signature{
			Name:  testAuthorName,
			Email: testAuthorEmail,
			When:  when,
		},
		Committer: &object.Signature{
			Name:  testAuthorName,
			Email: testAuthorEmail,
			When:  when,
		},
	})
	if err != nil {
		t.Fatalf("Commit(%q) error = %v", message, err)
	}
	return hash
}

func mustSetRepoUserConfig(t *testing.T, repo *git.Repository, name, email string) {
	t.Helper()

	cfg, err := repo.Config()
	if err != nil {
		t.Fatalf("Config() error = %v", err)
	}
	cfg.User.Name = name
	cfg.User.Email = email
	if err := repo.Storer.SetConfig(cfg); err != nil {
		t.Fatalf("SetConfig() error = %v", err)
	}
}

func mustCreateRemote(t *testing.T, repo *git.Repository, name, url string) *git.Remote {
	t.Helper()

	remote, err := repo.CreateRemote(&config.RemoteConfig{
		Name: name,
		URLs: []string{url},
	})
	if err != nil {
		t.Fatalf("CreateRemote(%q, %q) error = %v", name, url, err)
	}
	return remote
}

func mustPushRefs(t *testing.T, ctx context.Context, remote *git.Remote, refSpecs []config.RefSpec) {
	t.Helper()

	if remote == nil {
		t.Fatal("mustPushRefs() remote is nil")
	}
	if err := remote.PushContext(ctx, &git.PushOptions{RefSpecs: refSpecs}); err != nil {
		t.Fatalf("PushContext() error = %v", err)
	}
}

func mustFetchRefs(t *testing.T, ctx context.Context, remote *git.Remote, refSpecs []config.RefSpec) {
	t.Helper()

	if remote == nil {
		t.Fatal("mustFetchRefs() remote is nil")
	}
	if err := remote.FetchContext(
		ctx,
		&git.FetchOptions{RefSpecs: refSpecs},
	); err != nil &&
		!errors.Is(err, git.NoErrAlreadyUpToDate) {
		t.Fatalf("FetchContext() error = %v", err)
	}
}

func mustCheckoutBranch(t *testing.T, repo *git.Repository, name string, create bool) {
	t.Helper()

	wt, err := repo.Worktree()
	if err != nil {
		t.Fatalf("Worktree() error = %v", err)
	}
	if err := wt.Checkout(&git.CheckoutOptions{
		Branch: plumbing.NewBranchReferenceName(name),
		Create: create,
	}); err != nil {
		t.Fatalf("Checkout(branch=%q, create=%v) error = %v", name, create, err)
	}
}

func collapseRepeatedSlashes(s string) string {
	for strings.Contains(s, "//") {
		s = strings.ReplaceAll(s, "//", "/")
	}
	return s
}

func sortStringsCopy(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}
