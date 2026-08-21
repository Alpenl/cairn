package analyzer

import (
	"bytes"
	stdjson "encoding/json"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"

	"webtag/internal/jsonx"
	"webtag/internal/model"
	"webtag/internal/summarypolicy"
	"webtag/internal/textutil"
)

var (
	jsonBlockRE  = regexp.MustCompile("```(?:json)?\\s*(\\{.*?\\})\\s*```")
	jsonObjectRE = regexp.MustCompile(`\{.*\}`)
)

// parseAnalysisResponse runs every JSON-shaped fragment found inside raw
// through validateAnalysisResponse and returns the first one that passes.
// This three-tier extraction (whole text → fenced ```json``` block →
// regex-matched object → balanced-brace scanner) is what lets us tolerate
// models that wrap JSON in prose, double-escape backticks, or emit bare
// JSON without any wrapper.
func (a *OpenAIAnalyzer) parseAnalysisResponse(raw string) (AnalysisResult, error) {
	return a.parseAnalysisResponseWithLimit(raw, a.maxSummaryChars)
}

func (a *OpenAIAnalyzer) parseAnalysisResponseWithLimit(raw string, summaryLimit int) (AnalysisResult, error) {
	return a.parseAnalysisResponseForRequest(raw, summaryLimit, model.RequestedLibraryKindAuto)
}

func (a *OpenAIAnalyzer) parseAnalysisResponseForRequest(raw string, summaryLimit int, requested model.RequestedLibraryKind) (AnalysisResult, error) {
	text := strings.TrimSpace(raw)
	candidates := jsonCandidates(text)
	if len(candidates) == 0 {
		return AnalysisResult{}, ErrAnalyzerInvalidJSON
	}

	var lastErr error
	for _, candidate := range candidates {
		data, ok := decodeJSONObject(candidate)
		if !ok {
			continue
		}

		result, err := a.validateAnalysisResponseForRequest(data, summaryLimit, requested)
		if err == nil {
			return result, nil
		}
		// Keep the outermost candidate's diagnostic. Balanced-brace recovery
		// also yields nested profile objects; their missing top-level fields are
		// less useful than an explicit v2 kind conflict from the full response.
		if lastErr == nil {
			lastErr = err
		}
	}

	if lastErr != nil {
		return AnalysisResult{}, lastErr
	}

	return AnalysisResult{}, ErrAnalyzerInvalidJSON
}

func (a *OpenAIAnalyzer) validateAnalysisResponseForRequest(data map[string]any, summaryLimit int, requested model.RequestedLibraryKind) (AnalysisResult, error) {
	if accessible, present := data["accessible"].(bool); present && !accessible {
		return AnalysisResult{Accessible: false}, nil
	}
	if _, versioned := data["schema_version"]; !versioned {
		if requested == model.RequestedLibraryKindSite {
			return AnalysisResult{}, fmt.Errorf("analyzer library_kind conflicts with explicit request")
		}
		return a.validateReadingAnalysisProfile(data, summaryLimit)
	}
	return a.validateLibraryAnalysisResponse(data, summaryLimit, requested)
}

func (a *OpenAIAnalyzer) validateLibraryAnalysisResponse(data map[string]any, summaryLimit int, requested model.RequestedLibraryKind) (AnalysisResult, error) { //nolint:gocyclo // 逐字段校验模型响应，分支即 schema 约束
	version, ok := numericFloat32(data["schema_version"])
	if !ok || version != 2 {
		return AnalysisResult{}, fmt.Errorf("analyzer schema_version must be 2")
	}
	rawKind, ok := data["library_kind"].(string)
	if !ok || (rawKind != string(model.LibraryKindReading) && rawKind != string(model.LibraryKindSite)) {
		return AnalysisResult{}, fmt.Errorf("analyzer library_kind must be reading or site")
	}
	kind := model.LibraryKind(rawKind)
	if requested == model.RequestedLibraryKindReading && kind != model.LibraryKindReading || requested == model.RequestedLibraryKindSite && kind != model.LibraryKindSite {
		return AnalysisResult{}, fmt.Errorf("analyzer library_kind conflicts with explicit request")
	}
	result := AnalysisResult{Accessible: true, LibraryKind: kind}
	if kind == model.LibraryKindReading {
		profile, ok := data["reading_profile"].(map[string]any)
		if !ok {
			return AnalysisResult{}, fmt.Errorf("analyzer reading_profile missing")
		}
		return a.validateReadingAnalysisProfile(profile, summaryLimit)
	}
	profile, ok := data["site_profile"].(map[string]any)
	if !ok {
		return AnalysisResult{}, fmt.Errorf("analyzer site_profile missing")
	}
	name, nameOK := boundedString(profile["name"], 256)
	intro, introOK := boundedString(profile["intro"], 1000)
	entryName, entryNameOK := boundedString(profile["entry_name"], 256)
	purpose, purposeOK := boundedString(profile["purpose"], 1000)
	if !nameOK || !introOK || !entryNameOK || !purposeOK {
		return AnalysisResult{}, fmt.Errorf("analyzer site_profile fields missing or invalid")
	}
	tags, err := a.validateTags(profile["tags"])
	if err != nil {
		return AnalysisResult{}, err
	}
	result.SiteName, result.SiteIntro, result.EntryName, result.EntryPurpose, result.Tags = name, intro, entryName, purpose, tags
	return result, nil
}

