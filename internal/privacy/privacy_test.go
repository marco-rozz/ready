package privacy

import "testing"

func TestSanitizer_Redact(t *testing.T) {
	s := New(map[string]string{
		"/Users/mr":            "~",
		"/Users/mr/app/.ready": "~/.ready",
		"":                     "ignored",
	})

	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "redacts absolute home path",
			in:   "The skill path is /Users/mr/Gitrepo/agent-home/.ready/skills/home-topology.",
			want: "The skill path is ~/Gitrepo/agent-home/.ready/skills/home-topology.",
		},
		{
			name: "longest prefix wins",
			in:   "/Users/mr/app/.ready/checkpoints",
			want: "~/.ready/checkpoints",
		},
		{
			name: "does not touch MCP tool names",
			in:   "Calling light/turn_on then switch/toggle.",
			want: "Calling light/turn_on then switch/toggle.",
		},
		{
			name: "does not touch unrelated absolute paths",
			in:   "See /etc/hosts and /var/log/system.log",
			want: "See /etc/hosts and /var/log/system.log",
		},
		{
			name: "empty input",
			in:   "",
			want: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := s.Redact(tt.in); got != tt.want {
				t.Errorf("Redact(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestSanitizer_NilIsNoOp(t *testing.T) {
	var s *Sanitizer
	const in = "/Users/mr/secret"
	if got := s.Redact(in); got != in {
		t.Errorf("nil Sanitizer.Redact(%q) = %q, want unchanged", in, got)
	}
}

func TestFromHome_NeverNil(t *testing.T) {
	if s := FromHome(nil); s == nil {
		t.Fatal("FromHome(nil) returned nil")
	}
}
