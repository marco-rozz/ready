package telegram

import (
	"testing"
)

func TestSanitizeHTML_AllowedTagsPreserved(t *testing.T) {
	input := `✅ <b>Luce cucina</b> spenta — <i>stato</i>: <code>off</code>`
	got := sanitizeHTML(input)

	if got != input {
		t.Errorf("allowed tags should be preserved:\n  got:  %q\n  want: %q", got, input)
	}
}

func TestSanitizeHTML_UnknownTagsEscaped(t *testing.T) {
	input := `Use <div>this</div> and <script>alert(1)</script>`
	got := sanitizeHTML(input)
	want := `Use &lt;div&gt;this&lt;/div&gt; and &lt;script&gt;alert(1)&lt;/script&gt;`

	if got != want {
		t.Errorf("unknown tags should be escaped:\n  got:  %q\n  want: %q", got, want)
	}
}

func TestSanitizeHTML_BareAngleBracketsEscaped(t *testing.T) {
	input := `temperature 18 < 22 > 15`
	got := sanitizeHTML(input)
	want := `temperature 18 &lt; 22 &gt; 15`

	if got != want {
		t.Errorf("bare angle brackets should be escaped:\n  got:  %q\n  want: %q", got, want)
	}
}

func TestSanitizeHTML_AmpersandEscaped(t *testing.T) {
	input := `Tom & Jerry`
	got := sanitizeHTML(input)
	want := `Tom &amp; Jerry`

	if got != want {
		t.Errorf("ampersand should be escaped:\n  got:  %q\n  want: %q", got, want)
	}
}

func TestSanitizeHTML_MixedContent(t *testing.T) {
	input := `<b>OK</b> value < 100 & <i>running</i>`
	got := sanitizeHTML(input)
	want := `<b>OK</b> value &lt; 100 &amp; <i>running</i>`

	if got != want {
		t.Errorf("mixed content:\n  got:  %q\n  want: %q", got, want)
	}
}

func TestSanitizeHTML_AnchorTag(t *testing.T) {
	input := `Visit <a href="https://example.com">here</a> now`
	got := sanitizeHTML(input)

	if got != input {
		t.Errorf("anchor tag should be preserved:\n  got:  %q\n  want: %q", got, input)
	}
}

func TestSanitizeHTML_PreCodeBlock(t *testing.T) {
	input := "<pre><code class=\"json\">{\"a\":1}</code></pre>"
	got := sanitizeHTML(input)

	if got != input {
		t.Errorf("pre/code should be preserved:\n  got:  %q\n  want: %q", got, input)
	}
}

func TestSanitizeHTML_EmptyString(t *testing.T) {
	got := sanitizeHTML("")
	if got != "" {
		t.Errorf("expected empty string, got %q", got)
	}
}

func TestStripHTMLTags(t *testing.T) {
	input := `✅ <b>Luce cucina</b> spenta — <i>stato</i>: <code>off</code>`
	got := stripHTMLTags(input)
	want := `✅ Luce cucina spenta — stato: off`

	if got != want {
		t.Errorf("strip tags:\n  got:  %q\n  want: %q", got, want)
	}
}

func TestSanitizeHTML_BareSpanEscaped(t *testing.T) {
	// Telegram accepts <span> only in the tg-spoiler form: a bare opener is
	// escaped like any unknown tag, and its closer (kept by the pattern to
	// serve real spoiler spans) is dropped as a stray by the balancing step.
	input := `<span>highlighted</span> text`
	got := sanitizeHTML(input)
	want := `&lt;span&gt;highlighted text`

	if got != want {
		t.Errorf("bare span should be escaped:\n  got:  %q\n  want: %q", got, want)
	}
}

func TestSanitizeHTML_SpoilerSpanPreserved(t *testing.T) {
	input := `Result: <span class="tg-spoiler">secret</span>`
	got := sanitizeHTML(input)

	if got != input {
		t.Errorf("tg-spoiler span should be preserved:\n  got:  %q\n  want: %q", got, input)
	}
}

func TestBalanceTags_BalancedUnchanged(t *testing.T) {
	input := `✅ <b>Luce</b> <i>spenta</i>, <code>off</code>`

	if got := balanceTags(input); got != input {
		t.Errorf("balanced input must be a no-op:\n  got:  %q", got)
	}
}

func TestBalanceTags_UnclosedOpenerClosed(t *testing.T) {
	// The streaming-chunk failure observed on 2026-10-20: the chunk ends
	// before the model emitted the closing tag.
	input := `✅ <b>Luce soggiorno`
	got := balanceTags(input)
	want := `✅ <b>Luce soggiorno</b>`

	if got != want {
		t.Errorf("unclosed opener should be closed:\n  got:  %q\n  want: %q", got, want)
	}
}

func TestBalanceTags_UnclosedOpenersClosedInReverse(t *testing.T) {
	input := `<b><i>text`
	got := balanceTags(input)
	want := `<b><i>text</i></b>`

	if got != want {
		t.Errorf("nested openers closed in reverse order:\n  got:  %q\n  want: %q", got, want)
	}
}

func TestBalanceTags_StrayCloserDropped(t *testing.T) {
	// The second half of the same split: a chunk starting after the boundary
	// carries a closing tag with no opener.
	input := `</b> spenta`
	got := balanceTags(input)
	want := ` spenta`

	if got != want {
		t.Errorf("stray closer should be dropped:\n  got:  %q\n  want: %q", got, want)
	}
}

func TestBalanceTags_TruncatedMidTag(t *testing.T) {
	// The 4096 truncation can cut inside a kept tag.
	input := `text <a href="https://example.com/very/long/url-that-got`
	got := balanceTags(input)
	want := `text `

	if got != want {
		t.Errorf("incomplete trailing tag should be removed:\n  got:  %q\n  want: %q", got, want)
	}
}

func TestBalanceTags_TruncatedMidEntity(t *testing.T) {
	input := `Tom &amp`
	got := balanceTags(input)
	want := `Tom `

	if got != want {
		t.Errorf("incomplete trailing entity should be removed:\n  got:  %q\n  want: %q", got, want)
	}
}

func TestBalanceTags_TruncatedAfterCompleteTag(t *testing.T) {
	// The truncation lands exactly after a complete opener: it survives and
	// gets closed.
	input := `text <b>`
	got := balanceTags(input)
	want := `text <b></b>`

	if got != want {
		t.Errorf("complete opener at the cut should survive, closed:\n  got:  %q\n  want: %q", got, want)
	}
}
