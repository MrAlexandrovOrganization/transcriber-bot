package tgfmt

import "testing"

func TestEscape(t *testing.T) {
	if got := Escape("a < b & c"); got != "a &lt; b &amp; c" {
		t.Fatalf("Escape() = %q", got)
	}
}

func TestBlockquote(t *testing.T) {
	if got := Blockquote(Escape("текст <записи>")); got != "<blockquote>текст &lt;записи&gt;</blockquote>" {
		t.Fatalf("Blockquote() = %q", got)
	}
}
