package integration

import (
	"path/filepath"
	"testing"

	"github.com/flexigpt/llmtools-go/fstool"
	"github.com/flexigpt/llmtools-go/texttool"
)

// Read/modify loop + disambiguated replacements + insert/delete.

const (
	textTestDocPath               = "doc.md"
	textTestQueryTypeSubstring    = "substring"
	textTestInsertPositionBetween = "between"
	textTestFindQuery             = "TODO:"
	textTestIntroLine             = "Intro line"
	textTestInsertTextAbove       = "<!-- A END -->\n\n"
	textTestInsertTextBelow       = "## Section B"
	textTestInsertText            = "\nInserted after A"
	textTestInitialDoc            = `# Title
Intro line

## Section A
<!-- A START -->
TODO: old
<!-- A END -->

## Section B
<!-- B START -->
TODO: old
<!-- B END -->
`
	textTestExpectedDoc = `# Title

## Section A
<!-- A START -->
TODO: old
<!-- A END -->

Inserted after A

## Section B
<!-- B START -->
TODO: new
<!-- B END -->
`
)

func TestE2E_Text_ReadModifyLoop(t *testing.T) {
	base := t.TempDir()
	h := newHarness(t, base)

	docRel := textTestDocPath
	docAbs := filepath.Join(base, docRel)

	// 1) Create a file via writefile (end-to-end).
	_ = callJSON[fstool.WriteFileOut](t, h.r, integrationToolSlugWriteFile, fstool.WriteFileArgs{
		Path:          docRel,
		Encoding:      integrationEncodingText,
		Content:       textTestInitialDoc,
		Overwrite:     false,
		CreateParents: false,
	})

	// 2) Read a bounded range (marker-to-marker) to show how to constrain reads.
	rng := callJSON[texttool.ReadTextRangeOut](t, h.r, "readtextrange", texttool.ReadTextRangeArgs{
		Path:      docRel,
		StartLine: new(10),
		LineCount: new(3),
	})
	if rng.LinesReturned == 0 {
		t.Fatalf("expected some lines in range, got: %s", debugJSON(t, rng))
	}

	// 3) Find occurrences (substring) with context.
	found := callJSON[texttool.FindTextOut](t, h.r, "findtext", texttool.FindTextArgs{
		Path:         docRel,
		QueryType:    textTestQueryTypeSubstring,
		Query:        textTestFindQuery,
		ContextLines: 1,
		MaxMatches:   10,
	})
	if found.MatchesReturned < 2 {
		t.Fatalf("expected 2 TODO matches, got: %s", debugJSON(t, found))
	}

	// 4) Replace only the TODO in Section B using beforeLines/afterLines disambiguation.
	one := 1
	_ = callJSON[texttool.ReplaceTextOut](t, h.r, "replacetext", texttool.ReplaceTextArgs{
		Path:          docRel,
		TextAbove:     new("<!-- B START -->"),
		OldText:       new("TODO: old"),
		TextBelow:     new("<!-- B END -->"),
		NewText:       new("TODO: new"),
		ExpectedCount: &one,
	})

	// 5) Insert after a uniquely-matched anchor.
	_ = callJSON[texttool.InsertTextOut](t, h.r, "inserttext", texttool.InsertTextArgs{
		Path:      docRel,
		Position:  textTestInsertPositionBetween,
		TextAbove: new(textTestInsertTextAbove),
		TextBelow: new(textTestInsertTextBelow),
		Text:      new(textTestInsertText),
	})

	// 6) Delete a line block (exact match).
	_ = callJSON[texttool.DeleteTextOut](t, h.r, "deletetext", texttool.DeleteTextArgs{
		Path:          docRel,
		OldText:       new(textTestIntroLine),
		ExpectedCount: new(1),
	})

	// 7) Verify final content via readfile.
	out := callRaw(t, h.r, integrationToolSlugReadFile, fstool.ReadFileArgs{
		Path:     docRel,
		Encoding: integrationEncodingText,
	})
	got := requireSingleTextOutput(t, out)

	if got != textTestExpectedDoc {
		t.Fatalf("final doc mismatch\npath=%s\n--- got ---\n%s\n--- want ---\n%s", docAbs, got, textTestExpectedDoc)
	}
}
