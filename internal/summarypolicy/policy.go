// Package summarypolicy owns the product-level shape of human-readable
// summaries. The analyzer uses it to build prompts and the eval harness uses
// the same profiles to score output, keeping production and evaluation aligned.
package summarypolicy

import (
	"fmt"
	"net/url"
	"strings"
	"unicode"
)

// Input contains the content signals available before analysis.
type Input struct {
	URL         string
	ContentType string
	FetcherType string
}

// Profile describes the observable summary shape for one kind of content.
type Profile struct {
	Name         string
	MinRunes     int
	MaxRunes     int
	MinSentences int
	MaxSentences int
	AllowBullets bool
	MaxBullets   int
	Focus        string
}

// Assessment is the deterministic, audit-friendly summary shape score used by
// eval runs. Semantic fidelity is intentionally left to the LLM judge.
type Assessment struct {
	Score       float64
	LengthRunes int
	Issues      []string
}

// For returns a complete profile for the supplied content signals.
func For(in Input) Profile {
	if isSocialPost(in.URL) {
		return Profile{
			Name:         "social",
			MinRunes:     50,
			MaxRunes:     120,
			MinSentences: 1,
			MaxSentences: 2,
			AllowBullets: false,
			Focus:        "说明短帖分享的对象、用途和最关键的一两项信息，不把短帖扩写成文章",
		}
	}
	if isProject(in) {
		return Profile{
			Name:         "project",
			MinRunes:     60,
			MaxRunes:     120,
			MinSentences: 2,
			MaxSentences: 3,
			AllowBullets: false,
			Focus:        "说明项目是什么、解决什么问题，以及最关键的能力或差异",
		}
	}
	if isDigest(in.URL) {
		return Profile{
			Name:         "digest",
			MinRunes:     90,
			MaxRunes:     160,
			MinSentences: 2,
			MaxSentences: 3,
			AllowBullets: false,
			Focus:        "把周刊或资讯合集的整个条目作为摘要对象：概括开篇主内容，并明确覆盖至少2个其他栏目或类别。这是硬性覆盖要求，摘要不能只复述头条",
		}
	}
	contentType := strings.ToLower(strings.TrimSpace(in.ContentType))
	if contentType == "homepage" {
		return Profile{
			Name:         "homepage",
			MinRunes:     40,
			MaxRunes:     90,
			MinSentences: 1,
			MaxSentences: 2,
			AllowBullets: false,
			Focus:        "说明网站提供什么、适合谁，以及主要内容范围",
		}
	}
	if contentType == "listing" {
		return Profile{
			Name:         "listing",
			MinRunes:     40,
			MaxRunes:     90,
			MinSentences: 1,
			MaxSentences: 2,
			AllowBullets: false,
			Focus:        "说明栏目主题、覆盖的内容范围和适合的读者，不虚构统一结论",
		}
	}
	if contentType == "article" {
		return Profile{
			Name:         "article",
			MinRunes:     100,
			MaxRunes:     180,
			MinSentences: 2,
			MaxSentences: 4,
			AllowBullets: true,
			MaxBullets:   3,
			Focus:        "先说核心观点和最终结论或决定；若信息分布在多个章节，从全文选择最关键的事实、依据和可执行建议，不要只复述开头或时间线；原文有总结清单时压缩保留至少4项不同的具体建议",
		}
	}
	return Profile{
		Name:         "general",
		MinRunes:     70,
		MaxRunes:     140,
		MinSentences: 1,
		MaxSentences: 3,
		AllowBullets: false,
		Focus:        "直接说明内容主题、最有用的信息和可确认的结论",
	}
}

func isDigest(rawURL string) bool {
	parsed, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return false
	}
	path := strings.ToLower(strings.TrimRight(parsed.Path, "/"))
	if !strings.Contains(path, "/weekly/") {
		return false
	}
	lastSlash := strings.LastIndexByte(path, '/')
	last := path[lastSlash+1:]
	return strings.HasPrefix(last, "issue-") &&
		(strings.HasSuffix(last, ".md") || strings.HasSuffix(last, ".markdown"))
}

func isSocialPost(rawURL string) bool {
	parsed, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || parsed.Hostname() == "" {
		return false
	}
	host := strings.TrimPrefix(strings.ToLower(parsed.Hostname()), "www.")
	return host == "x.com" || strings.HasSuffix(host, ".x.com") ||
		host == "twitter.com" || strings.HasSuffix(host, ".twitter.com")
}

// PromptInstructions renders the profile as concise model guidance.
func (p Profile) PromptInstructions() string {
	bulletRule := "不要使用项目符号，写成连贯自然段。"
	if p.AllowBullets {
		bulletRule = fmt.Sprintf("仅当原文确有至少3个并列事实或时间节点时，才可使用最多%d个项目符号；否则写成自然段。", p.MaxBullets)
	}
	return fmt.Sprintf(
		"中文，%d-%d字，使用%d-%d个自然、完整的句子；%s。%s",
		p.MinRunes,
		p.MaxRunes,
		p.MinSentences,
		p.MaxSentences,
		p.Focus,
		bulletRule,
	)
}

