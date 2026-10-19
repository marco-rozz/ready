package gittool

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/flexigpt/llmtools-go/internal/fspolicy"
	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/format/index"
	"github.com/go-git/go-git/v5/plumbing/object"
)

var errStopIteration = errors.New("stop git iteration")

type OutputMeta struct {
	Bytes              int  `json:"bytes"`
	Truncated          bool `json:"truncated"`
	OmittedBinaryFiles int  `json:"omittedBinaryFiles"`
	SkippedLargeFiles  int  `json:"skippedLargeFiles"`
	MaxBytes           int  `json:"maxBytes"`
}

type gitToolSnapshot struct {
	policy             fspolicy.FSPolicy
	blockSymlinks      bool
	defaultAuthorName  string
	defaultAuthorEmail string
}

func (gt *GitTool) snapshot() gitToolSnapshot {
	gt.mu.RLock()
	defer gt.mu.RUnlock()
	return gitToolSnapshot{
		policy:             gt.policy,
		blockSymlinks:      gt.cfg.blockSymlinks,
		defaultAuthorName:  gt.cfg.defaultAuthorName,
		defaultAuthorEmail: gt.cfg.defaultAuthorEmail,
	}
}

func openWorktree(
	ctx context.Context,
	snap gitToolSnapshot,
	repoPath string,
) (*git.Repository, *git.Worktree, string, error) {
	repo, abs, err := openRepository(ctx, snap, repoPath)
	if err != nil {
		return nil, nil, "", err
	}
	wt, err := repo.Worktree()
	if err != nil {
		return nil, nil, "", fmt.Errorf("open git worktree %q: %w", abs, err)
	}
	return repo, wt, abs, nil
}

func openRepository(ctx context.Context, snap gitToolSnapshot, repoPath string) (*git.Repository, string, error) {
	if err := ctx.Err(); err != nil {
		return nil, "", err
	}

	if strings.TrimSpace(repoPath) == "" {
		return nil, "", errors.New("repoPath is required")
	}

	abs, err := snap.policy.ResolvePath(repoPath, "")
	if err != nil {
		return nil, "", err
	}

	st, err := os.Stat(abs)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, "", fmt.Errorf("repository path does not exist: %s", abs)
		}
		return nil, "", err
	}
	if !st.IsDir() {
		return nil, "", fmt.Errorf("repository path is not a directory: %s", abs)
	}

	repo, err := git.PlainOpen(abs)
	if err != nil {
		return nil, "", fmt.Errorf("open git repository %q: %w", abs, err)
	}

	return repo, abs, nil
}

func resolveCommit(repo *git.Repository, revision string) (*object.Commit, error) {
	rev := strings.TrimSpace(revision)
	if rev == "" {
		rev = revisionHead
	}
	if err := validateRevision(rev); err != nil {
		return nil, err
	}

	hash, err := repo.ResolveRevision(plumbing.Revision(rev))
	if err != nil {
		return nil, fmt.Errorf("resolve revision %q: %w", rev, err)
	}

	commit, err := repo.CommitObject(*hash)
	if err == nil {
		return commit, nil
	}

	tag, tagErr := repo.TagObject(*hash)
	if tagErr == nil {
		commit, commitErr := tag.Commit()
		if commitErr == nil {
			return commit, nil
		}
		return nil, fmt.Errorf("resolve annotated tag %q to commit: %w", rev, commitErr)
	}

	return nil, fmt.Errorf("load commit %q: %w", rev, err)
}

func validateRevision(rev string) error {
	if strings.TrimSpace(rev) == "" {
		return errors.New("revision is required")
	}
	if len(rev) > hardRevisionLength {
		return fmt.Errorf("revision too long: max %d bytes", hardRevisionLength)
	}
	if strings.HasPrefix(rev, "-") {
		return fmt.Errorf("revision %q is invalid: must not start with '-'", rev)
	}
	if strings.ContainsRune(rev, '\x00') {
		return errors.New("revision contains NUL byte")
	}
	for _, r := range rev {
		if unicode.IsControl(r) {
			return fmt.Errorf("revision contains control character %U", r)
		}
	}
	return nil
}

