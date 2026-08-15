package eval

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"webtag/internal/fetcher"
	"webtag/internal/jsonx"
)

// Judge asks an LLM to rate how well a (content, tags, summary)
// triple satisfies the operator's intent — independent of the
// rule-based scorer, which only knows about whatever lists were
// hand-curated in the case YAML.
//
// The judge is the answer to "I don't want to maintain a
// must_include list for every URL — just tell me, gpt-4o, are these
// tags any good?". It complements rather than replaces the rule
// scorer: rules are deterministic and audit-friendly; the LLM
// judge catches issues the operator hasn't articulated yet.
type Judge interface {
	JudgeCell(ctx context.Context, in JudgeInput) (JudgeVerdict, error)
}

// JudgeInput is what the LLM sees: the original URL + title + body
// preview, plus the tag set + summary the cell produced. Body
// preview is bounded so an enormous fetched article doesn't push the
// judge prompt out of context.
type JudgeInput struct {
	URL                 string
	Title               string
	BodyPreview         string
	Summary             string
	SummaryProfile      string
	SummaryRequirements string
	Tags                []string
}

// JudgeVerdict is a 1-5 score plus a one-sentence rationale. 1-5
// (Likert) rather than 0-1 because operators rate things on Likert
// scales naturally, and the discrete steps are easier to compare
// across runs than continuous floats.
type JudgeVerdict struct {
	Score        float64 // 1.0–5.0; 0 means error / not judged
	SummaryScore float64 // 1.0–5.0; 0 means absent in a legacy response
	TagScore     float64 // 1.0–5.0; 0 means absent in a legacy response
	Reason       string  // <= 200 chars
}

// HTTPJudge calls an OpenAI-compatible chat-completions endpoint
// directly. It does NOT reuse internal/service/analyzer because
// (a) the judge's prompt + parsing contract is different, (b)
// running the judge model against a separate base URL / API key
// from production is a valid use case ("score gpt-4.1-mini's
// output using gpt-4o").
//
// Production-safe defaults: 30s timeout, 256-rune body preview,
// temperature 0 for stable scoring.
type HTTPJudge struct {
	baseURL          string
	apiKey           string
	model            string
	client           *http.Client
	timeout          time.Duration
	bodyPreviewRunes int
}

// HTTPJudgeOptions 配置 HTTPJudge 的上游端点与请求约束；空字段使用包内默认值。
type HTTPJudgeOptions struct {
	BaseURL          string
	APIKey           string
	Model            string
	HTTPClient       *http.Client
	Timeout          time.Duration
	BodyPreviewRunes int
}

// NewHTTPJudge 构造一个 HTTPJudge，对缺失的可选字段补充安全默认值。
func NewHTTPJudge(opts HTTPJudgeOptions) *HTTPJudge {
	client := opts.HTTPClient
	if client == nil {
		client = fetcher.NewHardenedHTTPClient()
	}
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	preview := opts.BodyPreviewRunes
	if preview <= 0 {
		preview = 800
	}
	return &HTTPJudge{
		baseURL:          strings.TrimRight(opts.BaseURL, "/"),
		apiKey:           strings.TrimSpace(opts.APIKey),
		model:            strings.TrimSpace(opts.Model),
		client:           client,
		timeout:          timeout,
		bodyPreviewRunes: preview,
	}
}

const judgeSystemPrompt = `你是知识库内容质量评审员。给定 URL、标题、正文片段、摘要画像与要求、生成的 summary 和 tag 列表，分别按 1-5 分评价摘要和标签。

摘要评分同时考虑：
- 忠实：只陈述正文可支持的信息，不补写动机、重要性或结论
- 自然：像人写的简介，不使用“背景/问题”“关键过程”“明确结论/结果”等报告模板
- 密度：优先保留核心观点和关键事实，不复述完整过程，不空泛
- 画像符合度：满足给定长度、句数、段落或项目符号要求
- 若正文片段为空，先尝试访问 URL 核对原文；若仍无法访问，summary_score 不得高于 2，并在理由中明确说明无法核验忠实性

标签评分同时考虑：
- 精准覆盖核心主题，使用具体概念、实体或技术名
- 避免“技术 / AI / 互联网”一类无信息量宽泛词
- 不包含与正文无关或臆造的标签

5 = 准确、自然、完整满足要求
4 = 整体良好，仅有一个轻微问题
3 = 可用，但有明显遗漏、宽泛或表达问题
2 = 多数内容不准确、不自然或不符合要求
1 = 空内容、严重失真或几乎完全不可用

score 是摘要与标签的总体质量分。严格按以下 JSON 输出，不要其他内容：
{"score": 1-5 的整数, "summary_score": 1-5 的整数, "tag_score": 1-5 的整数, "reason": "30 字以内，同时概括摘要与标签的理由"}`