// Clean removes model formatting and known report-template labels while
// preserving the factual prose that follows them.
func Clean(text string) string {
	text = strings.TrimSpace(text)
	if text == "" {
		return ""
	}
	text = strings.NewReplacer(
		"**", "",
		"__", "",
		"核心结论是", "文章认为",
		"明确结论/结果是", "文章认为",
		"明确的结论/结果是", "文章认为",
	).Replace(text)

	lines := make([]string, 0, strings.Count(text, "\n")+1)
	for _, rawLine := range strings.Split(text, "\n") {
		line := strings.TrimSpace(rawLine)
		if line == "" {
			continue
		}
		line = strings.TrimSpace(strings.TrimLeft(line, "#"))
		line = stripTemplateLabel(line)
		if line != "" {
			lines = append(lines, line)
		}
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}

func stripTemplateLabel(line string) string {
	for _, label := range []string{
		"背景/问题",
		"关键过程",
		"明确结论/结果",
		"明确的结论/结果",
		"核心结论",
	} {
		if !strings.HasPrefix(line, label) {
			continue
		}
		remainder := strings.TrimSpace(strings.TrimPrefix(line, label))
		if strings.HasPrefix(remainder, "：") || strings.HasPrefix(remainder, ":") {
			return strings.TrimSpace(strings.TrimLeft(remainder, "：:"))
		}
	}
	return line
}

// Conform applies profile-specific formatting guarantees after generic
// cleaning. Profiles that require prose cannot leak model-generated list
// markers or line-oriented report formatting into the stored summary.
func Conform(text string, profile Profile) string {
	text = Clean(text)
	if text == "" || profile.AllowBullets {
		return text
	}

	lines := make([]string, 0, strings.Count(text, "\n")+1)
	for _, rawLine := range strings.Split(text, "\n") {
		line := stripListMarker(strings.TrimSpace(rawLine))
		if line != "" {
			lines = append(lines, line)
		}
	}
	return strings.TrimSpace(strings.Join(lines, " "))
}

func stripListMarker(line string) string {
	for _, marker := range []string{"- ", "* ", "+ ", "• "} {
		if strings.HasPrefix(line, marker) {
			return strings.TrimSpace(strings.TrimPrefix(line, marker))
		}
	}

	digitEnd := 0
	for digitEnd < len(line) && line[digitEnd] >= '0' && line[digitEnd] <= '9' {
		digitEnd++
	}
	if digitEnd > 0 && digitEnd < len(line) {
		remainder := line[digitEnd:]
		for _, marker := range []string{". ", ".", "、", ") ", ")"} {
			if strings.HasPrefix(remainder, marker) {
				return strings.TrimSpace(strings.TrimPrefix(remainder, marker))
			}
		}
	}
	return line
}

// Clamp bounds a cleaned summary without leaving a dangling partial sentence
// when a usable natural boundary exists. The returned value never exceeds
// maxRunes and uses an ellipsis only for the hard-fallback path.
func Clamp(text string, maxRunes int) string {
	text = Clean(text)
	if text == "" || maxRunes <= 0 {
		return ""
	}
	runes := []rune(text)
	if len(runes) <= maxRunes {
		return text
	}

	lateBoundary := maxRunes * 3 / 5
	if candidate := findClampCandidate(runes, maxRunes-1, lateBoundary, isNaturalBoundary); candidate != "" {
		return normalizeClampEnding(candidate)
	}
	if candidate := findClampCandidate(runes, maxRunes-1, lateBoundary, func(runes []rune, idx int) bool {
		return runes[idx] == '，'
	}); candidate != "" {
		return normalizeClampEnding(candidate)
	}
	if candidate := findClampCandidate(runes, lateBoundary-1, maxRunes/4, isNaturalBoundary); candidate != "" {
		return normalizeClampEnding(candidate)
	}

	if maxRunes == 1 {
		return "…"
	}
	return strings.TrimSpace(string(runes[:maxRunes-1])) + "…"
}

func findClampCandidate(runes []rune, start, minimum int, match func([]rune, int) bool) string {
	if start >= len(runes) {
		start = len(runes) - 1
	}
	if minimum < 0 {
		minimum = 0
	}
	for idx := start; idx >= minimum; idx-- {
		if !match(runes, idx) {
			continue
		}
		end := idx + 1
		if runes[idx] == '\n' {
			end = idx
		}
		if candidate := strings.TrimSpace(string(runes[:end])); candidate != "" {
			return candidate
		}
	}
	return ""
}

func normalizeClampEnding(text string) string {
	runes := []rune(text)
	if len(runes) == 0 {
		return text
	}
	switch runes[len(runes)-1] {
	case '；', ';', '，':
		runes[len(runes)-1] = '。'
	}
	return string(runes)
}

// Assess evaluates whether text follows a profile's human-readable shape.
func Assess(text string, profile Profile) Assessment { //nolint:gocyclo // 按档位逐条评估摘要合规性，分支与规格文档一一对应
	text = strings.TrimSpace(text)
	assessment := Assessment{Score: 1, LengthRunes: len([]rune(text))}
	if text == "" {
		return Assessment{Issues: []string{"empty"}}
	}

	penalize := func(issue string, amount float64) {
		assessment.Issues = append(assessment.Issues, issue)
		assessment.Score -= amount
	}
	if assessment.LengthRunes < profile.MinRunes {
		penalize("too_short", 0.25)
	}
	if assessment.LengthRunes > profile.MaxRunes {
		penalize("too_long", 0.35)
		if assessment.LengthRunes > profile.MaxRunes*2 {
			assessment.Score -= 0.2
		}
	}

	if containsTemplateHeading(text) {
		penalize("template_heading", 0.25)
	}
	if strings.Contains(text, "**") || strings.Contains(text, "__") {
		penalize("markdown_emphasis", 0.15)
	}
	if hasMarkdownHeading(text) {
		penalize("markdown_heading", 0.15)
	}

	bulletCount := countBulletLines(text)
	if bulletCount > 0 && !profile.AllowBullets {
		penalize("bullets_not_allowed", 0.2)
	} else if profile.MaxBullets > 0 && bulletCount > profile.MaxBullets {
		penalize("too_many_bullets", 0.15)
	}

	sentenceCount := countSentences(text)
	if sentenceCount < profile.MinSentences {
		penalize("too_few_sentences", 0.1)
	} else if sentenceCount > profile.MaxSentences+bulletCount {
		penalize("too_many_sentences", 0.15)
	}
	if endsAbruptly(text) {
		penalize("abrupt_ending", 0.15)
	}

	if assessment.Score < 0 {
		assessment.Score = 0
	}
	return assessment
}

func containsTemplateHeading(text string) bool {
	for _, term := range []string{"背景/问题", "关键过程", "明确结论/结果", "明确的结论/结果", "核心结论"} {
		if strings.Contains(text, term) {
			return true
		}
	}
	return false
}

func hasMarkdownHeading(text string) bool {
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "#") {
			return true
		}
	}
	return false
}

