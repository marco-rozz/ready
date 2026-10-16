// Package cli implements the CLI channel using stdin/stdout.
package cli

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"sync"

	"github.com/marco-rozz/ready/internal/channel"
)

// ChannelName is the channel identifier used for routing and session keys.
const ChannelName = "cli"

// Guarantees at compile time that Channel implements the interface channel.Channel.
var _ channel.Channel = (*Channel)(nil)

// Channel implements the channel.Channel interface for interactive CLI usage.
type Channel struct {
	reader *bufio.Scanner
	writer io.Writer
	done   chan struct{}
	readCh chan readResult

	// closeOnce makes Close idempotent (criticalities-remediation LOW
	// lifecycle): the second call is a no-op instead of panicking.
	closeOnce sync.Once
}

type readResult struct {
	line string
	err  error
}

// New creates a CLI channel using os.Stdin and os.Stdout.
func New() *Channel {
	return NewWithIO(os.Stdin, os.Stdout)
}

// NewWithIO creates a CLI channel with custom reader/writer for testability.
func NewWithIO(r io.Reader, w io.Writer) *Channel {
	ch := &Channel{
		reader: bufio.NewScanner(r),
		writer: w,
		done:   make(chan struct{}),
		readCh: make(chan readResult, 1),
	}

	go ch.scanLoop()

	return ch
}

// scanLoop reads lines in a dedicated goroutine so Receive can
// respect context cancellation.
func (c *Channel) scanLoop() {
	for {
		if c.reader.Scan() {
			select {
			case c.readCh <- readResult{line: c.reader.Text()}:
			case <-c.done:
				return
			}
		} else {
			err := c.reader.Err()
			if err == nil {
				err = io.EOF
			}

			select {
			case c.readCh <- readResult{err: err}:
			case <-c.done:
			}

			return
		}
	}
}

// Name returns the channel identifier.
func (c *Channel) Name() string {
	return ChannelName
}

// Receive blocks until a line is read from stdin or the context is cancelled.
func (c *Channel) Receive(ctx context.Context) (channel.Message, error) {
	select {
	case result := <-c.readCh:
		if result.err != nil {
			return channel.Message{}, result.err
		}

		return channel.Message{
			Content:     result.line,
			ChannelName: ChannelName,
		}, nil
	case <-ctx.Done():
		return channel.Message{}, ctx.Err()
	}
}

// Send writes a message to stdout.
func (c *Channel) Send(_ context.Context, msg channel.Message) error {
	_, err := fmt.Fprintln(c.writer, msg.Content)
	if err != nil {
		return fmt.Errorf("failed to write to CLI output: %w", err)
	}

	return nil
}

// Writer returns the channel's output writer, used by the streaming callback
// to write chunks directly to stdout during the agent run.
func (c *Channel) Writer() io.Writer {
	return c.writer
}

// Close signals the scan loop to stop. criticalities-remediation (LOW
// lifecycle): idempotent — a second call is a no-op instead of panicking on
// the double channel close.
func (c *Channel) Close() error {
	c.closeOnce.Do(func() {
		close(c.done)
	})

	return nil
}
