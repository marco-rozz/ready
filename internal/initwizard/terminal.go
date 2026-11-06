package initwizard

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/muesli/cancelreader"
	"golang.org/x/term"
)

type terminalStreams struct {
	io.Reader
	io.Writer
}

var _ io.ReadWriter = terminalStreams{}

// readTerminalLine owns the raw-mode interval and cancellable reader for one
// answer. It joins the cancellation callback before closing its reader and
// restores the original terminal state on every exit, including Ctrl+C/EOF.
func readTerminalLine(ctx context.Context, input *os.File, out io.Writer, secret bool) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", fmt.Errorf("read terminal: %w", err)
	}
	state, err := term.GetState(int(input.Fd()))
	if err != nil {
		return "", fmt.Errorf("read terminal state: %w", err)
	}
	defer func() { _ = term.Restore(int(input.Fd()), state) }()
	reader, err := cancelreader.NewReader(input)
	if err != nil {
		return "", fmt.Errorf("open cancellable terminal reader: %w", err)
	}
	defer func() { _ = reader.Close() }()
	if _, rawErr := term.MakeRaw(int(input.Fd())); rawErr != nil {
		return "", fmt.Errorf("prepare terminal input: %w", rawErr)
	}
	cancelled := make(chan struct{})
	stop := context.AfterFunc(ctx, func() { reader.Cancel(); close(cancelled) })
	defer func() {
		if !stop() {
			<-cancelled
		}
	}()
	terminal := term.NewTerminal(terminalStreams{Reader: reader, Writer: out}, "")
	var line string
	if secret {
		line, err = terminal.ReadPassword("")
	} else {
		line, err = terminal.ReadLine()
	}
	if ctx.Err() != nil {
		return "", fmt.Errorf("read terminal: %w", ctx.Err())
	}
	if errors.Is(err, io.EOF) {
		return "", ErrCancelled
	}
	if err != nil {
		return "", fmt.Errorf("read terminal input: %w", err)
	}

	return line, nil
}