func validateLocalBranchName(name string) error {
	n := strings.TrimSpace(name)
	if n == "" {
		return errors.New("branch name is required")
	}
	if len(n) > hardRefNameLength {
		return fmt.Errorf("branch name too long: max %d bytes", hardRefNameLength)
	}
	if n == "@" {
		return errors.New(`branch name "@" is invalid`)
	}
	if strings.HasPrefix(n, "refs/") {
		return fmt.Errorf("branch name %q is invalid: pass a plain branch name, not a full ref", n)
	}
	if strings.HasPrefix(n, "-") {
		return fmt.Errorf("branch name %q is invalid: must not start with '-'", n)
	}
	if strings.ContainsRune(n, '\x00') {
		return errors.New("branch name contains NUL byte")
	}
	for _, r := range n {
		if unicode.IsControl(r) {
			return fmt.Errorf("branch name contains control character %U", r)
		}
	}

	if strings.HasPrefix(n, "/") ||
		strings.HasSuffix(n, "/") ||
		strings.Contains(n, "//") ||
		strings.Contains(n, "..") ||
		strings.Contains(n, "@{") ||
		strings.HasSuffix(n, ".") ||
		strings.HasSuffix(n, ".lock") ||
		strings.ContainsAny(n, ` ~^:?*[\\`) {
		return fmt.Errorf("invalid branch name: %q", n)
	}

	return nil
}

func validateTagName(name string) error {
	n := strings.TrimSpace(name)
	if n == "" {
		return errors.New("tag name is required")
	}
	if len(n) > hardRefNameLength {
		return fmt.Errorf("tag name too long: max %d bytes", hardRefNameLength)
	}
	if n == "@" {
		return errors.New(`tag name "@" is invalid`)
	}
	if strings.HasPrefix(n, "-") {
		return fmt.Errorf("tag name %q is invalid: must not start with '-'", n)
	}
	if strings.HasPrefix(n, "refs/") {
		return fmt.Errorf("tag name %q is invalid: pass a plain tag name, not a full ref", n)
	}
	if strings.ContainsRune(n, '\x00') {
		return errors.New("tag name contains NUL byte")
	}
	for _, r := range n {
		if unicode.IsControl(r) {
			return fmt.Errorf("tag name contains control character %U", r)
		}
	}

	if strings.HasPrefix(n, "/") ||
		strings.HasSuffix(n, "/") ||
		strings.Contains(n, "//") ||
		strings.Contains(n, "..") ||
		strings.Contains(n, "@{") ||
		strings.HasSuffix(n, ".") ||
		strings.HasSuffix(n, ".lock") ||
		strings.ContainsAny(n, ` ~^:?*[\\`) {
		return fmt.Errorf("invalid tag name: %q", n)
	}
	return nil
}

func validateTagPattern(pattern string) error {
	if len(pattern) > hardRefNameLength {
		return fmt.Errorf("pattern too long: max %d bytes", hardRefNameLength)
	}
	if strings.ContainsRune(pattern, '\x00') {
		return errors.New("pattern contains NUL byte")
	}
	for _, r := range pattern {
		if unicode.IsControl(r) {
			return fmt.Errorf("pattern contains control character %U", r)
		}
	}
	return nil
}

func normalizeRepoRelativePaths(paths []string, allowEmpty bool) ([]string, error) {
	if len(paths) == 0 && allowEmpty {
		return nil, nil
	}
	if len(paths) > hardStagePaths {
		return nil, fmt.Errorf("too many paths: max %d", hardStagePaths)
	}
	out := make([]string, 0, len(paths))
	for _, p := range paths {
		cleaned, err := normalizeRepoRelativePath(p)
		if err != nil {
			return nil, err
		}
		out = append(out, cleaned)
	}
	return sortedStrings(out), nil
}

func normalizePositiveInt(value, def, minNum, maxNum int) int {
	if value == 0 {
		value = def
	}
	if value < minNum {
		value = minNum
	}
	if value > maxNum {
		value = maxNum
	}
	return value
}

func normalizeOptionalInt(value *int, def, minNum, maxNum int) int {
	if value == nil {
		return def
	}
	v := *value
	if v < minNum {
		return minNum
	}
	if v > maxNum {
		return maxNum
	}
	return v
}

func limitStringBytes(s string, maxBytes int) (string, bool) {
	if maxBytes <= 0 {
		maxBytes = defaultDiffMaxBytes
	}
	if maxBytes > hardDiffMaxBytes {
		maxBytes = hardDiffMaxBytes
	}
	if len(s) <= maxBytes {
		return s, false
	}

	cut := s[:maxBytes]
	for cut != "" && !utf8.ValidString(cut) {
		cut = cut[:len(cut)-1]
	}
	return cut + "\n[truncated]", true
}

