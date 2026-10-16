package channel

import (
	"context"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"
)

// testLogger returns a silent zerolog.Logger for use in tests.
func testLogger() zerolog.Logger {
	return zerolog.New(io.Discard)
}

// mockChannel is a test double that implements Channel.
type mockChannel struct {
	name   string
	mu     sync.Mutex
	msgCh  chan Message
	sent   []Message
	closed bool
}

func newMockChannel(name string) *mockChannel {
	return &mockChannel{
		name:  name,
		msgCh: make(chan Message, 10),
	}
}

func (m *mockChannel) Name() string { return m.name }

func (m *mockChannel) Receive(ctx context.Context) (Message, error) {
	select {
	case msg := <-m.msgCh:
		return msg, nil
	case <-ctx.Done():
		return Message{}, ctx.Err()
	}
}

func (m *mockChannel) Send(_ context.Context, msg Message) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.sent = append(m.sent, msg)

	return nil
}

func (m *mockChannel) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.closed = true

	return nil
}

func (m *mockChannel) getSent() []Message {
	m.mu.Lock()
	defer m.mu.Unlock()

	result := make([]Message, len(m.sent))
	copy(result, m.sent)

	return result
}

func (m *mockChannel) isClosed() bool {
	m.mu.Lock()
	defer m.mu.Unlock()

	return m.closed
}

func TestManager_NoChannels(t *testing.T) {
	mgr := NewManager(testLogger())

	if err := mgr.Start(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if err := mgr.Stop(); err != nil {
		t.Fatalf("unexpected error on stop: %v", err)
	}
}

func TestManager_ReceiveMessages(t *testing.T) {
	mock := newMockChannel("test")
	mgr := NewManager(testLogger(), mock)

	if err := mgr.Start(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	mock.msgCh <- Message{Content: "hello"}

	select {
	case msg := <-mgr.Incoming():
		if msg.Content != "hello" {
			t.Errorf("expected 'hello', got %q", msg.Content)
		}

		if msg.ChannelName != "test" {
			t.Errorf("expected channel name 'test', got %q", msg.ChannelName)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for message")
	}

	if err := mgr.Stop(); err != nil {
		t.Fatalf("unexpected error on stop: %v", err)
	}
}

func TestManager_SendToUnknownChannel(t *testing.T) {
	mgr := NewManager(testLogger())

	if err := mgr.Start(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	err := mgr.SendTo(context.Background(), "nonexistent", Message{Content: "hi"})
	if err == nil {
		t.Fatal("expected error for unknown channel")
	}

	if !errors.Is(err, ErrUnknownChannel) {
		t.Errorf("expected ErrUnknownChannel, got: %v", err)
	}

	if err := mgr.Stop(); err != nil {
		t.Fatalf("unexpected error on stop: %v", err)
	}
}

func TestManager_SendToChannel(t *testing.T) {
	mock := newMockChannel("test")
	mgr := NewManager(testLogger(), mock)

	if err := mgr.Start(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	msg := Message{Content: "response"}
	if err := mgr.SendTo(context.Background(), "test", msg); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	sent := mock.getSent()
	if len(sent) != 1 || sent[0].Content != "response" {
		t.Errorf("expected 1 sent message with 'response', got %v", sent)
	}

	if err := mgr.Stop(); err != nil {
		t.Fatalf("unexpected error on stop: %v", err)
	}
}

func TestManager_StopClosesChannels(t *testing.T) {
	mock := newMockChannel("test")
	mgr := NewManager(testLogger(), mock)

	if err := mgr.Start(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if err := mgr.Stop(); err != nil {
		t.Fatalf("unexpected error on stop: %v", err)
	}

	if !mock.isClosed() {
		t.Error("expected channel to be closed after Stop")
	}
}

func TestManager_MultipleChannels(t *testing.T) {
	mock1 := newMockChannel("ch1")
	mock2 := newMockChannel("ch2")
	mgr := NewManager(testLogger(), mock1, mock2)

	if err := mgr.Start(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	mock1.msgCh <- Message{Content: "from-ch1"}
	mock2.msgCh <- Message{Content: "from-ch2"}

	received := make(map[string]bool)

	for range 2 {
		select {
		case msg := <-mgr.Incoming():
			received[msg.Content] = true
		case <-time.After(time.Second):
			t.Fatal("timed out waiting for messages")
		}
	}

	if !received["from-ch1"] || !received["from-ch2"] {
		t.Errorf("expected both messages, got %v", received)
	}

	if err := mgr.Stop(); err != nil {
		t.Fatalf("unexpected error on stop: %v", err)
	}
}
