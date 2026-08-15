package textutil

import "testing"

func TestCount(t *testing.T) {
	cases := []struct {
		in   string
		want int
	}{
		{"", 0},
		{"abc", 3},
		{"你好", 2},
		{"a你b好c", 5},
	}
	for _, c := range cases {
		if got := Count(c.in); got != c.want {
			t.Fatalf("Count(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}

func TestTruncate(t *testing.T) {
	if got := Truncate("hello world", 5); got != "hello" {
		t.Fatalf("Truncate ascii = %q", got)
	}
	if got := Truncate("你好世界再见", 4); got != "你好世界" {
		t.Fatalf("Truncate runes = %q", got)
	}
	if got := Truncate("short", 100); got != "short" {
		t.Fatalf("Truncate fits unchanged = %q", got)
	}
	if got := Truncate("anything", 0); got != "" {
		t.Fatalf("Truncate(_,0) = %q, want empty", got)
	}
	if got := Truncate("anything", -3); got != "" {
		t.Fatalf("Truncate(_,negative) = %q, want empty", got)
	}
}

func TestLimitTextByRunes(t *testing.T) {
	if got := LimitTextByRunes("  hello  ", 3); got != "hel" {
		t.Fatalf("LimitTextByRunes trim+truncate = %q", got)
	}
	if got := LimitTextByRunes("   ", 4); got != "" {
		t.Fatalf("LimitTextByRunes whitespace-only = %q, want empty", got)
	}
	if got := LimitTextByRunes(" 你好世界再见 ", 4); got != "你好世界" {
		t.Fatalf("LimitTextByRunes runes = %q", got)
	}
}