func headInfo(repo *git.Repository) (branch, hash string, detached bool) {
	branch, hash, detached, _ = headInfoDetailed(repo)
	return branch, hash, detached
}

func headInfoDetailed(repo *git.Repository) (branch, hash string, detached, unborn bool) {
	ref, err := repo.Head()
	if err != nil {
		return "", "", false, true
	}
	hash = ref.Hash().String()
	if ref.Name().IsBranch() {
		return ref.Name().Short(), hash, false, false
	}
	return "", hash, true, false
}

func ensureNoIndexConflicts(repo *git.Repository, wt *git.Worktree) error {
	state, err := detectIndexState(repo, wt)
	if err != nil {
		return err
	}
	if state.HasConflicts {
		return fmt.Errorf("repository has unresolved index conflicts: %s", strings.Join(state.ConflictPaths, ", "))
	}
	return nil
}

type IndexState struct {
	Entries       int      `json:"entries"`
	HasConflicts  bool     `json:"hasConflicts"`
	ConflictPaths []string `json:"conflictPaths,omitempty"`
}

func detectIndexState(repo *git.Repository, wt *git.Worktree) (IndexState, error) {
	state := IndexState{}
	if repo != nil {
		if idx, err := repo.Storer.Index(); err == nil && idx != nil {
			state.Entries = len(idx.Entries)
		}
	}
	if wt == nil {
		return state, nil
	}

	st, err := wt.Status()
	if err != nil {
		return state, err
	}

	seen := make(map[string]bool)
	for p, fs := range st {
		if statusCodeIsConflict(fs.Staging) || statusCodeIsConflict(fs.Worktree) {
			if !seen[p] {
				state.ConflictPaths = append(state.ConflictPaths, p)
				seen[p] = true
			}
		}
	}
	sort.Strings(state.ConflictPaths)
	state.HasConflicts = len(state.ConflictPaths) > 0
	return state, nil
}

func statusCodeIsConflict(code git.StatusCode) bool {
	return string(code) == "U"
}

func statusCodeName(code git.StatusCode) string {
	switch string(code) {
	case " ":
		return "unmodified"
	case "?":
		return "untracked"
	case "M":
		return "modified"
	case "A":
		return "added"
	case "D":
		return "deleted"
	case "R":
		return "renamed"
	case "C":
		return "copied"
	case "U":
		return "unmerged"
	default:
		return string(code)
	}
}

type CommitInfo struct {
	Hash        string    `json:"hash"`
	ShortHash   string    `json:"shortHash"`
	AuthorName  string    `json:"authorName"`
	AuthorEmail string    `json:"authorEmail"`
	AuthorWhen  time.Time `json:"authorWhen"`
	Subject     string    `json:"subject"`
	Body        string    `json:"body,omitempty"`
}

func commitInfo(c *object.Commit) CommitInfo {
	if c == nil {
		return CommitInfo{}
	}
	subject, body := splitCommitMessage(c.Message)
	hash := c.Hash.String()
	return CommitInfo{
		Hash:        hash,
		ShortHash:   shortHash(hash),
		AuthorName:  c.Author.Name,
		AuthorEmail: c.Author.Email,
		AuthorWhen:  c.Author.When,
		Subject:     subject,
		Body:        body,
	}
}

func shortHash(hash string) string {
	short := hash
	if len(short) > 12 {
		short = short[:12]
	}
	return short
}

func splitCommitMessage(msg string) (subject, line string) {
	m := strings.TrimRight(msg, "\r\n")
	if m == "" {
		return "", ""
	}
	lines := strings.SplitN(m, "\n", 2)
	subject = strings.TrimSpace(lines[0])
	if len(lines) == 1 {
		return subject, ""
	}
	return subject, strings.TrimSpace(lines[1])
}

func parseTime(name, value string) (*time.Time, error) {
	v := strings.TrimSpace(value)
	if v == "" {
		return nil, errors.New("got empty time")
	}
	if strings.HasPrefix(v, "-") {
		return nil, fmt.Errorf("%s must not start with '-'", name)
	}

	formats := []string{
		time.RFC3339,
		"2006-01-02T15:04:05",
		"2006-01-02",
	}
	for _, format := range formats {
		t, err := time.Parse(format, v)
		if err == nil {
			return &t, nil
		}
	}
	return nil, fmt.Errorf("%s must be RFC3339, YYYY-MM-DDTHH:MM:SS, or YYYY-MM-DD", name)
}