func countBulletLines(text string) int {
	count := 0
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "- ") || strings.HasPrefix(line, "• ") {
			count++
		}
	}
	return count
}

func countSentences(text string) int {
	runes := []rune(text)
	count := 0
	for i, r := range runes {
		switch r {
		case '。', '！', '？', '!', '?':
			if isNaturalBoundary(runes, i) {
				count++
			}
		}
	}
	return count
}

func endsAbruptly(text string) bool {
	runes := []rune(strings.TrimSpace(text))
	if len(runes) == 0 {
		return true
	}
	switch runes[len(runes)-1] {
	case '。', '！', '？', '!', '?':
		return false
	default:
		return true
	}
}

func isNaturalBoundary(runes []rune, idx int) bool { //nolint:gocyclo // 逐字符扫描自然断句边界，分支即规则表；拆分会把同一套判据散到多处
	if idx < 0 || idx >= len(runes) {
		return false
	}
	r := runes[idx]
	switch r {
	case '。', '！', '？', '；', '\n':
		return true
	case '?':
		if idx > 0 && unicode.IsSpace(runes[idx-1]) {
			return false
		}
		if idx+1 >= len(runes) {
			return true
		}
		next := runes[idx+1]
		if next == '=' || unicode.IsLetter(next) || unicode.IsDigit(next) {
			return false
		}
		return unicode.IsSpace(next) || strings.ContainsRune("\"')]}”’", next)
	case '.', '!', ';':
		if idx > 0 && unicode.IsSpace(runes[idx-1]) {
			return false
		}
		if idx+1 >= len(runes) {
			return true
		}
		next := runes[idx+1]
		if next == '=' {
			return false
		}
		return unicode.IsSpace(next) || unicode.Is(unicode.Han, next) || strings.ContainsRune("\"')]}”’", next)
	default:
		return false
	}
}

func isProject(in Input) bool {
	rawURL := strings.TrimSpace(in.URL)
	if rawURL == "" {
		return strings.EqualFold(strings.TrimSpace(in.FetcherType), "github")
	}
	parsed, err := url.Parse(rawURL)
	if err != nil || parsed.Hostname() == "" {
		return strings.EqualFold(strings.TrimSpace(in.FetcherType), "github")
	}
	host := strings.TrimPrefix(strings.ToLower(parsed.Hostname()), "www.")
	if host != "github.com" {
		return false
	}
	segments := strings.FieldsFunc(parsed.EscapedPath(), func(r rune) bool { return r == '/' })
	return len(segments) == 2
}
