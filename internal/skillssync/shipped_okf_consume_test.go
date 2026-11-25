package skillssync

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/rs/zerolog"

	"github.com/marco-rozz/ready/internal/config"
	"github.com/marco-rozz/ready/internal/skills"
)

func TestShippedLibraryIncludesOKFConsume(t *testing.T) {
	rt := t.TempDir()
	if err := Sync(context.Background(), rt, "../../resources/skills", "auto", zerolog.Nop()); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"SKILL.md", "LICENSE", "NOTICE", "references/trust.md"} {
		if _, err := os.Stat(filepath.Join(rt, "okf-consume", f)); err != nil {
			t.Fatalf("%s: %v", f, err)
		}
	}
	m, err := skills.NewManager(context.Background(), &config.SkillsConfig{Path: rt}, zerolog.Nop())
	if err != nil {
		t.Fatal(err)
	}
	list, err := m.ListSkills(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, si := range list {
		if si.Name == "okf-consume" {
			found = true
		}
	}
	if !found {
		t.Fatalf("okf-consume not discovered: %+v", list)
	}
}
