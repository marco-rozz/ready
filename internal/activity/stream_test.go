package activity

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
)

const testStreamName = "stream"

type sampleStream struct{ reader *schema.StreamReader[string] }

func (*sampleStream) Info(context.Context) (*schema.ToolInfo, error) {
	return &schema.ToolInfo{Name: testStreamName}, nil
}

func (s *sampleStream) StreamableRun(context.Context, string, ...tool.Option) (*schema.StreamReader[string], error) {
	return s.reader, nil
}

func TestStreamActivityUntilEOF(t *testing.T) {
	source, writer := schema.Pipe[string](0)
	tracker := New()
	defer tracker.Close()
	wrapped := &StreamTool{Inner: &sampleStream{reader: source}}
	reader, err := wrapped.StreamableRun(WithTracker(context.Background(), tracker), "{}")
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	if got := <-tracker.Updates(); got.ToolName != testStreamName {
		t.Fatalf("start: %+v", got)
	}
	go func() { writer.Send("chunk", nil); writer.Close() }()
	value, err := reader.Recv()
	if err != nil || value != "chunk" {
		t.Fatalf("chunk %q: %v", value, err)
	}
	if _, err := reader.Recv(); !errors.Is(err, io.EOF) {
		t.Fatalf("end: %v", err)
	}
	select {
	case got := <-tracker.Updates():
		if got.ToolName != "" {
			t.Fatalf("end: %+v", got)
		}
	case <-time.After(time.Second):
		t.Fatal("activity did not end")
	}
}

func TestStreamActivityEndsOnCancellation(t *testing.T) {
	source, writer := schema.Pipe[string](0)
	tracker := New()
	defer tracker.Close()
	ctx, cancel := context.WithCancel(WithTracker(context.Background(), tracker))
	wrapped := &StreamTool{Inner: &sampleStream{reader: source}}
	reader, err := wrapped.StreamableRun(ctx, "{}")
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	if got := <-tracker.Updates(); got.ToolName != testStreamName {
		t.Fatalf("start: %+v", got)
	}
	cancel()
	select {
	case got := <-tracker.Updates():
		if got.ToolName != "" {
			t.Fatalf("end: %+v", got)
		}
	case <-time.After(time.Second):
		t.Fatal("activity survived cancellation")
	}
	writer.Close()
}

func TestStreamActivityEndsOnEarlyClose(t *testing.T) {
	source, writer := schema.Pipe[string](0)
	tracker := New()
	defer tracker.Close()
	wrapped := &StreamTool{Inner: &sampleStream{reader: source}}
	reader, err := wrapped.StreamableRun(WithTracker(context.Background(), tracker), "{}")
	if err != nil {
		t.Fatal(err)
	}
	if got := <-tracker.Updates(); got.ToolName != testStreamName {
		t.Fatalf("start: %+v", got)
	}
	reader.Close()
	select {
	case got := <-tracker.Updates():
		if got.ToolName != "" {
			t.Fatalf("end: %+v", got)
		}
	case <-time.After(time.Second):
		t.Fatal("activity survived reader close")
	}
	writer.Close()
}

func TestStreamActivityEndsWhenCopiedReadersClose(t *testing.T) {
	source, writer := schema.Pipe[string](0)
	tracker := New()
	defer tracker.Close()
	wrapped := &StreamTool{Inner: &sampleStream{reader: source}}
	reader, err := wrapped.StreamableRun(WithTracker(context.Background(), tracker), "{}")
	if err != nil {
		t.Fatal(err)
	}
	<-tracker.Updates()
	readers := reader.Copy(2)
	readers[0].Close()
	select {
	case got := <-tracker.Updates():
		t.Fatalf("activity ended while a reader remained: %+v", got)
	default:
	}
	readers[1].Close()
	select {
	case got := <-tracker.Updates():
		if got.ToolName != "" {
			t.Fatalf("end: %+v", got)
		}
	case <-time.After(time.Second):
		t.Fatal("activity survived closing copies")
	}
	writer.Close()
}
