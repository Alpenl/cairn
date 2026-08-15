package textutil

import (
	"net/url"
	"path"
	"strings"
)

// MaxTitleRunes is the persisted display-title ceiling. The analyzer prompt
// asks the model to stay below the same limit, while ChooseTitle enforces it
// when a provider returns a page title or an entire social post instead.
const MaxTitleRunes = 36

// genericTitleSet enumerates titles that the pipeline / analyzer / fetcher
// treat as "no useful title". Both the source-specific pre-fetch fallback
// (try ogp:title etc.) and the post-fetch low-confidence heuristic check
// against this list, so it lives in textutil to keep the three call sites
// in lockstep.
var genericTitleSet = map[string]struct{}{
	"home":      {},
	"homepage":  {},
	"index":     {},
	"untitled":  {},
	"404":       {},
	"403":       {},
	"not found": {},
	"x":         {},
	"twitter":   {},
}

// IsGenericTitle reports whether value is empty / whitespace-only or matches
// one of the well-known placeholder titles a CMS or SPA shell typically
// emits when no real per-page title is set. Comparison is case-insensitive
// and whitespace-tolerant.
func IsGenericTitle(value string) bool {
	normalized := strings.ToLower(NormalizeTitle(value))
	if normalized == "" {
		return true
	}
	_, ok := genericTitleSet[normalized]
	return ok
}

// NormalizeTitle removes model/page formatting without changing the title's
// meaning. It intentionally does not truncate; callers can still distinguish
// a useful short title from a page title that contains the complete body.
func NormalizeTitle(value string) string {
	value = strings.Join(strings.Fields(value), " ")
	value = strings.TrimSpace(strings.Trim(value, "#*`"))
	for _, prefix := range []string{"标题：", "标题:"} {
		value = strings.TrimSpace(strings.TrimPrefix(value, prefix))
	}
	for _, suffix := range []string{" / X", " | X", " - X", " on X", " / Twitter", " | Twitter", " - Twitter"} {
		if len(value) >= len(suffix) && strings.EqualFold(value[len(value)-len(suffix):], suffix) {
			value = strings.TrimSpace(value[:len(value)-len(suffix)])
			break
		}
	}
	return strings.TrimSpace(value)
}

// IsConciseTitle reports whether a title is meaningful and fits the product's
// display-title budget. Long source titles are not discarded outright: they
// remain available to ChooseTitle after summary and URL fallbacks.
func IsConciseTitle(value string) bool {
	value = NormalizeTitle(value)
	return !IsGenericTitle(value) && Count(value) <= MaxTitleRunes
}

// ChooseTitle selects the user-facing title from analyzer output and source
// metadata. Generated titles win when concise. A long provider/page title is
// treated as content rather than a title, so the summary becomes the next
// semantic source before any deterministic truncation or URL fallback.
func ChooseTitle(generated, source, summary, rawURL string) string {
	generated = NormalizeTitle(generated)
	if IsConciseTitle(generated) {
		return generated
	}

	source = NormalizeTitle(source)
	if IsConciseTitle(source) {
		return source
	}

	if candidate := titleFromSummary(summary); candidate != "" {
		return candidate
	}
	for _, candidate := range []string{generated, source} {
		if !IsGenericTitle(candidate) {
			return clampTitle(candidate)
		}
	}
	if candidate := titleFromURL(rawURL); candidate != "" {
		return candidate
	}
	return "未命名内容"
}

func titleFromSummary(summary string) string {
	candidate := NormalizeTitle(summary)
	for _, prefix := range []string{
		"这篇文章主要介绍", "这篇文章介绍", "这篇文章讨论", "这篇文章分析",
		"本文主要介绍", "本文介绍", "本文讨论", "本文分析",
		"文章主要介绍", "文章介绍", "文章讨论", "文章分析",
		"该内容介绍", "该内容讨论", "该项目是", "这条动态介绍",
	} {
		if strings.HasPrefix(candidate, prefix) {
			candidate = strings.TrimSpace(strings.TrimLeft(strings.TrimPrefix(candidate, prefix), "：:，,"))
			break
		}
	}

	for _, copula := range []string{" 是一个", " 是一款", " 是个"} {
		if index := strings.Index(candidate, copula); index > 0 {
			candidate = strings.TrimSpace(candidate[:index]) + "：" + strings.TrimSpace(candidate[index+len(copula):])
			break
		}
	}
	if index := strings.IndexAny(candidate, "。！？!?；;\n"); index >= 0 {
		candidate = candidate[:index]
	}
	candidate = strings.TrimSpace(strings.Trim(candidate, "，,。！？!?；;：:"))
	if IsGenericTitle(candidate) {
		return ""
	}
	return clampTitle(candidate)
}

func titleFromURL(rawURL string) string {
	parsed, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return ""
	}
	host := strings.TrimPrefix(strings.ToLower(parsed.Hostname()), "www.")
	segments := strings.FieldsFunc(strings.Trim(parsed.EscapedPath(), "/"), func(r rune) bool { return r == '/' })
	if (host == "x.com" || host == "twitter.com") && len(segments) > 0 {
		name, err := url.PathUnescape(segments[0])
		if err == nil && strings.TrimSpace(name) != "" {
			return clampTitle("@" + strings.TrimSpace(name) + " 的 X 动态")
		}
	}
	if base := path.Base(strings.Trim(parsed.Path, "/")); base != "." && base != "" && base != "/" {
		if decoded, err := url.PathUnescape(base); err == nil {
			decoded = strings.NewReplacer("-", " ", "_", " ").Replace(decoded)
			if !IsGenericTitle(decoded) {
				return clampTitle(decoded)
			}
		}
	}
	return clampTitle(host)
}

func clampTitle(value string) string {
	value = NormalizeTitle(value)
	if Count(value) <= MaxTitleRunes {
		return value
	}
	runes := []rune(value)
	minimum := MaxTitleRunes / 2
	for index := MaxTitleRunes - 1; index >= minimum; index-- {
		switch runes[index] {
		case ' ', '\t', '，', ',', '：', ':', '；', ';', '|', '｜', '—':
			if candidate := strings.TrimSpace(strings.Trim(string(runes[:index]), "，,：:；;|｜—")); candidate != "" {
				return candidate
			}
		}
	}
	return strings.TrimSpace(string(runes[:MaxTitleRunes-1])) + "…"
}
