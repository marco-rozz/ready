package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/marco-rozz/ready/internal/protocol"
)

func TestChatProtocolVersionCurrentAgainstThree(t *testing.T) {
	var stderr bytes.Buffer
	err := checkProtocolVersion(&protocol.Response{ProtocolVersion: 3, ErrorCode: protocol.ErrorCodeProtocolMismatch}, &stderr)
	if err == nil || !strings.Contains(stderr.String(), fmt.Sprintf("client %d, daemon 3", protocol.Version)) || !strings.Contains(stderr.String(), "matching") {
		t.Fatalf("error=%v message=%q", err, stderr.String())
	}
	stderr.Reset()
	if err := checkProtocolVersion(&protocol.Response{ProtocolVersion: protocol.Version}, &stderr); err != nil || stderr.Len() != 0 {
		t.Fatalf("matching version: %v, %q", err, stderr.String())
	}
}

// Wire-protocol spec: on a round-1 protocol_mismatch the client prints one
// stderr line naming BOTH versions plus the matching-releases advice —
// never the generic "expected challenge" complaint.
func TestRound1Gate_MismatchNamesBothVersions(t *testing.T) {
	var stderr bytes.Buffer
	resp := protocol.Response{
		Status:          protocol.ResponseStatusError,
		ErrorCode:       protocol.ErrorCodeProtocolMismatch,
		ProtocolVersion: protocol.Version - 1,
	}

	err := round1Gate(&resp, &stderr)

	out := stderr.String()
	if err == nil {
		t.Fatal("mismatch must exit with an error")
	}
	if !strings.Contains(out, fmt.Sprintf("client %d", protocol.Version)) ||
		!strings.Contains(out, fmt.Sprintf("daemon %d", protocol.Version-1)) {
		t.Fatalf("both versions not named: %q", out)
	}
	if !strings.Contains(out, "matching") {
		t.Fatalf("matching-releases advice missing: %q", out)
	}
	if strings.Contains(out, "expected challenge") {
		t.Fatalf("mismatch reached the challenge complaint: %q", out)
	}
}

// Wire-protocol spec: a round-1 error reply WITHOUT protocol_version is
// reported as a daemon that predates protocol versioning.
func TestRound1Gate_ErrorWithoutVersionPredatesVersioning(t *testing.T) {
	var stderr bytes.Buffer
	resp := protocol.Response{Status: protocol.ResponseStatusError, ErrorCode: "auth_fail"}

	err := round1Gate(&resp, &stderr)

	out := stderr.String()
	if err == nil {
		t.Fatal("version-less error reply must exit with an error")
	}
	if !strings.Contains(out, protocol.PredatesVersioningHint) || !strings.Contains(out, "matching") {
		t.Fatalf("predates-versioning report missing: %q", out)
	}
	if strings.Contains(out, "expected challenge") {
		t.Fatalf("version-less error reached the challenge complaint: %q", out)
	}
}

// A version-correct challenge reply passes the gate untouched.
func TestRound1Gate_ChallengePasses(t *testing.T) {
	var stderr bytes.Buffer
	resp := protocol.Response{
		Status:          protocol.ResponseStatusOK,
		Content:         "nonce",
		ErrorCode:       challenge,
		ProtocolVersion: protocol.Version,
	}

	if err := round1Gate(&resp, &stderr); err != nil || stderr.Len() != 0 {
		t.Fatalf("challenge reply gated: err=%v stderr=%q", err, stderr.String())
	}
}

// Wire-protocol spec: a round-1 reply that cannot be decoded is reported as
// a daemon that predates protocol versioning (one stderr line, generic exit
// 1), never as a raw parse error.
func TestHandleHandshakeRecvError_UndecodablePredatesVersioning(t *testing.T) {
	var stderr bytes.Buffer
	wrapped := fmt.Errorf("%w: unknown field \"legacy_field\"", errUndecodableReply)

	err := handleHandshakeRecvError(&stderr, wrapped, make(chan struct{}))

	out := stderr.String()
	if err == nil {
		t.Fatal("undecodable reply must exit with an error")
	}
	if !strings.Contains(out, protocol.PredatesVersioningHint) || !strings.Contains(out, "matching") {
		t.Fatalf("predates-versioning report missing: %q", out)
	}
	if strings.Contains(out, "unknown field") {
		t.Fatalf("raw parse error leaked: %q", out)
	}
}

// Other handshake receive errors keep their existing handling — the
// undecodable branch must not swallow them.
func TestHandleHandshakeRecvError_KeepsExistingExits(t *testing.T) {
	var stderr bytes.Buffer
	err := handleHandshakeRecvError(&stderr, fmt.Errorf("wrapped: %w", context.DeadlineExceeded), make(chan struct{}))
	if err == nil || stderr.String() != "timeout\n" {
		t.Fatalf("deadline handling changed: err=%v stderr=%q", err, stderr.String())
	}

	stderr.Reset()
	err = handleHandshakeRecvError(&stderr, errors.New("socket closed"), make(chan struct{}))
	if err == nil || !strings.Contains(stderr.String(), "socket closed") {
		t.Fatalf("generic error handling changed: err=%v stderr=%q", err, stderr.String())
	}
}
