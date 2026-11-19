package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/marco-rozz/ready/internal/protocol"
)

func TestChatProtocolVersionFourAgainstThree(t *testing.T) {
	var stderr bytes.Buffer
	err := checkProtocolVersion(&protocol.Response{ProtocolVersion: 3, ErrorCode: protocol.ErrorCodeProtocolMismatch}, &stderr)
	if err == nil || !strings.Contains(stderr.String(), "client 4, daemon 3") || !strings.Contains(stderr.String(), "matching") {
		t.Fatalf("error=%v message=%q", err, stderr.String())
	}
	stderr.Reset()
	if err := checkProtocolVersion(&protocol.Response{ProtocolVersion: 4}, &stderr); err != nil || stderr.Len() != 0 {
		t.Fatalf("matching version: %v, %q", err, stderr.String())
	}
}