func (a *OpenAIAnalyzer) validateReadingAnalysisProfile(profile map[string]any, summaryLimit int) (AnalysisResult, error) {
	title, _ := profile["title"].(string)
	summary, err := a.validateSummary(profile["summary"], summaryLimit)
	if err != nil {
		return AnalysisResult{}, err
	}
	tags, err := a.validateTags(profile["tags"])
	if err != nil {
		return AnalysisResult{}, err
	}
	return AnalysisResult{
		Accessible:  true,
		LibraryKind: model.LibraryKindReading,
		Title:       textutil.NormalizeTitle(title),
		Summary:     summary,
		Tags:        tags,
	}, nil
}

func numericFloat32(value any) (float32, bool) {
	switch number := value.(type) {
	case float64:
		return float32(number), true
	case float32:
		return number, true
	case int:
		return float32(number), true
	case int64:
		return float32(number), true
	case int32:
		return float32(number), true
	case stdjson.Number:
		parsed, err := number.Float64()
		return float32(parsed), err == nil
	default:
		return 0, false
	}
}

func boundedString(value any, max int) (string, bool) {
	text, ok := value.(string)
	text = strings.TrimSpace(text)
	return text, ok && text != "" && runeCount(text) <= max
}

func (a *OpenAIAnalyzer) validateSummary(value any, summaryLimit int) (string, error) {
	summary, ok := value.(string)
	if !ok {
		return "", fmt.Errorf("analyzer summary missing or empty")
	}
	summary = summarypolicy.Clean(summary)
	if summary == "" {
		return "", fmt.Errorf("analyzer summary missing or empty")
	}
	if runeCount(summary) > summaryLimit {
		summary = summarypolicy.Clamp(summary, summaryLimit)
	}
	return summary, nil
}

func (a *OpenAIAnalyzer) validateTags(value any) ([]string, error) {
	rawTags, ok := value.([]any)
	if !ok {
		return nil, nil
	}
	tags := make([]string, 0, len(rawTags))
	seen := make(map[string]struct{}, len(rawTags))
	for _, rawTag := range rawTags {
		tag, ok := rawTag.(string)
		if !ok {
			continue
		}
		tag = normalizeTag(tag)
		if tag == "" {
			continue
		}
		if runeCount(tag) > a.maxTagChars {
			continue
		}
		key := strings.ToLower(tag)
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		tags = append(tags, tag)
		if len(tags) == a.maxTags {
			break
		}
	}
	return tags, nil
}

func decodeJSONObject(raw string) (map[string]any, bool) {
	var data map[string]any
	if err := jsonx.Unmarshal([]byte(strings.TrimSpace(raw)), &data); err != nil {
		return nil, false
	}
	if data == nil {
		return nil, false
	}
	return data, true
}

// decodeAnalyzerMessageContent normalises the four content shapes seen in
// real OpenAI-compatible responses: plain string, array of content parts,
// single content-part object, and the special "null" sentinel. Anything
// outside those four returns an error so the call layer can mark it
// retryable.
func decodeAnalyzerMessageContent(raw jsonx.RawMessage) (string, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return "", nil
	}

	var text string
	if err := jsonx.Unmarshal(trimmed, &text); err == nil {
		return text, nil
	}

	var parts []map[string]any
	if err := jsonx.Unmarshal(trimmed, &parts); err == nil {
		fragments := make([]string, 0, len(parts))
		for _, part := range parts {
			if fragment := analyzerContentPartText(part); fragment != "" {
				fragments = append(fragments, fragment)
			}
		}
		return strings.TrimSpace(strings.Join(fragments, "\n")), nil
	}

	var part map[string]any
	if err := jsonx.Unmarshal(trimmed, &part); err == nil {
		return analyzerContentPartText(part), nil
	}

	return "", fmt.Errorf("unsupported content shape")
}

