package translator

import (
	"crypto/sha256"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

const modelPlaceholderPrefix = "__WEBTAG_PROTECTED_"

var (
	urlSchemePattern        = regexp.MustCompile(`(?i)\b(?:https?|ftp)://`)
	modelPlaceholderPattern = regexp.MustCompile(
		`__WEBTAG_PROTECTED_[0-9a-f]{16}_X*[0-9]{6}__`,
	)
)

type protectedRange struct {
	start int
	end   int
}

type protectedToken struct {
	Placeholder string
	Original    string
	Namespace   string
}

func protectForTranslation(text string) (string, []protectedToken) {
	ranges := protectedTranslationRanges(text)
	if len(ranges) == 0 {
		return text, nil
	}
	digest := sha256.Sum256([]byte(text))
	namespace := fmt.Sprintf("%s%x_", modelPlaceholderPrefix, digest[:8])
	for strings.Contains(text, namespace) {
		namespace += "X"
	}
	tokens := make([]protectedToken, 0, len(ranges))
	var output strings.Builder
	start := 0
	for index, item := range ranges {
		placeholder := fmt.Sprintf("%s%06d__", namespace, index)
		output.WriteString(text[start:item.start])
		output.WriteString(placeholder)
		tokens = append(tokens, protectedToken{
			Placeholder: placeholder,
			Original:    text[item.start:item.end],
			Namespace:   namespace,
		})
		start = item.end
	}
	output.WriteString(text[start:])
	return output.String(), tokens
}

func restoreProtectedTokens(text string, tokens []protectedToken) (string, error) {
	if len(tokens) == 0 {
		return text, nil
	}
	lastIndex := -1
	for _, token := range tokens {
		index := strings.Index(text, token.Placeholder)
		if count := strings.Count(text, token.Placeholder); count != 1 {
			return "", fmt.Errorf("protected placeholder %q occurred %d times", token.Placeholder, count)
		}
		if index <= lastIndex {
			return "", fmt.Errorf("protected placeholders were reordered")
		}
		lastIndex = index
	}
	restored := text
	for _, token := range tokens {
		restored = strings.Replace(restored, token.Placeholder, token.Original, 1)
	}
	if strings.Contains(restored, tokens[0].Namespace) {
		return "", fmt.Errorf("translation returned an unknown protected placeholder")
	}
	return restored, nil
}

func stripProtectedTokens(text string, tokens []protectedToken) string {
	for _, token := range tokens {
		text = strings.ReplaceAll(text, token.Placeholder, "")
	}
	return text
}

func protectedInlineRanges(text string) []protectedRange {
	ranges := make([]protectedRange, 0)
	ranges = append(ranges, inlineCodeRanges(text)...)
	ranges = append(ranges, markdownLinkTailRanges(text)...)
	ranges = append(ranges, rawURLRanges(text)...)
	return mergeProtectedRanges(ranges)
}

func protectedTranslationRanges(text string) []protectedRange {
	ranges := fencedCodeRanges(text)
	ranges = append(ranges, protectedInlineRanges(text)...)
	return mergeProtectedRanges(ranges)
}

func fencedCodeRanges(text string) []protectedRange {
	ranges := make([]protectedRange, 0)
	offset := 0
	for _, part := range splitFencedCodeParts(text) {
		end := offset + len(part.Text)
		if !part.Translate {
			ranges = append(ranges, protectedRange{start: offset, end: end})
		}
		offset = end
	}
	return ranges
}

func rawURLRanges(text string) []protectedRange {
	ranges := make([]protectedRange, 0)
	searchFrom := 0
	for searchFrom < len(text) {
		match := urlSchemePattern.FindStringIndex(text[searchFrom:])
		if match == nil {
			break
		}
		start := searchFrom + match[0]
		end := scanRawURLEnd(text, start)
		if end <= start {
			searchFrom = start + 1
			continue
		}
		ranges = append(ranges, protectedRange{start: start, end: end})
		searchFrom = end
	}
	return ranges
}

func scanRawURLEnd(text string, start int) int {
	parenthesisDepth := 0
	for cursor := start; cursor < len(text); {
		r, size := utf8.DecodeRuneInString(text[cursor:])
		if unicode.IsSpace(r) || strings.ContainsRune("<>\"", r) {
			return trimClosingSingleQuote(text, start, cursor)
		}
		switch r {
		case '(':
			parenthesisDepth++
		case ')':
			if parenthesisDepth == 0 {
				return trimClosingSingleQuote(text, start, cursor)
			}
			parenthesisDepth--
		}
		cursor += size
	}
	return trimClosingSingleQuote(text, start, len(text))
}

func trimClosingSingleQuote(text string, start, end int) int {
	if start == 0 || text[start-1] != '\'' || end <= start {
		return end
	}
	segment := text[start:end]
	closing := strings.LastIndexByte(segment, '\'')
	if closing < 0 {
		return end
	}
	for _, r := range segment[closing+1:] {
		if !unicode.IsPunct(r) {
			return end
		}
	}
	return start + closing
}

func mergeProtectedRanges(ranges []protectedRange) []protectedRange {
	if len(ranges) < 2 {
		return ranges
	}
	sort.Slice(ranges, func(i, j int) bool {
		if ranges[i].start == ranges[j].start {
			return ranges[i].end > ranges[j].end
		}
		return ranges[i].start < ranges[j].start
	})
	merged := ranges[:1]
	for _, item := range ranges[1:] {
		last := &merged[len(merged)-1]
		if item.start <= last.end {
			if item.end > last.end {
				last.end = item.end
			}
			continue
		}
		merged = append(merged, item)
	}
	return merged
}

func inlineCodeRanges(text string) []protectedRange {
	ranges := make([]protectedRange, 0)
	for index := 0; index < len(text); {
		if text[index] != '`' {
			index++
			continue
		}
		start := index
		width := repeatedPrefix(text[index:], '`')
		index += width
		for index < len(text) {
			next := strings.IndexByte(text[index:], '`')
			if next < 0 {
				index = len(text)
				break
			}
			candidate := index + next
			closingWidth := repeatedPrefix(text[candidate:], '`')
			if closingWidth == width {
				end := candidate + closingWidth
				ranges = append(ranges, protectedRange{start: start, end: end})
				index = end
				break
			}
			index = candidate + closingWidth
		}
	}
	return ranges
}

func markdownLinkTailRanges(text string) []protectedRange {
	ranges := make([]protectedRange, 0)
	for start := 0; start+1 < len(text); start++ {
		if text[start] != ']' || text[start+1] != '(' {
			continue
		}
		depth := 1
		for cursor := start + 2; cursor < len(text); cursor++ {
			if text[cursor] == '\\' {
				cursor++
				continue
			}
			switch text[cursor] {
			case '(':
				depth++
			case ')':
				depth--
				if depth == 0 {
					ranges = append(ranges, protectedRange{start: start, end: cursor + 1})
					start = cursor
					cursor = len(text)
				}
			}
		}
	}
	return ranges
}
