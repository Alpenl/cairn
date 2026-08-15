package translator

import (
	"strings"
)

func simplifyNaturalLanguage(text string) (string, error) {
	converter, err := traditionalToSimplifiedConverter()
	if err != nil {
		return "", err
	}
	var output strings.Builder
	for _, part := range splitFencedCodeParts(text) {
		if !part.Translate {
			output.WriteString(part.Text)
			continue
		}
		converted, err := transformUnprotected(part.Text, converter.Convert)
		if err != nil {
			return "", err
		}
		output.WriteString(converted)
	}
	return output.String(), nil
}

func transformUnprotected(text string, transform func(string) (string, error)) (string, error) {
	ranges := protectedInlineRanges(text)
	if len(ranges) == 0 {
		return transform(text)
	}
	var output strings.Builder
	start := 0
	for _, item := range ranges {
		converted, err := transform(text[start:item.start])
		if err != nil {
			return "", err
		}
		output.WriteString(converted)
		output.WriteString(text[item.start:item.end])
		start = item.end
	}
	converted, err := transform(text[start:])
	if err != nil {
		return "", err
	}
	output.WriteString(converted)
	return output.String(), nil
}

func naturalLanguageText(text string) string {
	var output strings.Builder
	for _, part := range splitFencedCodeParts(text) {
		if !part.Translate {
			output.WriteByte(' ')
			continue
		}
		output.WriteString(replaceProtectedInlineWithSpaces(part.Text))
	}
	return output.String()
}

func replaceProtectedInlineWithSpaces(text string) string {
	ranges := protectedInlineRanges(text)
	if len(ranges) == 0 {
		return text
	}
	var output strings.Builder
	start := 0
	for _, item := range ranges {
		output.WriteString(text[start:item.start])
		output.WriteByte(' ')
		start = item.end
	}
	output.WriteString(text[start:])
	return output.String()
}
