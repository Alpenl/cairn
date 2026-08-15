package notetitle

import (
	"strings"
	"testing"
)

func TestDeriveUsesGFMTitleContract(t *testing.T) {
	base59 := strings.Repeat("a", 59)
	base60 := strings.Repeat("b", 60)
	base61 := strings.Repeat("c", 61)
	tests := []struct {
		name, content, want string
	}{
		{"h1 after prose", "ordinary prose\n\n# Final title", "Final title"},
		{"first of multiple h1", "# First\n\n# Second", "First"},
		{"setext h1", "Setext title\n============", "Setext title"},
		{"inline formatting", "# **Bold** and `code` [link](https://example.test)", "Bold and code link"},
		{"autolink", "# <https://example.test/path>", "https://example.test/path"},
		{"raw html", "# before <span>inside</span> after", "before <span>inside</span> after"},
		{"empty heading falls back", "#   \n\nfirst paragraph", "first paragraph"},
		{"non heading", "## Secondary\n\nbody", "Secondary"},
		{"fenced code fallback", "```text\nfirst code line\n```", "first code line"},
		{"blank", " \n\t\n", Untitled},
		{"emoji and combining", "# 😀 e\u0301", "😀 é"},
		{"59 code points", "# " + base59, base59},
		{"60 code points", "# " + base60, base60},
		{"61 code points", "# " + base61, base61[:60]},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := Derive(test.content); got != test.want {
				t.Fatalf("Derive(%q) = %q, want %q", test.content, got, test.want)
			}
		})
	}
}
