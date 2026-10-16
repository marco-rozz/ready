package main

import (
	"bytes"
	"errors"
	"testing"

	"github.com/marco-rozz/ready/internal/privacy"
	"github.com/marco-rozz/ready/internal/protocol"
	"github.com/urfave/cli/v3"
)

// TestFinalizeReply_LLMUnavailable pins the llm-status-visibility
// chat-tui-rendering scenario "Exec LLM outage returns failure": on the
// --exec and non-TTY stdout paths a final reply with ErrorCode
// llm_unavailable writes its Content to stderr and exits 1. The session is
// ended (these paths exit regardless — there is no session to keep open).
func TestFinalizeReply_LLMUnavailable(t *testing.T) {
	t.Parallel()

	const terminalMsg = "The AI service is temporarily unavailable. Please try again in a few minutes."

	var stdout, stderr bytes.Buffer
	ended := false

	err := finalizeReply(
		protocol.Response{
			Status:       protocol.ResponseStatusError,
			ResponseType: protocol.ResponseTypeReply,
			ErrorCode:    protocol.ErrorCodeLLMUnavailable,
			Content:      terminalMsg,
		},
		&stdout, &stderr,
		func() { ended = true },
		privacy.New(nil),
		false,
	)

	var exitErr cli.ExitCoder
	if !errors.As(err, &exitErr) {
		t.Fatalf("expected cli.ExitCoder, got %T: %v", err, err)
	}

	if got := exitErr.ExitCode(); got != ExitErr {
		t.Errorf("exit code = %d, want %d", got, ExitErr)
	}

	if stderr.String() != terminalMsg+"\n" {
		t.Errorf("stderr = %q, want the terminal message on stderr", stderr.String())
	}

	if stdout.Len() != 0 {
		t.Errorf("stdout = %q, want empty (an outage is never an answer)", stdout.String())
	}

	if !ended {
		t.Error("session_end was not sent before exiting")
	}
}

// TestFinalizeReply_OtherErrorsStillUseErrorCode guards the boundary: a
// generic error reply keeps the "error: <code>" stderr shape — only
// llm_unavailable renders its Content.
func TestFinalizeReply_OtherErrorsStillUseErrorCode(t *testing.T) {
	t.Parallel()

	var stdout, stderr bytes.Buffer

	err := finalizeReply(
		protocol.Response{
			Status:       protocol.ResponseStatusError,
			ResponseType: protocol.ResponseTypeReply,
			ErrorCode:    "malformed_request",
			Content:      "not rendered",
		},
		&stdout, &stderr,
		func() {},
		privacy.New(nil),
		false,
	)

	var exitErr cli.ExitCoder
	if !errors.As(err, &exitErr) {
		t.Fatalf("expected cli.ExitCoder, got %T: %v", err, err)
	}

	if got := exitErr.ExitCode(); got != ExitErr {
		t.Errorf("exit code = %d, want %d", got, ExitErr)
	}

	if stderr.String() != "error: malformed_request\n" {
		t.Errorf("stderr = %q, want the generic error shape", stderr.String())
	}
}

// TestIsModelStatusControl pins the non-TUI filter: unsolicited model-status
// control events (Status ok with model fields) are recognised so the
// line-reader/exec paths keep waiting instead of treating them as lifecycle
// control.
func TestIsModelStatusControl(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		resp protocol.Response
		want bool
	}{
		{
			"model-status event",
			protocol.Response{Status: protocol.ResponseStatusOK, ResponseType: protocol.ResponseTypeControl, ModelState: protocol.ModelStateFallback},
			true,
		},
		{
			"lifecycle error control",
			protocol.Response{Status: protocol.ResponseStatusError, ResponseType: protocol.ResponseTypeControl, ErrorCode: "no_active_session"},
			false,
		},
		{
			"control without model fields",
			protocol.Response{Status: protocol.ResponseStatusOK, ResponseType: protocol.ResponseTypeControl},
			false,
		},
		{
			"session_changed event (session-lifecycle)",
			protocol.Response{Status: protocol.ResponseStatusOK, ResponseType: protocol.ResponseTypeControl, Content: protocol.ControlSessionChanged, SessionID: "s"},
			true,
		},
		{
			"compacted event (session-lifecycle)",
			protocol.Response{Status: protocol.ResponseStatusOK, ResponseType: protocol.ResponseTypeControl, Content: protocol.ControlCompacted, Compaction: &protocol.CompactionInfo{TokensBefore: 2, TokensAfter: 1}},
			true,
		},
		{
			"shutdown control is lifecycle",
			protocol.Response{Status: protocol.ResponseStatusShutdown, ResponseType: protocol.ResponseTypeControl, Content: "daemon shutting down"},
			false,
		},
		{
			"ok reply with model fields (not control)",
			protocol.Response{Status: protocol.ResponseStatusOK, ResponseType: protocol.ResponseTypeReply, ModelState: protocol.ModelStateDefault},
			false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if got := isModelStatusControl(tc.resp); got != tc.want {
				t.Errorf("isModelStatusControl(%+v) = %v, want %v", tc.resp, got, tc.want)
			}
		})
	}
}
