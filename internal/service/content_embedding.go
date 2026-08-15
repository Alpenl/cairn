package service

import "strings"

// contentEmbeddingBodyRunes is the rune budget of the body fragment folded
// into a link's content-embedding input. Matches the Phase 7 retrieval budget
// (retrieveTextRunes) and the Spec's "正文片段（前 2000 runes）": enough signal
// to place the link in semantic space without inflating the embedding call.
const contentEmbeddingBodyRunes = 2000

// buildContentEmbeddingInput assembles the text embedded as a link's content
// vector: title, then summary, then the first contentEmbeddingBodyRunes runes
// of the body, each on its own line, blank parts skipped. This is the single
// source of truth for the embedding input shape used by parse-time writes and
// the optional LinkBackfiller. The default Runtime does not start that repair
// runner automatically.
//
// Note the input differs from the Phase 7 retrieval text (title + body only):
// the content vector additionally folds in the analyzer-produced summary,
// which only exists after analysis, so the two cannot share one embedding call
// — they are computed at different pipeline stages from different inputs.
func buildContentEmbeddingInput(title, summary, body string) string {
	parts := make([]string, 0, 3)
	if t := strings.TrimSpace(title); t != "" {
		parts = append(parts, t)
	}
	summaryTrim := strings.TrimSpace(summary)
	if summaryTrim != "" {
		parts = append(parts, summaryTrim)
	}
	// grok 直连路径把 Body 设为 Summary（充当正文信号、避免被误判 thin），此时 summary
	// 与 body 完全相同，原样拼接会让同一段文本进 embedding 两遍、语义被摘要重复加权。
	// 去重：body 与 summary 相同则不再追加。
	if b := strings.TrimSpace(truncateRunes(body, contentEmbeddingBodyRunes)); b != "" && b != summaryTrim {
		parts = append(parts, b)
	}
	return strings.Join(parts, "\n")
}

// truncateRunes returns the first n runes of s (the whole string when shorter).
// A non-positive n yields the empty string.
func truncateRunes(s string, n int) string {
	if n <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}
