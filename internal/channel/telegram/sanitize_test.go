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