func analyzerContentPartText(part map[string]any) string {
	if part == nil {
		return ""
	}

	switch text := part["text"].(type) {
	case string:
		return strings.TrimSpace(text)
	case map[string]any:
		if value, ok := text["value"].(string); ok {
			return strings.TrimSpace(value)
		}
	}

	if value, ok := part["value"].(string); ok {
		return strings.TrimSpace(value)
	}
	if content, ok := part["content"].(string); ok {
		return strings.TrimSpace(content)
	}

	return ""
}

// jsonCandidates emits up to four candidate JSON strings: the trimmed raw
// text, the contents of each fenced ```json``` block, the first
// regex-matched balanced object, and every brace-balanced run found by
// the manual scanner. Duplicates are dropped before return so callers can
// blindly try them in order.
func jsonCandidates(text string) []string {
	candidates := make([]string, 0, 4)
	seen := map[string]struct{}{}

	add := func(candidate string) {
		candidate = strings.TrimSpace(candidate)
		if candidate == "" {
			return
		}
		if _, ok := seen[candidate]; ok {
			return
		}
		seen[candidate] = struct{}{}
		candidates = append(candidates, candidate)
	}

	add(text)
	if matches := jsonBlockRE.FindAllStringSubmatch(text, -1); len(matches) > 0 {
		for _, match := range matches {
			if len(match) == 2 {
				add(match[1])
			}
		}
	}
	if match := jsonObjectRE.FindString(text); match != "" {
		add(match)
	}
	for _, candidate := range scanJSONObjectCandidates(text) {
		add(candidate)
	}

	return candidates
}

// scanJSONObjectCandidates walks the text once, tracking whether the
// cursor is inside a JSON string and respecting escape sequences. Every
// balanced `{...}` it sees becomes a candidate. This catches JSON that is
// followed (or preceded) by stray commentary the regex scanner misses.
func scanJSONObjectCandidates(text string) []string {
	type stackEntry struct {
		index int
	}

	candidates := make([]string, 0, 4)
	stack := make([]stackEntry, 0, 8)
	inString := false
	escaped := false

	for idx, r := range text {
		if escaped {
			escaped = false
			continue
		}
		if r == '\\' && inString {
			escaped = true
			continue
		}
		if r == '"' {
			inString = !inString
			continue
		}
		if inString {
			continue
		}

		switch r {
		case '{':
			stack = append(stack, stackEntry{index: idx})
		case '}':
			if len(stack) == 0 {
				continue
			}
			start := stack[len(stack)-1].index
			stack = stack[:len(stack)-1]
			candidates = append(candidates, text[start:idx+utf8.RuneLen(r)])
		}
	}

	return candidates
}

// normalizeTag is the format safety-net for model-emitted tags: even after
// the prompt asks for short single-topic tags, models still leak
// hashtag markers, wrapping brackets/quotes, slash hierarchy separators
// (e.g. "check/handle"), and ragged whitespace. It cleans those in-place
// (one tag in, one tag out) BEFORE the length / dedup / count checks run,
// so the existing reject-and-retry contract is preserved — it never
// splits a tag into many or relaxes any bound.
//
// Slash/backslash hierarchy separators are collapsed by keeping the single
// longest segment ("check/handle" → "handle"), which de-hierarchizes the
// tag into one reusable value instead of polluting the tag tree. Internal
// whitespace runs are collapsed to a single space (phrase-splitting is left
// to the prompt — naive space-splitting would mint junk like "2" from
// "Go 2").
func normalizeTag(tag string) string {
	tag = strings.TrimSpace(tag)
	// Strip hashtag markers and wrapping punctuation models like to add.
	tag = strings.Trim(tag, "#＃[](){}「」『』\"'`， \t\n")
	if tag == "" {
		return ""
	}
	if strings.ContainsAny(tag, "/\\") {
		best := ""
		for _, seg := range strings.FieldsFunc(tag, func(r rune) bool {
			return r == '/' || r == '\\'
		}) {
			seg = strings.TrimSpace(seg)
			if runeCount(seg) > runeCount(best) {
				best = seg
			}
		}
		tag = best
	}
	// Collapse internal whitespace runs to a single space.
	tag = strings.Join(strings.Fields(tag), " ")
	return tag
}

// runeCount and truncateRunes delegate to internal/textutil so the analyzer
// shares the same rune semantics as the fetch path. These thin wrappers
// stay in-package to keep the analyzer's call sites readable.
func runeCount(value string) int {
	return textutil.Count(value)
}

func truncateRunes(value string, limit int) string {
	return textutil.Truncate(value, limit)
}
