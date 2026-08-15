package fetcher

import "testing"

func TestLimitTextByRunesTrimsAndPreservesWholeCharacters(t *testing.T) {
	t.Parallel()

	got := limitTextByRunes(" 你好世界再见 ", 4)
	if got != "你好世界" {
		t.Fatalf("limitTextByRunes() = %q, want 你好世界", got)
	}
}