func sortedStrings(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}

func statusDiffPaths(st git.Status, kind DiffKind, filters []string) []string {
	paths := make([]string, 0, len(st))
	for p, fs := range st {
		if !matchesRepoRelativePathFilter(p, filters) {
			continue
		}
		switch kind {
		case DiffKindWorking:
			if fs.Worktree != git.Unmodified {
				paths = append(paths, p)
			}
		case DiffKindStaged:
			if fs.Staging != git.Unmodified && fs.Staging != git.Untracked {
				paths = append(paths, p)
			}
		default:
		}
	}
	sort.Strings(paths)
	return paths
}

func filterObjectChangesByPaths(changes object.Changes, filters []string) object.Changes {
	if len(filters) == 0 {
		return changes
	}
	filtered := make(object.Changes, 0, len(changes))
	for _, change := range changes {
		from, to, _ := objectChangePaths(change)
		if matchesRepoRelativePathFilter(from, filters) || matchesRepoRelativePathFilter(to, filters) {
			filtered = append(filtered, change)
		}
	}
	return filtered
}

func matchesRepoRelativePathFilter(p string, filters []string) bool {
	if len(filters) == 0 {
		return true
	}
	for _, f := range filters {
		if f == "." || p == f || strings.HasPrefix(p, f+"/") {
			return true
		}
	}
	return false
}

type treePathIdentity struct {
	Exists bool
	Hash   string
	Mode   string
	Size   int64
}

func indexEntriesByPath(repo *git.Repository) (map[string]index.Entry, error) {
	idx, err := repo.Storer.Index()
	if err != nil {
		return nil, err
	}
	out := make(map[string]index.Entry, len(idx.Entries))
	for i := range idx.Entries {
		e := idx.Entries[i]
		out[e.Name] = *e
	}
	return out, nil
}

func indexPathContentLimited(
	repo *git.Repository,
	entries map[string]index.Entry,
	p string,
	maxBytes int64,
) (data []byte, exists, truncated bool, err error) {
	entry, ok := entries[p]
	if !ok {
		return nil, false, false, nil
	}
	data, truncated, err = blobContent(repo, entry.Hash, maxBytes)
	return data, true, truncated, err
}

func blobContent(repo *git.Repository, hash plumbing.Hash, maxBytes int64) (data []byte, ok bool, err error) {
	blob, err := repo.BlobObject(hash)
	if err != nil {
		return nil, false, err
	}
	r, err := blob.Reader()
	if err != nil {
		return nil, false, err
	}
	defer r.Close()
	return readLimited(r, maxBytes)
}

func treePathContent(tree *object.Tree, p string, maxBytes int64) (data []byte, ok bool, err error) {
	data, ok, _, err = treePathContentLimited(tree, p, maxBytes)
	return data, ok, err
}

func treePathBlobIdentity(tree *object.Tree, p string) treePathIdentity {
	if tree == nil {
		return treePathIdentity{}
	}
	f, err := tree.File(p)
	if err != nil {
		return treePathIdentity{}
	}
	return treePathIdentity{
		Exists: true,
		Hash:   f.Hash.String(),
		Mode:   f.Mode.String(),
		Size:   f.Size,
	}
}

func worktreePathContentLimited(
	wt *git.Worktree,
	p string,
	maxBytes int64,
) (data []byte, ok, truncated bool, err error) {
	f, err := wt.Filesystem.Open(p)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, false, false, nil
		}
		return nil, false, false, err
	}
	defer f.Close()
	data, truncated, err = readLimited(f, maxBytes)
	if err != nil {
		return nil, false, false, err
	}
	return data, true, truncated, nil
}

func validateNoSymlinkTraversal(root, rel string) error {
	if rel == "." {
		return nil
	}
	cleaned, err := normalizeRepoRelativePath(rel)
	if err != nil {
		return err
	}
	cur := root
	parts := strings.Split(cleaned, "/")
	for i, part := range parts {
		if part == "" || part == "." {
			continue
		}
		cur = filepath.Join(cur, part)
		info, err := os.Lstat(cur)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) && i == len(parts)-1 {
				return nil
			}
			if errors.Is(err, os.ErrNotExist) {
				return nil
			}
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("path traverses symlink: %s", cleaned)
		}
	}
	return nil
}