// JudgeCell sends one cell to the LLM and returns its verdict.
// Errors bubble up to the runner which records them in the cell's
// JudgeReason field as "error: ..." so the report row makes it
// obvious which side failed.
func (j *HTTPJudge) JudgeCell(ctx context.Context, in JudgeInput) (JudgeVerdict, error) {
	if j.baseURL == "" || j.apiKey == "" || j.model == "" {
		return JudgeVerdict{}, fmt.Errorf("judge: BaseURL / APIKey / Model required")
	}

	preview := in.BodyPreview
	if r := []rune(preview); len(r) > j.bodyPreviewRunes {
		preview = string(r[:j.bodyPreviewRunes])
	}
	userPrompt := fmt.Sprintf(
		"URL: %s\n标题: %s\n正文片段:\n%s\n\n摘要画像: %s\n摘要要求: %s\nsummary: %s\ntags: %s",
		in.URL,
		coalesce(in.Title, "（无标题）"),
		coalesce(preview, "（无正文）"),
		coalesce(in.SummaryProfile, "（未指定）"),
		coalesce(in.SummaryRequirements, "（未指定）"),
		coalesce(in.Summary, "（无 summary）"),
		fmtTagsCSV(in.Tags),
	)

	payload := map[string]any{
		"model": j.model,
		"messages": []map[string]any{
			{"role": "system", "content": judgeSystemPrompt},
			{"role": "user", "content": userPrompt},
		},
		"max_tokens":  180,
		"temperature": 0.0,
		"stream":      false,
	}
	body, err := jsonx.Marshal(payload)
	if err != nil {
		return JudgeVerdict{}, fmt.Errorf("judge marshal: %w", err)
	}

	reqCtx, cancel := context.WithTimeout(ctx, j.timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, j.baseURL+"/chat/completions", strings.NewReader(string(body)))
	if err != nil {
		return JudgeVerdict{}, fmt.Errorf("judge request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+j.apiKey)

	resp, err := j.client.Do(req)
	if err != nil {
		err = fetcher.SanitizeSecurityError(err)
		return JudgeVerdict{}, fmt.Errorf("judge call: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return JudgeVerdict{}, fmt.Errorf("judge HTTP %d", resp.StatusCode)
	}

	var decoded struct {
		Choices []struct {
			Message struct {
				Content jsonx.RawMessage `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := jsonx.NewDecoder(resp.Body).Decode(&decoded); err != nil {
		return JudgeVerdict{}, fmt.Errorf("judge decode: %w", err)
	}
	if len(decoded.Choices) == 0 {
		return JudgeVerdict{}, fmt.Errorf("judge: empty choices")
	}

	// content may be a JSON string ("{...}") or a JSON-fenced string.
	// Mirror the analyzer/judge.go normalisation so model quirks
	// (markdown fence, extra whitespace) do not break parsing.
	raw, err := decodeJudgeContent(decoded.Choices[0].Message.Content)
	if err != nil {
		return JudgeVerdict{}, fmt.Errorf("judge content: %w", err)
	}

	return parseJudgeVerdict(raw)
}

func parseJudgeVerdict(raw string) (JudgeVerdict, error) {
	raw = trimJudgeMarkdownWrapper(raw)

	var decoded struct {
		Score        any    `json:"score"`
		SummaryScore any    `json:"summary_score"`
		TagScore     any    `json:"tag_score"`
		Reason       string `json:"reason"`
	}
	if err := jsonx.Unmarshal([]byte(raw), &decoded); err != nil {
		return JudgeVerdict{}, fmt.Errorf("decode verdict %q: %w", raw, err)
	}
	score, ok := coerceScore(decoded.Score)
	if !ok {
		return JudgeVerdict{}, fmt.Errorf("judge score field is not numeric: %v", decoded.Score)
	}
	score = clampJudgeScore(score)
	summaryScore, err := optionalJudgeScore(decoded.SummaryScore, "summary_score")
	if err != nil {
		return JudgeVerdict{}, err
	}
	tagScore, err := optionalJudgeScore(decoded.TagScore, "tag_score")
	if err != nil {
		return JudgeVerdict{}, err
	}
	reason := strings.TrimSpace(decoded.Reason)
	if r := []rune(reason); len(r) > 200 {
		reason = string(r[:200])
	}
	return JudgeVerdict{
		Score:        score,
		SummaryScore: summaryScore,
		TagScore:     tagScore,
		Reason:       reason,
	}, nil
}

func trimJudgeMarkdownWrapper(raw string) string {
	raw = strings.TrimSpace(raw)
	for {
		before := raw
		switch {
		case strings.HasPrefix(raw, "```json") && strings.HasSuffix(raw, "```"):
			raw = strings.TrimSuffix(strings.TrimPrefix(raw, "```json"), "```")
		case strings.HasPrefix(raw, "```") && strings.HasSuffix(raw, "```"):
			raw = strings.TrimSuffix(strings.TrimPrefix(raw, "```"), "```")
		case strings.HasPrefix(raw, "**") && strings.HasSuffix(raw, "**") && len(raw) >= 4:
			raw = strings.TrimSuffix(strings.TrimPrefix(raw, "**"), "**")
		case strings.HasPrefix(raw, "__") && strings.HasSuffix(raw, "__") && len(raw) >= 4:
			raw = strings.TrimSuffix(strings.TrimPrefix(raw, "__"), "__")
		}
		raw = strings.TrimSpace(raw)
		if raw == before {
			return raw
		}
	}
}

func optionalJudgeScore(value any, field string) (float64, error) {
	if value == nil {
		return 0, nil
	}
	score, ok := coerceScore(value)
	if !ok {
		return 0, fmt.Errorf("judge %s field is not numeric: %v", field, value)
	}
	return clampJudgeScore(score), nil
}

func clampJudgeScore(score float64) float64 {
	if score < 1 {
		return 1
	}
	if score > 5 {
		return 5
	}
	return score
}

func coerceScore(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case int:
		return float64(x), true
	case int64:
		return float64(x), true
	case string:
		// Some chatty models return "score":"4"; tolerate.
		var f float64
		_, err := fmt.Sscanf(strings.TrimSpace(x), "%f", &f)
		if err != nil {
			return 0, false
		}
		return f, true
	}
	return 0, false
}

// decodeJudgeContent unwraps the OpenAI message content. Most servers
// return a string; a few wrap it in an array of {type:text,text:...}.
// We handle both so the judge prompt does not need to specify a
// response_format the upstream may not support.
func decodeJudgeContent(raw jsonx.RawMessage) (string, error) {
	if len(raw) == 0 {
		return "", fmt.Errorf("empty content")
	}
	// Try plain string first.
	var s string
	if err := jsonx.Unmarshal(raw, &s); err == nil {
		return s, nil
	}
	// Try OpenAI vision-style array.
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := jsonx.Unmarshal(raw, &parts); err == nil {
		var b strings.Builder
		for _, p := range parts {
			if p.Type == "text" {
				b.WriteString(p.Text)
			}
		}
		return b.String(), nil
	}
	return "", fmt.Errorf("content is neither string nor part array")
}

func coalesce(s, fallback string) string {
	if strings.TrimSpace(s) == "" {
		return fallback
	}
	return s
}

func fmtTagsCSV(tags []string) string {
	if len(tags) == 0 {
		return "（无 tag）"
	}
	return strings.Join(tags, ", ")
}
