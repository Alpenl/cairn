package translator

import (
	"regexp"
	"strings"
	"unicode"
)

var latinWordPattern = regexp.MustCompile(`[A-Za-z]{2,}`)

type scriptProfile struct {
	han     int
	latin   int
	foreign int
}

// validSimplifiedChineseOutput rejects unchanged non-Chinese output and
// foreign-script residue. The caller first normalizes natural-language spans
// through OpenCC, and this function also checks that normalization is
// idempotent before accepting the result.
func validSimplifiedChineseOutput(source, output string) bool {
	source = strings.TrimSpace(source)
	output = strings.TrimSpace(output)
	if output == "" {
		return false
	}
	if isProtectedOnly(source) {
		if normalizedNaturalText(source) == normalizedNaturalText(output) {
			return true
		}
		profile := profileScripts(naturalLanguageText(output))
		return profile.han > 0 && profile.foreign == 0 && isSimplified(output)
	}

	sourceNatural := naturalLanguageText(source)
	outputNatural := naturalLanguageText(output)
	sourceProfile := profileScripts(sourceNatural)
	outputProfile := profileScripts(outputNatural)
	unchanged := normalizedNaturalText(sourceNatural) == normalizedNaturalText(outputNatural)
	alreadySimplified := sourceProfile.han > 0 && sourceProfile.foreign == 0 && isSimplified(source)
	if unchanged {
		return alreadySimplified
	}
	if outputProfile.han == 0 || outputProfile.foreign > 0 || !isSimplified(output) {
		return false
	}
	if len(latinWordPattern.FindAllString(outputNatural, -1)) >= 3 && outputProfile.latin > outputProfile.han*2 && outputProfile.latin > 24 {
		return false
	}
	return true
}

func isSimplified(text string) bool {
	converted, err := simplifyNaturalLanguage(text)
	return err == nil && converted == text
}

func normalizedNaturalText(text string) string {
	var output strings.Builder
	for _, r := range text {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			output.WriteRune(unicode.ToLower(r))
		}
	}
	return output.String()
}

func profileScripts(text string) scriptProfile {
	var profile scriptProfile
	for _, r := range text {
		switch {
		case unicode.Is(unicode.Han, r):
			profile.han++
		case unicode.Is(unicode.Latin, r):
			profile.latin++
		case unicode.IsLetter(r):
			profile.foreign++
		}
	}
	return profile
}

func isProtectedOnly(text string) bool {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return false
	}
	urlRanges := rawURLRanges(trimmed)
	return (len(urlRanges) == 1 && urlRanges[0].start == 0 && urlRanges[0].end == len(trimmed)) ||
		isWrappedCode(trimmed) || isCodeLike(trimmed) ||
		isIdentifierLike(trimmed) || isUppercaseAcronym(trimmed)
}

func isWrappedCode(text string) bool {
	parts := splitFencedCodeParts(text)
	if len(parts) == 1 && !parts[0].Translate {
		return true
	}
	return strings.HasPrefix(text, "`") && strings.HasSuffix(text, "`") && strings.Count(text, "`") == 2
}

func isCodeLike(text string) bool {
	codeMarkers := 0
	for _, r := range text {
		if strings.ContainsRune("(){}[];=<>\\/", r) {
			codeMarkers++
		}
	}
	return (!strings.ContainsAny(text, " \t\r\n") && codeMarkers > 0) || codeMarkers >= 3
}

func isIdentifierLike(text string) bool {
	return strings.ContainsAny(text, "_.:") && !strings.ContainsAny(text, " \t\r\n")
}

func isUppercaseAcronym(text string) bool {
	return len([]rune(text)) <= 12 && text == strings.ToUpper(text) && profileScripts(text).latin > 0
}