func normalizeRepoRelativePath(p string) (string, error) {
	trimmed := strings.TrimSpace(p)
	if trimmed == "" {
		return "", errors.New("path is empty")
	}
	if strings.ContainsRune(trimmed, '\x00') {
		return "", errors.New("path contains NUL byte")
	}
	slashed := strings.ReplaceAll(trimmed, `\`, `/`)
	if filepath.IsAbs(trimmed) ||
		path.IsAbs(slashed) ||
		strings.HasPrefix(slashed, "//") ||
		hasWindowsVolumeName(slashed) {
		return "", fmt.Errorf("path must be repository-relative: %q", p)
	}

	cleaned := path.Clean(slashed)
	if cleaned == "." {
		return ".", nil
	}
	if cleaned == ".." ||
		strings.HasPrefix(cleaned, "../") ||
		strings.Contains(cleaned, "/../") ||
		strings.HasPrefix(cleaned, "//") ||
		hasWindowsVolumeName(cleaned) {
		return "", fmt.Errorf("path escapes repository root: %q", p)
	}
	if strings.HasPrefix(cleaned, "-") {
		return "", fmt.Errorf("path %q is invalid: must not start with '-'", p)
	}
	return cleaned, nil
}

func hasWindowsVolumeName(p string) bool {
	if len(p) < 2 {
		return false
	}
	if p[1] != ':' {
		return false
	}
	r := rune(p[0])
	return (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z')
}

func unifiedFileDiff(
	p string,
	oldData []byte,
	oldExists bool,
	oldTruncated bool,
	newData []byte,
	newExists bool,
	newTruncated bool,
	contextLines int,
) (part string, binaryOmitted, largeSkipped bool) {
	return unifiedFileDiffPaths(
		p,
		p,
		oldData,
		oldExists,
		oldTruncated,
		newData,
		newExists,
		newTruncated,
		contextLines,
	)
}

func treeChangesUnifiedDiff(
	ctx context.Context,
	baseTree *object.Tree,
	targetTree *object.Tree,
	changes object.Changes,
	contextLines int,
) (diffText string, omittedBinaryFiles, skippedLargeFiles int, err error) {
	var b strings.Builder
	for _, change := range changes {
		if err := ctx.Err(); err != nil {
			return "", 0, 0, err
		}
		oldPath, newPath, displayPath := objectChangePaths(change)
		oldData, oldExists, oldTruncated, err := treePathContentLimited(baseTree, oldPath, hardBlobReadBytes)
		if err != nil {
			return "", 0, 0, err
		}
		newData, newExists, newTruncated, err := treePathContentLimited(targetTree, newPath, hardBlobReadBytes)
		if err != nil {
			return "", 0, 0, err
		}
		if oldPath == "" {
			oldPath = displayPath
		}
		if newPath == "" {
			newPath = displayPath
		}
		part, binaryOmitted, largeSkipped := unifiedFileDiffPaths(
			oldPath, newPath, oldData, oldExists, oldTruncated, newData, newExists, newTruncated, contextLines,
		)
		if binaryOmitted {
			omittedBinaryFiles++
		}
		if largeSkipped {
			skippedLargeFiles++
		}
		b.WriteString(part)
	}
	return b.String(), omittedBinaryFiles, skippedLargeFiles, nil
}

func objectChangePaths(change *object.Change) (from, to, display string) {
	if change == nil {
		return "", "", ""
	}
	if change.From.Name != "" {
		from = change.From.Name
	}
	if change.To.Name != "" {
		to = change.To.Name
	}
	display = to
	if display == "" {
		display = from
	}
	return from, to, display
}

func treePathContentLimited(
	tree *object.Tree,
	p string,
	maxBytes int64,
) (data []byte, ok, truncated bool, err error) {
	if tree == nil {
		return nil, false, false, nil
	}
	f, err := tree.File(p)
	if err != nil {
		return nil, false, false, nil
	}
	r, err := f.Reader()
	if err != nil {
		return nil, false, false, err
	}
	defer r.Close()
	data, truncated, err = readLimited(r, maxBytes)
	if err != nil {
		return nil, false, false, err
	}
	return data, true, truncated, nil
}

func readLimited(r io.Reader, maxBytes int64) (data []byte, ok bool, err error) {
	var b bytes.Buffer
	n, err := io.CopyN(&b, r, maxBytes+1)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, false, err
	}
	data = b.Bytes()
	if n > maxBytes || int64(len(data)) > maxBytes {
		return data[:maxBytes], true, nil
	}
	return data, false, nil
}

func unifiedFileDiffPaths(
	oldPath string,
	newPath string,
	oldData []byte,
	oldExists bool,
	oldTruncated bool,
	newData []byte,
	newExists bool,
	newTruncated bool,
	contextLines int,
) (part string, binaryOmitted, largeSkipped bool) {
	if oldExists && newExists && bytes.Equal(oldData, newData) {
		return "", false, false
	}
	if !oldExists && !newExists {
		return "", false, false
	}
	displayPath := newPath
	if displayPath == "" {
		displayPath = oldPath
	}
	if oldPath == "" {
		oldPath = displayPath
	}
	if newPath == "" {
		newPath = displayPath
	}

	var b strings.Builder
	writeDiffHeader(&b, oldPath, newPath)

	if oldTruncated || newTruncated {
		b.WriteString("[diff omitted: file exceeds per-file read limit]\n")
		return b.String(), false, true
	}

	if !oldExists {
		b.WriteString("new file mode 100644\n")
	}
	if !newExists {
		b.WriteString("deleted file mode 100644\n")
	}

	if isBinaryData(oldData) || isBinaryData(newData) {
		b.WriteString("Binary files ")
		if oldExists {
			b.WriteString("a/")
			b.WriteString(oldPath)
		} else {
			b.WriteString("/dev/null")
		}
		b.WriteString(" and ")
		if newExists {
			b.WriteString("b/")
			b.WriteString(newPath)
		} else {
			b.WriteString("/dev/null")
		}
		b.WriteString(" differ\n")
		return b.String(), true, false
	}

	if oldExists {
		b.WriteString("--- a/")
		b.WriteString(oldPath)
		b.WriteString("\n")
	} else {
		b.WriteString("--- /dev/null\n")
	}
	if newExists {
		b.WriteString("+++ b/")
		b.WriteString(newPath)
		b.WriteString("\n")
	} else {
		b.WriteString("+++ /dev/null\n")
	}

	body := unifiedLineDiff(string(oldData), string(newData), contextLines)
	if body == "" {
		return "", false, false
	}
	b.WriteString(body)
	return b.String(), false, false
}

func writeDiffHeader(b *strings.Builder, oldPath, newPath string) {
	b.WriteString("diff --git a/")
	b.WriteString(oldPath)
	b.WriteString(" b/")
	b.WriteString(newPath)
	b.WriteString("\n")
}

type diffRecord struct {
	Text       string
	HasNewline bool
}

type diffOp struct {
	Kind    byte
	Line    diffRecord
	OldLine int
	NewLine int
}

func unifiedLineDiff(oldText, newText string, contextLines int) string {
	if contextLines < 0 {
		contextLines = defaultContextLines
	}
	oldLines := splitDiffRecords(oldText)
	newLines := splitDiffRecords(newText)
	ops := buildDiffOps(oldLines, newLines)
	if len(ops) == 0 {
		return ""
	}
	ranges := diffHunkRanges(ops, contextLines)
	if len(ranges) == 0 {
		return ""
	}

	var b strings.Builder
	for _, r := range ranges {
		hops := ops[r[0]:r[1]]
		oldStart, oldCount, newStart, newCount := hunkStats(hops)
		fmt.Fprintf(
			&b,
			"@@ -%s +%s @@\n",
			formatUnifiedRange(oldStart, oldCount),
			formatUnifiedRange(newStart, newCount),
		)
		for _, op := range hops {
			writeDiffRecord(&b, op)
		}
	}
	return b.String()
}

func splitDiffRecords(s string) []diffRecord {
	if s == "" {
		return nil
	}
	out := make([]diffRecord, 0, strings.Count(s, "\n")+1)
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] != '\n' {
			continue
		}
		out = append(out, diffRecord{
			Text:       s[start:i],
			HasNewline: true,
		})
		start = i + 1
	}
	if start < len(s) {
		out = append(out, diffRecord{
			Text:       s[start:],
			HasNewline: false,
		})
	}
	return out
}

func buildDiffOps(oldLines, newLines []diffRecord) []diffOp {
	n := len(oldLines)
	m := len(newLines)
	if n == 0 && m == 0 {
		return nil
	}
	if (n+1)*(m+1) > hardLineDiffMatrixCells {
		return buildFullReplaceOps(oldLines, newLines)
	}

	width := m + 1
	dp := make([]int, (n+1)*(m+1))
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if diffRecordsEqual(oldLines[i], newLines[j]) {
				dp[i*width+j] = dp[(i+1)*width+j+1] + 1
				continue
			}
			a := dp[(i+1)*width+j]
			b := dp[i*width+j+1]
			dp[i*width+j] = max(a, b)
		}
	}

	ops := make([]diffOp, 0, n+m)
	i, j := 0, 0
	for i < n || j < m {
		switch {
		case i < n && j < m && diffRecordsEqual(oldLines[i], newLines[j]):
			ops = append(ops, diffOp{Kind: '=', Line: oldLines[i], OldLine: i + 1, NewLine: j + 1})
			i++
			j++
		case j < m && (i == n || dp[i*width+j+1] >= dp[(i+1)*width+j]):
			ops = append(ops, diffOp{Kind: '+', Line: newLines[j], OldLine: i + 1, NewLine: j + 1})
			j++
		case i < n:
			ops = append(ops, diffOp{Kind: '-', Line: oldLines[i], OldLine: i + 1, NewLine: j + 1})
			i++
		}
	}
	return ops
}

func buildFullReplaceOps(oldLines, newLines []diffRecord) []diffOp {
	ops := make([]diffOp, 0, len(oldLines)+len(newLines))
	for i, line := range oldLines {
		ops = append(ops, diffOp{Kind: '-', Line: line, OldLine: i + 1, NewLine: 1})
	}
	for i, line := range newLines {
		ops = append(ops, diffOp{Kind: '+', Line: line, OldLine: len(oldLines) + 1, NewLine: i + 1})
	}
	return ops
}

func diffRecordsEqual(a, b diffRecord) bool {
	return a.Text == b.Text && a.HasNewline == b.HasNewline
}

func diffHunkRanges(ops []diffOp, contextLines int) [][2]int {
	var ranges [][2]int
	for i := 0; i < len(ops); {
		for i < len(ops) && ops[i].Kind == '=' {
			i++
		}
		if i >= len(ops) {
			break
		}

		start := max(i-contextLines, 0)
		lastChange := i
		j := i + 1
		for j < len(ops) {
			if ops[j].Kind != '=' {
				lastChange = j
			}
			if j-lastChange > contextLines {
				break
			}
			j++
		}
		end := min(lastChange+contextLines+1, len(ops))
		if len(ranges) > 0 && start <= ranges[len(ranges)-1][1] {
			if end > ranges[len(ranges)-1][1] {
				ranges[len(ranges)-1][1] = end
			}
		} else {
			ranges = append(ranges, [2]int{start, end})
		}
		i = end
	}
	return ranges
}

func hunkStats(ops []diffOp) (oldStart, oldCount, newStart, newCount int) {
	for _, op := range ops {
		if op.Kind != '+' {
			oldCount++
			if oldStart == 0 {
				oldStart = op.OldLine
			}
		}
		if op.Kind != '-' {
			newCount++
			if newStart == 0 {
				newStart = op.NewLine
			}
		}
	}
	if oldStart == 0 {
		oldStart = max(ops[0].OldLine-1, 0)
	}
	if newStart == 0 {
		newStart = max(ops[0].NewLine-1, 0)
	}
	return oldStart, oldCount, newStart, newCount
}

func formatUnifiedRange(start, count int) string {
	if count == 1 {
		return strconv.Itoa(start)
	}
	return fmt.Sprintf("%d,%d", start, count)
}

func writeDiffRecord(b *strings.Builder, op diffOp) {
	if op.Kind == '=' {
		b.WriteByte(' ')
	} else {
		b.WriteByte(op.Kind)
	}
	b.WriteString(op.Line.Text)
	b.WriteByte('\n')
	if !op.Line.HasNewline {
		b.WriteString("\\ No newline at end of file\n")
	}
}

func isBinaryData(data []byte) bool {
	if len(data) == 0 {
		return false
	}
	return bytes.IndexByte(data, 0) >= 0 || !utf8.Valid(data)
}
