package ioutil

import (
	"errors"
	"io/fs"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/flexigpt/llmtools-go/internal/fspolicy"
)

var (
	// ErrNotUTF8Text indicates a file could be read but is not valid UTF‑8.
	ErrNotUTF8Text        = errors.New("file is not valid UTF-8 text")
	ErrFileExceedsMaxSize = errors.New("file exceeds maximum allowed size")
)

// TextFile is a normalized in-memory view of a UTF‑8 text file.
// Lines never include trailing newline characters.
type TextFile struct {
	Path            string
	Perm            fs.FileMode
	Newline         NewlineKind
	HasFinalNewline bool
	Lines           []string
	SizeBytes       int64
	ModTimeUTC      *time.Time
}

// Render converts Lines back into a file string preserving newline style and final newline presence.
func (t *TextFile) Render() string {
	sep := t.Newline.sep()
	if len(t.Lines) == 0 {
		if t.HasFinalNewline {
			return sep
		}
		return ""
	}
	s := strings.Join(t.Lines, sep)
	if t.HasFinalNewline {
		s += sep
	}
	return s
}

// ReadTextFileUTF8 reads a file as UTF‑8 text and returns a normalized TextFile view.
// It preserves newline kind (LF vs CRLF) and whether the file ended with a final newline.
//
// Safety behavior (policy-driven):
//   - Enforces maxBytes if > 0.
func ReadTextFileUTF8(p fspolicy.FSPolicy, path string, maxBytes int64) (*TextFile, error) {
	abs, err := p.ResolvePath(path, "")
	if err != nil {
		return nil, err
	}

	st, err := p.RequireExistingRegularFileResolved(abs)
	if err != nil {
		return nil, err
	}

	// Use existing utility (bounded).
	s, err := ReadFile(abs, ReadEncodingText, maxBytes)
	if err != nil {
		return nil, err
	}
	if !utf8.ValidString(s) {
		return nil, ErrNotUTF8Text
	}

	kind := detectNewlineKind(s)
	norm, hasFinal := normalizeNewlines(s, kind)

	var lines []string
	if norm == "" && !hasFinal {
		lines = nil
	} else {
		// Note: if norm == "" and hasFinal == true, we want one empty line: [""].
		lines = strings.Split(norm, "\n")
	}

	mt := st.ModTime().UTC()
	out := &TextFile{
		Path:            abs,
		Perm:            st.Mode().Perm(),
		Newline:         kind,
		HasFinalNewline: hasFinal,
		Lines:           lines,
		SizeBytes:       st.Size(),
		ModTimeUTC:      &mt,
	}
	return out, nil
}

func detectNewlineKind(s string) NewlineKind {
	// If we see any CRLF, preserve CRLF; this matches most “Windows file” expectations.
	if strings.Contains(s, "\r\n") {
		return NewlineCRLF
	}
	return NewlineLF
}

func normalizeNewlines(s string, kind NewlineKind) (norm string, hasFinalNewline bool) {
	// Convert to internal '\n' representation for consistent line splitting.
	if kind == NewlineCRLF {
		s = strings.ReplaceAll(s, "\r\n", "\n")
		s = strings.ReplaceAll(s, "\r", "\n")
	} else {
		// If file is mostly LF but contains stray CR (rare), normalize them too.
		s = strings.ReplaceAll(s, "\r", "\n")
	}

	hasFinalNewline = strings.HasSuffix(s, "\n")
	if hasFinalNewline {
		s = strings.TrimSuffix(s, "\n")
	}
	return s, hasFinalNewline
}
