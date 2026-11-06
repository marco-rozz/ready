package main

import (
	"bytes"
	"strings"
	"testing"
)

const rejectedChatProfileName = "admin"

func TestChatRejectsProfileFlag(t *testing.T) {
	var output bytes.Buffer
	command := newRootCommand(&output)
	command.Writer = &output
	err := command.Run(t.Context(), []string{homeAgent, chatLabel, "--profile", rejectedChatProfileName})
	if err == nil || !strings.Contains(err.Error(), "flag provided but not defined") || !strings.Contains(err.Error(), "profile") {
		t.Fatalf("chat must reject the server-only profile flag: %v", err)
	}
}
