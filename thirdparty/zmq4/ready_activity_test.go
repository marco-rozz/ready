package zmq4

import (
	"context"
	"net"
	"sync/atomic"
	"testing"
	"time"
)

func TestReadyActivityTimeoutClosesPeerBeforeReply(t *testing.T) {
	server, client := net.Pipe()
	defer client.Close()
	conn := &Conn{rw: server, sec: nullSecurity{}}
	conn.Peer.Meta = Metadata{sysSockID: "peer"}
	writer := newRouterMWriter(context.Background())
	writer.ws = []*Conn{conn}
	var failed atomic.Bool
	started := make(chan struct{})
	finished := make(chan error, 1)
	go func() {
		finished <- writer.writeActivity(NewMsgFrom([]byte("peer"), []byte{}, []byte("activity")), 50*time.Millisecond, func() bool { close(started); return true }, func() { failed.Store(true) })
	}()
	<-started
	reply := make(chan error, 1)
	go func() {
		reply <- writer.writeChecked(NewMsgFrom([]byte("peer"), []byte{}, []byte("reply")), func() bool { return !failed.Load() })
	}()
	select {
	case err := <-finished:
		if err == nil {
			t.Fatal("expected activity timeout")
		}
	case <-time.After(time.Second):
		t.Fatal("activity did not time out")
	}
	select {
	case err := <-reply:
		if err == nil {
			t.Fatal("reply accepted on failed connection")
		}
	case <-time.After(time.Second):
		t.Fatal("reply remained blocked")
	}
	if !failed.Load() || !conn.Closed() {
		t.Fatal("failed activity did not close peer")
	}
}

func TestReadyActivityDropsExpiredRequest(t *testing.T) {
	writer := newRouterMWriter(context.Background())
	failed := false
	if err := writer.writeActivity(NewMsgFrom([]byte("peer"), []byte{}, []byte("activity")), time.Second, func() bool { return false }, func() { failed = true }); err != nil {
		t.Fatal(err)
	}
	if failed {
		t.Fatal("expired request reported transport failure")
	}
}

func TestReadyActivityDisconnectedPeer(t *testing.T) {
	writer := newRouterMWriter(context.Background())
	failed := false
	err := writer.writeActivity(NewMsgFrom([]byte("gone"), []byte{}, []byte("activity")), time.Second, func() bool { return true }, func() { failed = true })
	if err == nil || !failed {
		t.Fatalf("disconnected peer: error=%v failed=%v", err, failed)
	}
}
