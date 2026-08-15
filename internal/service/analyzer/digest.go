package analyzer

import (
	"strings"
	"unicode"

	"webtag/internal/summarypolicy"
)

const maxDigestCoverageSections = 3

var digestCategoryTitles = map[string]struct{}{
	"ai相关": {}, "ai项目": {}, "articles": {}, "news": {}, "projects": {},
	"quotes": {}, "resources": {}, "tools": {}, "往年回顾": {}, "工具": {},
	"开发工具": {}, "文摘": {}, "文章": {}, "新闻": {}, "学习资源": {},
	"图片": {}, "科技动态": {}, "科技新闻": {}, "言论": {}, "资源": {},
}

// ensureDigestCoverage repairs the stable model failure mode where a digest
// summary describes only the lead story. The added sentence is assembled from
// actual section headings and their first visible item, then the lead is
// shortened to keep the profile limit. No facts are inferred beyond the body.
func ensureDigestCoverage(summary, body string, limit int) string {
	sections := digestCoverageSections(body)
	if len(sections) < 2 || digestCoverageCount(summary, sections) >= 2 {
		return summary
	}

	addition := digestCoverageSentence(sections)
	if addition == "" {
		return summary
	}
	additionRunes := runeCount(addition)
	if limit <= additionRunes+1 {
		return summarypolicy.Clamp(addition, limit)
	}

	base := compactDigestLead(summary, limit-additionRunes-1)
	if base == "" {
		return addition
	}
	return strings.TrimSpace(base) + addition
}

func compactDigestLead(summary string, limit int) string {
	base := summarypolicy.Clamp(summary, limit)
	if !strings.HasSuffix(base, "…") {
		return base
	}

	runes := []rune(summary)
	start := min(limit-1, len(runes)-1)
	for i := start; i >= limit/3; i-- {
		if strings.ContainsRune("，,；;", runes[i]) {
			return strings.TrimSpace(string(runes[:i])) + "。"
		}
	}
	return strings.TrimSuffix(base, "…") + "。"
}

func digestCoverageSections(body string) []documentSection {
	lines := strings.Split(strings.ReplaceAll(body, "\r\n", "\n"), "\n")
	sections := make([]documentSection, 0, maxDigestCoverageSections)
	seen := make(map[string]struct{}, maxDigestCoverageSections)
	for i := range lines {
		title := normalizedSectionTitle(lines[i])
		key := strings.ToLower(strings.ReplaceAll(title, " ", ""))
		if _, ok := digestCategoryTitles[key]; !ok {
			continue
		}
		if _, duplicate := seen[key]; duplicate {
			continue
		}
		seen[key] = struct{}{}

		preview := ""
		for j := i + 1; j < len(lines); j++ {
			if candidate := strings.TrimSpace(lines[j]); candidate != "" {
				preview = cleanDigestPreview(candidate)
				break
			}
		}
		sections = append(sections, documentSection{title: title, preview: preview})
		if len(sections) == maxDigestCoverageSections {
			break
		}
	}
	return sections
}

func digestCoverageCount(summary string, sections []documentSection) int {
	normalized := strings.ToLower(strings.ReplaceAll(summary, " ", ""))
	count := 0
	for _, section := range sections {
		title := strings.ToLower(strings.ReplaceAll(section.title, " ", ""))
		if strings.Contains(normalized, title) {
			count++
		}
	}
	return count
}

func digestCoverageSentence(sections []documentSection) string {
	parts := make([]string, 0, min(len(sections), maxDigestCoverageSections))
	for _, section := range sections {
		part := section.title
		if section.preview != "" {
			part += "“" + truncateRunes(section.preview, 28) + "”"
		}
		parts = append(parts, part)
		if len(parts) == maxDigestCoverageSections {
			break
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return "此外，本期还收录" + joinDigestParts(parts) + "等内容。"
}

func joinDigestParts(parts []string) string {
	switch len(parts) {
	case 0:
		return ""
	case 1:
		return parts[0]
	case 2:
		return parts[0] + "和" + parts[1]
	default:
		return strings.Join(parts[:len(parts)-1], "、") + "以及" + parts[len(parts)-1]
	}
}

func cleanDigestPreview(value string) string {
	runes := []rune(strings.TrimSpace(value))
	i := 0
	for i < len(runes) && unicode.IsDigit(runes[i]) {
		i++
	}
	for i < len(runes) && strings.ContainsRune("、.)． ", runes[i]) {
		i++
	}
	return strings.TrimSpace(string(runes[i:]))
}

func canonicalizeDigestTags(tags []string, body string) []string {
	if len(tags) == 0 {
		return tags
	}
	hasShortStory := strings.Contains(body, "短篇小说")
	hasToolSection := containsStandaloneSection(body, "工具") || containsStandaloneSection(body, "开发工具")
	hasTechnologyScope := strings.Contains(body, "科技周刊") || containsStandaloneSection(body, "科技动态")

	out := make([]string, 0, len(tags))
	seen := make(map[string]struct{}, len(tags))
	for _, tag := range tags {
		switch strings.ToLower(strings.TrimSpace(tag)) {
		case "周刊":
			if hasTechnologyScope {
				tag = "科技周刊"
			}
		case "小说", "ai小说":
			if hasShortStory {
				tag = "短篇小说"
			}
		case "工具", "编程工具":
			if hasToolSection {
				tag = "开发工具"
			}
		}
		key := strings.ToLower(strings.TrimSpace(tag))
		if key == "" {
			continue
		}
		if _, duplicate := seen[key]; duplicate {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, tag)
	}
	return out
}

func containsStandaloneSection(body, title string) bool {
	for _, line := range strings.Split(strings.ReplaceAll(body, "\r\n", "\n"), "\n") {
		if strings.EqualFold(normalizedSectionTitle(line), title) {
			return true
		}
	}
	return false
}

func normalizedSectionTitle(value string) string {
	value = strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(value), "#"))
	value = strings.TrimSpace(strings.TrimSuffix(strings.TrimSuffix(value, ":"), "："))
	return value
}
