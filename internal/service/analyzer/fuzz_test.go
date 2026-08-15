package analyzer

import (
	"encoding/json"
	"testing"
)

func FuzzParseAnalysisResponse(f *testing.F) {
	analyzer := NewOpenAIAnalyzer(OpenAIAnalyzerOptions{
		BaseURL:         "https://example.com",
		APIKey:          "secret-key",
		Model:           "gpt-test",
		MaxSummaryChars: 50,
		MinTags:         1,
		MaxTags:         5,
		MaxTagChars:     20,
	})

	seeds := []string{
		`{"summary":"ok","tags":["Go"]}`,
		`{"summary":"","tags":["Go"]}`,
		"```json\n{\"summary\":\"fenced\",\"tags\":[\"Go\",\"AI\"]}\n```",
		"前文 {\"summary\":\"embedded\",\"tags\":[\"Go\"]} 后文",
		"{",
		"",
		"\x00\xff{\"summary\":\"binary\",\"tags\":[\"Go\"]}",
	}
	for _, seed := range seeds {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, raw string) {
		_, _ = analyzer.parseAnalysisResponse(raw)
	})
}

func FuzzDecodeAnalyzerMessageContent(f *testing.F) {
	seeds := [][]byte{
		[]byte(`"plain text"`),
		[]byte(`null`),
		[]byte(`[]`),
		[]byte(`[{"type":"output_text","text":"{\"summary\":\"ok\",\"tags\":[\"Go\"]}"}]`),
		[]byte(`{"type":"output_text","text":"{\"summary\":\"obj\",\"tags\":[\"Go\"]}"}`),
		[]byte(`{"text":{"value":"nested"}}`),
		[]byte(`{"content":"direct"}`),
		[]byte(`{`),
		{0x00, 0xff, '{', '}'},
	}
	for _, seed := range seeds {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, raw []byte) {
		_, err := decodeAnalyzerMessageContent(json.RawMessage(raw))
		if err == nil {
			return
		}
		switch err.Error() {
		case "unsupported content shape":
			return
		default:
			return
		}
	})
}
