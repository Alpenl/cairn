package translator

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

type translationPart struct {
	Text       string
	Translate  bool
	Protection []protectedToken
}

func splitTranslationParts(text string, maxChars int) []translationPart {
	sourceChunks := splitAtNaturalBoundaries(text, maxChars)
	parts := make([]translationPart, 0, len(sourceChunks))
	for _, sourceChunk := range sourceChunks {
		modelText, tokens := protectForTranslation(sourceChunk)
		parts = append(parts, translationPart{
			Text:       modelText,
			Translate:  true,
			Protection: tokens,
		})
	}
	return parts
}

func countTranslatableParts(parts []translationPart) int {
	count := 0
	for _, part := range parts {
		if partNeedsModel(part) {
			count++
		}
	}
	return count
}

func partNeedsModel(part translationPart) bool {
	return part.Translate && strings.TrimSpace(stripProtectedTokens(part.Text, part.Protection)) != ""
}

func outerWhitespace(text string) (leading, core, trailing string) {
	core = strings.TrimSpace(text)
	if core == "" {
		return text, "", ""
	}
	start := strings.Index(text, core)
	return text[:start], core, text[start+len(core):]
}

func splitAtNaturalBoundaries(text string, maxChars int) []string {
	if maxChars <= 0 || utf8.RuneCountInString(text) <= maxChars {
		return []string{text}
	}
	runes := []rune(text)
	chunks := make([]string, 0, len(runes)/maxChars+1)
	for len(runes) > maxChars {
		cut := naturalBoundary(runes, maxChars)
		chunks = append(chunks, string(runes[:cut]))
		runes = runes[cut:]
	}
	if len(runes) > 0 {
		chunks = append(chunks, string(runes))
	}
	return chunks
}

func naturalBoundary(runes []rune, limit int) int {
	minimum := max(1, limit/2)
	cut := limit
	for i := limit; i >= minimum; i-- {
		if i >= 2 && runes[i-2] == '\n' && runes[i-1] == '\n' {
			cut = i
			return adjustProtectedBoundary(runes, cut, minimum)
		}
	}
	for i := limit; i >= minimum; i-- {
		if runes[i-1] == '\n' {
			cut = i
			return adjustProtectedBoundary(runes, cut, minimum)
		}
	}
	for i := limit; i >= minimum; i-- {
		if isSentenceEnd(runes[i-1]) && (i == len(runes) || unicode.IsSpace(runes[i])) {
			cut = i
			return adjustProtectedBoundary(runes, cut, minimum)
		}
	}
	for i := limit; i >= minimum; i-- {
		if unicode.IsSpace(runes[i-1]) {
			cut = i
			return adjustProtectedBoundary(runes, cut, minimum)
		}
	}
	return adjustProtectedBoundary(runes, cut, minimum)
}

func adjustProtectedBoundary(runes []rune, cut, minimum int) int {
	text := string(runes)
	byteCut := len(string(runes[:cut]))
	for _, protected := range protectedTranslationRanges(text) {
		if protected.start >= byteCut || protected.end <= byteCut {
			continue
		}
		start := utf8.RuneCountInString(text[:protected.start])
		if start >= minimum {
			return start
		}
		return utf8.RuneCountInString(text[:protected.end])
	}
	return cut
}

func isSentenceEnd(r rune) bool {
	return strings.ContainsRune(".!?;。！？；", r)
}

func splitFencedCodeParts(text string) []translationPart {
	parts := make([]translationPart, 0, 4)
	plainStart := 0
	fenceStart := -1
	var fenceChar byte
	fenceWidth := 0
	for lineStart := 0; lineStart < len(text); {
		lineEnd := nextLineEnd(text, lineStart)
		line := strings.TrimSuffix(strings.TrimSuffix(text[lineStart:lineEnd], "\n"), "\r")
		if fenceStart < 0 {
			if char, width, ok := openingFence(line); ok {
				appendTranslationPart(&parts, text[plainStart:lineStart], true)
				fenceStart, fenceChar, fenceWidth = lineStart, char, width
			}
		} else if closingFence(line, fenceChar, fenceWidth) {
			appendTranslationPart(&parts, text[fenceStart:lineEnd], false)
			plainStart = lineEnd
			fenceStart = -1
		}
		lineStart = lineEnd
	}
	if fenceStart >= 0 {
		appendTranslationPart(&parts, text[fenceStart:], false)
	} else {
		appendTranslationPart(&parts, text[plainStart:], true)
	}
	if len(parts) == 0 {
		return []translationPart{{Text: text, Translate: true}}
	}
	return parts
}

func appendTranslationPart(parts *[]translationPart, text string, translate bool) {
	if text != "" {
		*parts = append(*parts, translationPart{Text: text, Translate: translate})
	}
}

func nextLineEnd(text string, start int) int {
	if offset := strings.IndexByte(text[start:], '\n'); offset >= 0 {
		return start + offset + 1
	}
	return len(text)
}

func openingFence(line string) (byte, int, bool) {
	trimmed, ok := trimFenceIndent(line)
	if !ok || len(trimmed) < 3 || (trimmed[0] != '`' && trimmed[0] != '~') {
		return 0, 0, false
	}
	width := repeatedPrefix(trimmed, trimmed[0])
	return trimmed[0], width, width >= 3
}

func closingFence(line string, char byte, minimumWidth int) bool {
	trimmed, ok := trimFenceIndent(line)
	if !ok || len(trimmed) < minimumWidth || trimmed[0] != char {
		return false
	}
	width := repeatedPrefix(trimmed, char)
	return width >= minimumWidth && strings.TrimSpace(trimmed[width:]) == ""
}

func trimFenceIndent(line string) (string, bool) {
	indent := 0
	for indent < len(line) && line[indent] == ' ' {
		indent++
	}
	return line[indent:], indent <= 3
}

func repeatedPrefix(text string, char byte) int {
	width := 0
	for width < len(text) && text[width] == char {
		width++
	}
	return width
}
