package session

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/marco-rozz/ready/internal/persistence"
	"github.com/rs/zerolog"
)

// fileTimeLayout is the human-readable local start-time prefix of a Session
// file name. It is for humans only (it repeats an hour at the DST fall-back):
// files are ordered by the UUIDv7 that follows it.
const fileTimeLayout = "2006-01-02T15-04-05"

// fileSuffix is the Session file extension.
const fileSuffix = ".jsonl"

// tailProbe bounds how much of a file's end is read to find its last line;
// a closing record is always far smaller.
const tailProbe = 64 * 1024

// Session is one conversation Session of an address.
type Session struct {
	// ID is the Session's UUIDv7.
	ID string
	// Address is the owning Conversation address.
	Address Address
	// StartedAt is the Session's local start time (the file name prefix).
	StartedAt time.Time
	// Path is the Session file path (created lazily at the first Turn).
	Path string
}

// Store maps Sessions onto the filesystem: one directory per address under
// the root, one append-only JSONL file per Session, and no index — the
// current Session of an address is its newest file (by UUIDv7) when that
// file does not end with a closing record.
type Store struct {
	root   string
	logger zerolog.Logger
}

// NewStore returns a store rooted at root (created lazily, 0700).
func NewStore(root string, logger zerolog.Logger) (*Store, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve sessions path: %w", err)
	}

	return &Store{root: abs, logger: logger}, nil
}

// Root returns the absolute sessions root.
func (s *Store) Root() string { return s.root }

// newSession builds a fresh Session for addr with a new UUIDv7, started now.
func (s *Store) newSession(addr Address, now time.Time) (*Session, error) {
	id, err := uuid.NewV7()
	if err != nil {
		return nil, fmt.Errorf("failed to mint session id: %w", err)
	}

	return s.sessionFor(addr, id.String(), now), nil
}

// sessionFor builds the Session value for a known id and start time.
func (s *Store) sessionFor(addr Address, id string, start time.Time) *Session {
	name := start.Local().Format(fileTimeLayout) + "_" + id + fileSuffix

	return &Session{
		ID:        id,
		Address:   addr,
		StartedAt: start,
		Path:      filepath.Join(s.root, addr.DirName(), name),
	}
}

// sessionFile is one scheme-matching file of an address directory.
type sessionFile struct {
	path  string
	id    string
	day   time.Time // the name's local calendar day
	start time.Time // the name's local start time
	size  int64
}

// parseFileName splits `<YYYY-MM-DDTHH-MM-SS>_<uuidv7>.jsonl`.
func parseFileName(name string) (start time.Time, id string, ok bool) {
	stem, hasSuffix := strings.CutSuffix(name, fileSuffix)
	if !hasSuffix {
		return time.Time{}, "", false
	}

	ts, rawID, found := strings.Cut(stem, "_")
	if !found {
		return time.Time{}, "", false
	}

	start, err := time.ParseInLocation(fileTimeLayout, ts, time.Local)
	if err != nil {
		return time.Time{}, "", false
	}

	parsed, err := uuid.Parse(rawID)
	if err != nil || parsed.Version() != 7 {
		return time.Time{}, "", false
	}

	return start, parsed.String(), true
}

// listFiles returns the scheme-matching files of dir ordered by UUIDv7
// (oldest first). A missing directory lists as empty.
func listFiles(dir string) ([]sessionFile, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}

	if err != nil {
		return nil, fmt.Errorf("failed to list %q: %w", dir, err)
	}

	var out []sessionFile

	for _, e := range entries {
		if !e.Type().IsRegular() {
			continue
		}

		start, id, ok := parseFileName(e.Name())
		if !ok {
			continue
		}

		info, err := e.Info()
		if err != nil {
			continue
		}

		y, m, d := start.Date()
		out = append(out, sessionFile{
			path:  filepath.Join(dir, e.Name()),
			id:    id,
			day:   time.Date(y, m, d, 0, 0, 0, 0, time.Local),
			start: start,
			size:  info.Size(),
		})
	}

	// Canonical UUIDv7 strings sort lexicographically in creation order.
	sort.Slice(out, func(i, j int) bool { return out[i].id < out[j].id })

	return out, nil
}

// isClosed reports whether the file's last line is a closing record.
func isClosed(path string) (bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return false, fmt.Errorf("failed to open %q: %w", path, err)
	}
	defer func() { _ = f.Close() }()

	info, err := f.Stat()
	if err != nil {
		return false, fmt.Errorf("failed to stat %q: %w", path, err)
	}

	offset := max(info.Size()-tailProbe, 0)

	buf := make([]byte, info.Size()-offset)
	if _, err := f.ReadAt(buf, offset); err != nil && !errors.Is(err, io.EOF) {
		return false, fmt.Errorf("failed to read the tail of %q: %w", path, err)
	}

	buf = bytes.TrimRight(buf, "\n\r\t ")
	if i := bytes.LastIndexByte(buf, '\n'); i >= 0 {
		buf = buf[i+1:]
	} else if offset > 0 {
		return false, nil // a last line longer than the probe is no closing record
	}

	var last persistence.HistoryMessage
	if err := json.Unmarshal(buf, &last); err != nil {
		return false, nil // a corrupt last line is crash residue, not a closing record
	}

	return last.Kind == persistence.KindSessionClosed, nil
}

// resolve returns the current Session of addr: its newest file when that
// file does not end with a closing record, nil otherwise (the address has no
// current Session). Older files without a closing record cannot arise with a
// single writer; one found (manual tampering) is treated as ended with a
// WARN.
func (s *Store) resolve(ctx context.Context, addr Address) (*Session, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("failed to resolve session: %w", err)
	}

	files, err := listFiles(filepath.Join(s.root, addr.DirName()))
	if err != nil || len(files) == 0 {
		return nil, err
	}

	for _, f := range files[:len(files)-1] {
		if closed, cerr := isClosed(f.path); cerr == nil && !closed {
			s.logger.Warn().
				Str("address", string(addr)).
				Str("file", f.path).
				Msg("session file without a closing record is not the newest — treated as ended")
		}
	}

	newest := files[len(files)-1]

	closed, err := isClosed(newest.path)
	if err != nil {
		return nil, err
	}

	if closed {
		return nil, nil
	}

	return &Session{ID: newest.id, Address: addr, StartedAt: newest.start, Path: newest.path}, nil
}
