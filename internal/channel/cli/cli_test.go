package cli

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/marco-rozz/ready/internal/channel"
)

const channelName = "cli"

func ensureClose(t *testing.T, c io.Closer) {
	t.Helper()
	if err := c.Close(); err != nil {
		t.Errorf("failed to close: %v", err)
	}
}

func TestChannel_Name(t *testing.T) {
	ch := NewWithIO(strings.NewReader(""), &bytes.Buffer{})
	defer func() { ensureClose(t, ch) }()

	if ch.Name() != channelName {
		t.Errorf("expected name 'cli', got %q", ch.Name())
	}
}

func TestChannel_Send(t *testing.T) {
	var buf bytes.Buffer
	ch := NewWithIO(strings.NewReader(""), &buf)
	defer func() { ensureClose(t, ch) }()

	msg := channel.Message{Content: "hello world"}

	if err := ch.Send(context.Background(), msg); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if got := buf.String(); got != "hello world\n" {
		t.Errorf("expected 'hello world\\n', got %q", got)
	}
}

func TestChannel_Receive(t *testing.T) {
	input := "user input\n"
	ch := NewWithIO(strings.NewReader(input), &bytes.Buffer{})
	defer func() { ensureClose(t, ch) }()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	msg, err := ch.Receive(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if msg.Content != "user input" {
		t.Errorf("expected 'user input', got %q", msg.Content)
	}

	if msg.ChannelName != channelName {
		t.Errorf("expected channel name 'cli', got %q", msg.ChannelName)
	}
}

func TestChannel_ReceiveMultipleLines(t *testing.T) {
	input := "line1\nline2\nline3\n"
	ch := NewWithIO(strings.NewReader(input), &bytes.Buffer{})
	defer func() { ensureClose(t, ch) }()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	for _, expected := range []string{"line1", "line2", "line3"} {
		msg, err := ch.Receive(ctx)
		if err != nil {
			t.Fatalf("unexpected error reading %q: %v", expected, err)
		}

		if msg.Content != expected {
			t.Errorf("expected %q, got %q", expected, msg.Content)
		}
	}
}

func TestChannel_ReceiveContextCancellation(t *testing.T) {
	// Use a reader that blocks forever (pipe with no writer)
	r, _ := strings.NewReader(""), &bytes.Buffer{}
	// Actually use a reader that never produces data.
	// strings.NewReader("") will EOF immediately, so we need a blocking reader.
	pr, _ := newBlockingReader()
	ch := NewWithIO(pr, &bytes.Buffer{})
	defer func() { ensureClose(t, ch) }()

	ctx, cancel := context.WithCancel(context.Background())

	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	_, err := ch.Receive(ctx)
	if err == nil {
		t.Fatal("expected error from cancelled context")
	}

	_ = r // avoid unused variable
}

// blockingReader is an io.Reader that never returns data.
type blockingReader struct {
	ch chan struct{}
}

func newBlockingReader() (*blockingReader, func()) {
	br := &blockingReader{ch: make(chan struct{})}

	return br, func() { close(br.ch) }
}

func (br *blockingReader) Read(_ []byte) (int, error) {
	<-br.ch // block until closed

	return 0, context.Canceled
}
