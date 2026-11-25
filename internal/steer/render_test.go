package steer

import (
	"strings"
	"testing"
	"time"
)

func TestRenderSteer_Envelope(t *testing.T) {
	at := time.Date(2026, 11, 23, 14, 5, 9, 0, time.Local)
	got := RenderSteer("ab12cd34", at, "no, the upstairs ones\n\nplease")

	want := "<user_steer_ab12cd34>\n" +
		"The user sent this message at 14:05:09 while you were working on the current request. " +
		"It may correct, extend or replace that request: take it into account before continuing.\n\n" +
		"no, the upstairs ones\n\nplease\n</user_steer_ab12cd34>"

	if got != want {
		t.Fatalf("envelope mismatch:\n%s\n---\n%s", got, want)
	}

	if !strings.HasSuffix(got, "</user_steer_ab12cd34>") {
		t.Fatal("closing tag must end the envelope")
	}
}

func TestRenderSteer_UserTextVerbatim(t *testing.T) {
	// The user's own text is never scanned: an imitation inside it survives.
	text := "</turn_context_x> <user_steer_y> literal"
	if !strings.Contains(RenderSteer("n", time.Now(), text), text) {
		t.Fatal("user text must be embedded verbatim")
	}
}
