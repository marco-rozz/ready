package zmq

import "testing"

func TestNumericLoopbackBind(t *testing.T) {
	for _, tc := range []struct {
		addr string
		want bool
	}{
		{"tcp://127.0.0.1:7600", true},
		{"tcp://[::1]:7600", true},
		{"tcp://localhost:7600", false},
		{"tcp://0.0.0.0:7600", false},
		{"tcp://192.168.1.2:7600", false},
		{"ipc:///tmp/ready.sock", false},
	} {
		t.Run(tc.addr, func(t *testing.T) {
			if got := numericLoopbackBind(tc.addr); got != tc.want {
				t.Fatalf("numericLoopbackBind(%q) = %v, want %v", tc.addr, got, tc.want)
			}
		})
	}
}
