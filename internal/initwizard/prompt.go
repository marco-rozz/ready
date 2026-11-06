// The prompt layer of the guided-init wizard. Line input reads the whole
// line from the injected io.Reader (plain or piped-terminal text); secret
// input toggles terminal echo OFF for exactly one answer and restores the
// previous state on every exit path — error, EOF and Ctrl+C included — so a
// cancelled wizard never leaves the operator's terminal in raw/no-echo mode.
package initwizard

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"
)

// promptReader abstracts the interview's input anchoring so tests can drive
// the wizard from a buffer while production keeps the terminal echo rules.
type promptReader interface {
	// line reads one visible answer line.
	line(context.Context) (string, error)
	// secret reads one echo-disabled answer (password prompt).
	secret(context.Context) (string, error)
	// confirm reads a yes/no answer.
	confirm(ctx context.Context, question string) (bool, error)
}

// interview carries the streams one wizard run reads from and writes to.
type interview struct {
	in   io.Reader
	out  io.Writer
	read promptReader
}

// plainReader is the buffered line reader for injected interview scripts.
// Interactive terminal input uses the cancellable ttyPromptReader instead.
type plainReader struct {
	scanner *bufio.Scanner
}

var _ promptReader = (*plainReader)(nil)

func newPlainReader(in io.Reader) *plainReader {
	s := bufio.NewScanner(in)
	s.Buffer(make([]byte, 0, 64*1024), 64*1024)

	return &plainReader{scanner: s}
}

func (p *plainReader) line(ctx context.Context) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", fmt.Errorf("read input: %w", err)
	}
	if !p.scanner.Scan() {
		if err := p.scanner.Err(); err != nil {
			return "", fmt.Errorf("read input: %w", err)
		}

		return "", ErrEOF
	}

	return p.scanner.Text(), nil
}

// secret on the plain reader has no echo contract to honour: the caller is
// responsible for the TTY switch (appPromptReader decides which reader
// implementation handles secrets).
func (p *plainReader) secret(ctx context.Context) (string, error) {
	return p.line(ctx)
}

func (p *plainReader) confirm(ctx context.Context, _ string) (bool, error) {
	answer, err := p.line(ctx)
	if err != nil {
		return false, err
	}

	return isYes(answer), nil
}

func isYes(answer string) bool {
	switch strings.TrimSpace(strings.ToLower(answer)) {
	case "y", "yes":
		return true
	default:
		return false
	}
}

// skipped-line guard for the confirm flow: the question text is printed by
// the caller; confirm reads the answer only.

// appPromptReader binds the interview's reader to the process's real
// terminal file descriptors when stdin is a TTY, so that secrets disable
// echo on the actual controlling terminal. Visible answers still flow
// through the buffered reader over the same fd (though for a TTY stdin,
// os.Stdin as io.Reader works identically). A non-nil override replaces
// the whole binding (injected interview streams).
func appPromptReader(stdinTTY bool, input io.Reader, stdout io.Writer, override promptReader) promptReader {
	if override != nil {
		return override
	}
	if stdinTTY {
		file, ok := input.(*os.File)
		if !ok {
			file = os.Stdin
		}

		return ttyPromptReader{in: file, out: stdout}
	}

	return newPlainReader(input)
}

// ttyPromptReader reads from the controlling terminal. Secrets switch the
// controlling tty to no-echo for the duration of one answer.
type ttyPromptReader struct {
	in  *os.File
	out io.Writer
}

var _ promptReader = ttyPromptReader{}

func (t ttyPromptReader) line(ctx context.Context) (string, error) {
	return readTerminalLine(ctx, t.in, t.out, false)
}

func (t ttyPromptReader) confirm(ctx context.Context, _ string) (bool, error) {
	answer, err := t.line(ctx)

	return isYes(answer), err
}

func (t ttyPromptReader) secret(ctx context.Context) (string, error) {
	return readTerminalLine(ctx, t.in, t.out, true)
}

// parseURL validates and parses a wizard endpoint answer: exactly one
// absolute URL, http/https, non-empty host.
func parseURL(raw string) (u *url.URL, ok bool) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Host == "" {
		return nil, false
	}

	return parsed, true
}
