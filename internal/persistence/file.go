// Package persistence stores conversation Session transcripts as append-only
// JSONL files (the session-lifecycle, the architecture decision records/0014): one file
// per Session, never rewritten, truncated, or renamed.
package persistence

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
)

// Session file and directory permissions (session-lifecycle: directories
// 0700, files 0600 — tool results carry entity states and presence data).
const (
	dirPerm  = 0o700
	filePerm = 0o600
)

// SessionFile is the append-only handle of one Session's JSONL file. The file
// is created lazily by the first Append (so a Session that never records a
// Turn leaves nothing on disk). ConversationHistory is its only writer.
type SessionFile struct {
	path string

	mu sync.Mutex
	// framingChecked records that the framing repair ran
	// for this file in this process.
	framingChecked bool
}

// NewSessionFile returns the handle for the file at path. Nothing is created
// on disk until the first Append.
func NewSessionFile(path string) *SessionFile {
	return &SessionFile{path: path}
}

// Path returns the file path.
func (f *SessionFile) Path() string { return f.path }

// Exists reports whether the file has been created.
func (f *SessionFile) Exists() bool {
	_, err := os.Stat(f.path)

	return err == nil
}

// Read returns the whole file. A file that does not exist yet reads as empty.
func (f *SessionFile) Read(ctx context.Context) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("failed to read session file: %w", err)
	}

	data, err := os.ReadFile(f.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}

	if err != nil {
		return nil, fmt.Errorf("failed to read session file %q: %w", f.path, err)
	}

	return data, nil
}

// Append writes lines (each without its trailing newline) at the end of the
// file in ONE buffered write (design R3: a completed tool-call group lands in
// a single write, narrowing the crash window). It returns how many bytes of
// the joined payload reached the file, so a caller can tell exactly which
// lines were persisted when the write fails midway. Before the first append
// to an existing file in this process, a missing trailing newline is repaired
// so a line truncated by a crash stays one isolated corrupt line and never
// merges with the next entry.
func (f *SessionFile) Append(ctx context.Context, lines ...[]byte) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, fmt.Errorf("failed to append to session file: %w", err)
	}

	if len(lines) == 0 {
		return 0, nil
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	if err := os.MkdirAll(filepath.Dir(f.path), dirPerm); err != nil {
		return 0, fmt.Errorf("failed to create session directory: %w", err)
	}

	file, err := os.OpenFile(f.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, filePerm)
	if err != nil {
		return 0, fmt.Errorf("failed to open session file %q: %w", f.path, err)
	}
	defer func() { _ = file.Close() }()

	if !f.framingChecked {
		if repairErr := repairFraming(f.path, file); repairErr != nil {
			return 0, repairErr
		}

		f.framingChecked = true
	}

	var buf bytes.Buffer
	for _, line := range lines {
		buf.Write(line)
		buf.WriteByte('\n')
	}

	n, err := file.Write(buf.Bytes())
	if err != nil {
		// A partial write may leave a line without its newline: re-run the
		// framing repair before the next append.
		f.framingChecked = false

		return n, fmt.Errorf("failed to append to session file %q: %w", f.path, err)
	}

	return n, nil
}

// repairFraming writes a newline when the file's last byte is not one.
func repairFraming(path string, w io.Writer) error {
	r, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("failed to open session file for framing check: %w", err)
	}
	defer func() { _ = r.Close() }()

	info, err := r.Stat()
	if err != nil {
		return fmt.Errorf("failed to stat session file: %w", err)
	}

	if info.Size() == 0 {
		return nil
	}

	last := make([]byte, 1)
	if _, err := r.ReadAt(last, info.Size()-1); err != nil {
		return fmt.Errorf("failed to read session file tail: %w", err)
	}

	if last[0] == '\n' {
		return nil
	}

	if _, err := w.Write([]byte{'\n'}); err != nil {
		return fmt.Errorf("failed to repair session file framing: %w", err)
	}

	return nil
}
