// Daemon-side protocol-version gate tests (guided-init C.19/C.20): the gate
// precedes the challenge round (no pending entry survives a mismatch) and
// every session_start response carries the version.
package zmqcli

import (
	"context"
	"testing"
	"time"

	"github.com/marco-rozz/ready/internal/protocol"
)

func TestVersionGate_MismatchIssuesNoNonce(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch, addr, _ := newIntegrationChannelWithBuf(ctx, t, "secret-version", 0)
	dealer := dialStatusDealer(ctx, t, addr, "client-mismatch")
	_ = ch // closed via t.Cleanup

	sendDealerRequest(t, dealer, protocol.Request{
		ConnectionID:    "conn",
		Type:            protocol.RequestTypeSessionStart,
		Timestamp:       time.Now().UTC().Format(time.RFC3339),
		ProtocolVersion: protocol.Version + 1,
	})

	resp, ok := recvDealerResponse(t, dealer, 2*time.Second)
	if !ok {
		t.Fatal("no response to a version-mismatched session_start")
	}
	if resp.ErrorCode != protocol.ErrorCodeProtocolMismatch {
		t.Fatalf("error_code = %q, want protocol_mismatch", resp.ErrorCode)
	}
	if resp.ProtocolVersion != protocol.Version {
		t.Fatalf("daemon version echo = %d, want %d", resp.ProtocolVersion, protocol.Version)
	}
}

func TestVersionGate_MismatchLeavesNoStateAndChallengeStillWorks(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch, addr, _ := newIntegrationChannelWithBuf(ctx, t, "secret-version", 0)
	dealer := dialStatusDealer(ctx, t, addr, "client-gate")
	_ = ch

	// A mismatch first: gated, no challenge state.
	sendDealerRequest(t, dealer, protocol.Request{
		ConnectionID:    "conn-bad",
		Type:            protocol.RequestTypeSessionStart,
		Timestamp:       time.Now().UTC().Format(time.RFC3339),
		ProtocolVersion: protocol.Version + 3,
	})
	resp, ok := recvDealerResponse(t, dealer, 2*time.Second)
	if !ok || resp.ErrorCode != protocol.ErrorCodeProtocolMismatch {
		t.Fatalf("mismatch not gated first (ok=%v, error_code=%q)", ok, resp.ErrorCode)
	}

	// A version-correct probe then still gets a normal challenge.
	sendDealerRequest(t, dealer, protocol.Request{
		ConnectionID:    "conn-good",
		Type:            protocol.RequestTypeSessionStart,
		Timestamp:       time.Now().UTC().Format(time.RFC3339),
		ProtocolVersion: protocol.Version,
	})
	_, ok = recvDealerResponse(t, dealer, 2*time.Second)
	if !ok {
		t.Fatal("version-correct probe lost its challenge after a refused mismatch")
	}
}

func TestVersionGate_MissingVersionReturnsMismatch(t *testing.T) {
	const legacyConnection = "legacy"
	ctx := t.Context()
	ch, addr, _ := newIntegrationChannelWithBuf(ctx, t, "secret-version", 0)
	dealer := dialStatusDealer(ctx, t, addr, "legacy-client")
	sendDealerRequest(t, dealer, protocol.Request{ConnectionID: legacyConnection, RequestID: "probe", Type: protocol.RequestTypeSessionStart, Timestamp: time.Now().UTC().Format(time.RFC3339)})
	resp, ok := recvDealerResponse(t, dealer, 2*time.Second)
	if !ok || resp.ErrorCode != protocol.ErrorCodeProtocolMismatch || resp.ProtocolVersion != protocol.Version || resp.RequestID != "probe" {
		t.Fatalf("legacy probe: ok=%v, response=%+v", ok, resp)
	}
	if ch.challenges.PendingCount() != 0 {
		t.Fatal("legacy probe retained a challenge")
	}
	// A missing version on the verification round must not consume a nonce.
	sendDealerRequest(t, dealer, protocol.Request{ConnectionID: legacyConnection, Type: protocol.RequestTypeSessionStart, Timestamp: time.Now().UTC().Format(time.RFC3339), ProtocolVersion: protocol.Version})
	if _, ok = recvDealerResponse(t, dealer, 2*time.Second); !ok {
		t.Fatal("missing challenge")
	}
	sendDealerRequest(t, dealer, protocol.Request{ConnectionID: legacyConnection, Type: protocol.RequestTypeSessionStart, Timestamp: time.Now().UTC().Format(time.RFC3339), AuthToken: string([]byte{1, 2, 3})})
	resp, ok = recvDealerResponse(t, dealer, 2*time.Second)
	if !ok || resp.ErrorCode != protocol.ErrorCodeProtocolMismatch {
		t.Fatalf("legacy verify: %+v", resp)
	}
	if ch.challenges.PendingCount() != 1 {
		t.Fatal("legacy verification consumed the issued nonce")
	}
}
