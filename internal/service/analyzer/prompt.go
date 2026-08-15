package analyzer

import (
	"fmt"
	"strings"
	"unicode"

	"webtag/internal/fetcher"
	"webtag/internal/model"
	"webtag/internal/security"
	"webtag/internal/summarypolicy"
	"webtag/internal/textutil"
)

const maxAnalysisImages = 3

const bodyOmissionMarker = "\n\n[中间内容已省略]\n\n"

var contentTypeHints = map[string]string{
	"article":  "这是文章/内容页，tags 侧重核心主题、具体对象与方法。",
	"listing":  "这是列表/分类页，tags 侧重栏目领域和内容类型。",
	"homepage": "这是网站主页，tags 侧重网站类型和行业领域。",
}

const outputContract = "你是内容分析助手。严格按以下 JSON 格式输出，不输出任何其他内容：\n" +
	"{\"title\": \"便于浏览的短标题\", \"summary\": \"自然、精炼的中文摘要\", \"tags\": [\"标签1\", \"标签2\"]}\n\n" +
	"要求："

func libraryOutputContract(requested model.RequestedLibraryKind) string {
	kindInstruction := "自动判断 reading 或 site。"
	if requested == model.RequestedLibraryKindReading {
		kindInstruction = "用户已选择 reading，library_kind 必须为 reading。"
	}
	if requested == model.RequestedLibraryKindSite {
		kindInstruction = "用户已选择 site，library_kind 必须为 site。"
	}
	return "网站收藏 v2 输出：" + kindInstruction + "严格输出 schema_version=2 的 JSON。reading 使用 {\"schema_version\":2,\"library_kind\":\"reading\",\"classification_confidence\":0.0,\"classification_reason\":\"reason_code\",\"classification_explanation\":\"简短解释\",\"reading_profile\":{\"title\":\"标题\",\"summary\":\"摘要\",\"tags\":[\"标签\"]}}；site 使用 {\"schema_version\":2,\"library_kind\":\"site\",\"classification_confidence\":0.0,\"classification_reason\":\"reason_code\",\"classification_explanation\":\"简短解释\",\"site_profile\":{\"name\":\"网站名\",\"intro\":\"60-120字简介\",\"entry_name\":\"入口名\",\"purpose\":\"20-50字用途\",\"tags\":[\"标签\"]}}。site 不得生成 summary。"
}

// interstitialGuardInstructions gates the whole analysis on the body being
// real content. The fetchers only catch hard failures (transport errors,
// jina's soft-200 error pages via jinaSoftFailure); a target that happily
// serves a Cloudflare challenge, a login wall, or a bare cookie banner
// reaches the model as ordinary body text, and nothing in the tag rules
// stops it from inventing tags for that page.
//
// Empty tags are the part that matters. title/summary are per-link and a
// wrong one stays wrong on one link, but tags are resolved into the shared
// concept vocabulary — one "人机验证" concept then shows up as a retrieval
// candidate for every link parsed afterwards, and goes on to compete in
// merge proposals. The guard is stated as overriding the tag-count rules
// because those demand 3-5 (free) / 2-4 (retrieve-select) tags and would
// otherwise contradict it.
const interstitialGuardInstructions = "- 先判断正文是不是真实内容。若正文主体属于下列拦截页 / 占位页之一：\n" +
	"  ① 错误页：404、403、5xx、DNS 解析失败、请求超时、服务不可用\n" +
	"  ② 人机校验：Cloudflare 验证、CAPTCHA、机器人检查、浏览器校验、访问被拒绝\n" +
	"  ③ 墙：登录墙、付费墙、订阅提示、年龄确认\n" +
	"  ④ 边角料：Cookie 同意、GDPR 提示、纯导航菜单、空白页\n" +
	"  则 tags 必须输出空数组 []；所有文本字段（summary / intro / purpose / name 等，以本次要求的输出格式为准）只如实说明这是哪一类拦截页，\n" +
	"  不得依据 URL、标题或站点名推断、补全或编造正文内容。\n" +
	"  该规则优先于下面所有关于标签数量的要求——宁可不打标签，也不要给拦截页本身打标签。"

const tagCentralityInstructions = "  ⑤ 只选贯穿全文核心主题、且能由最终摘要核心信息直接支持的概念；章节举例或顺带提及的名词不能成为标签。\n" +
	"  ⑥ 作者名、账号名、站点名和来源名默认不是主题标签；除非内容本身就在研究该对象，否则改选其主题。\n" +
	"  ⑦ 优先复用已有规范写法；不要为已有概念另造同义标签，也不要同时输出同义标签。\n" +
	"  例：API 设计文章即使提到 REST/GraphQL，也应选 API、接口设计、向后兼容、幂等性等主线概念；综合周刊应让标签覆盖主文与栏目范围，不要全部来自头条。\n"

const freeTagInstructions = "- tags：3-5个，中英文均可。每个标签必须是**短、单一概念、可复用的归类词**（名词或专有名词），用于跨内容归类，而不是对这篇内容的描述。\n" +
	"  硬性规则（违反则标签作废）：\n" +
	"  ① 不含空格——不要短语，把短语拆成独立概念（错误：\"Go error handling\" → 正确：\"Go\"、\"错误处理\"）\n" +
	"  ② 不含斜杠或层级符（错误：\"check/handle\" → 正确：\"错误处理\"）\n" +
	"  ③ 尽量 ≤6 个字/词，越短越好\n" +
	"  ④ 必须能被其它同类内容复用，禁止文章专属短语或标题碎片（错误：\"try proposal\"、\"Never Gonna Give You Up\"）\n" +
	tagCentralityInstructions +
	"  好标签示例：Go、AI、API、接口设计、错误处理、量化交易、科技周刊、开发工具\n" +
	"  坏标签示例：空泛分类词（技术、互联网、其他、综合）、带空格短语、标题碎片"

const retrieveTagInstructions = "- tags：优先从下方【候选标签】中选择 2-4 个最贴切的标签，**原样照抄候选标签的文字**，不要改写。\n" +
	"  只有当候选标签都明显不贴切时，才允许新增**至多 1 个**新标签。\n" +
	"  新增的标签必须是**短、单一概念、可复用的归类词**，遵守：① 不含空格（短语拆成独立概念）；② 不含斜杠或层级符；③ 尽量 ≤6 个字/词；④ 能被其它同类内容复用，禁止文章专属短语或标题碎片。\n" +
	tagCentralityInstructions +
	"  好标签示例：Go、AI、API、接口设计、错误处理、量化交易、科技周刊、开发工具\n" +
	"  坏标签示例：空泛分类词（技术、互联网、其他、综合）、带空格短语（\"try proposal\"）、层级符（\"check/handle\"）"

const urlDirectOutputContract = "你是内容分析助手，具备联网抓取能力。请抓取用户给出的 URL 的真实网页内容后分析。\n" +
	"严格按以下 JSON 格式输出，不输出任何其他内容：\n" +
	"{\"accessible\": true, \"title\": \"便于浏览的短标题\", \"summary\": \"自然、精炼的中文摘要\", \"tags\": [\"标签1\", \"标签2\"]}\n\n" +
	"要求：\n" +
	"- 先真实抓取该 URL。若无法访问（登录墙/反爬/404/超时），输出 {\"accessible\": false} 即可，不要编造内容。"

var titleInstructions = fmt.Sprintf("- title：根据内容主题生成便于稍后浏览的短标题，不超过 %d 个 Unicode 字符。不要直接复制网页 title、作者名、整段正文或摘要；社交帖子要概括分享对象或核心主题，不要带引号、序号、站点后缀（如 / X）或句号。必须保留原文的事实限定，例如暂缓、计划、可能、可预见的未来；不得把阶段性决定改写成永久停止。", textutil.MaxTitleRunes)

func summaryInstructions(in summarypolicy.Input) string {
	return "- summary：" + summarypolicy.For(in).PromptInstructions() + "\n" +
		"  写成给自己稍后重读的阅读笔记：开头直接说明内容是什么或核心观点，只保留最有助于理解和判断的信息，不重复标题，不使用宣传腔或报告腔。\n" +
		"  不要输出“背景/问题”“关键过程”“明确结论/结果”“核心结论”等模板标题，不要使用 Markdown 标题，不要加粗，不要暴露写作指令。\n" +
		"  严格依据提供或抓取到的内容客观转述；原文没有给出原因、价值或结论时不要强行补充，信息不足就明确保持有限。\n" +
		"  严格区分原作者、编者或转载者，以及“可能由 AI 生成”等推测，不要把不同主体和不确定信息合并成确定事实。\n" +
		"  示例产品、公司或人物只能作为例子，不得改写成作者参与构建、任职或拥有；不得把 CEO 等已知职位升级为创始人。\n" +
		"  中文摘要和标签优先使用自然、通行中文术语，例如 idempotency → 幂等性、cursor pagination → 游标分页；专有名词和无通行译名的术语保留原文。\n" +
		"  输出前按上述内容类型要求检查覆盖范围；若遗漏最终决定、重要后续章节或周刊其他栏目，先重写摘要再输出 JSON。"
}

// buildURLDirectPrompt composes the grok-direct system prompt: the
// fetch-and-analyze base instructions plus an optional candidate-tag block
// (same retrieve-then-select reuse hint as the fetched-content path) and the
// content-type hint. Candidates/contentType are URL-derived so they are
// available even before any fetch.
func buildURLDirectPrompt(candidates []string, contentType string) string {
	return buildURLDirectPromptFor(nil, candidates, summarypolicy.Input{ContentType: contentType})
}

func buildURLDirectPromptFor(existingTags, candidates []string, in summarypolicy.Input) string {
	tagInstructions := freeTagInstructions
	if len(candidates) > 0 {
		tagInstructions = retrieveTagInstructions
	}
	// No interstitial guard here, deliberately. URL-direct already has a
	// stronger, purpose-built contract for exactly these pages:
	// urlDirectOutputContract tells the model to answer {"accessible": false}
	// when the fetch hits a login wall / anti-bot / 404, and runURLDirect
	// turns that into a fall back to the local fetcher — which then gets a
	// real shot at the content. The guard's "describe the interstitial and
	// return empty tags" would satisfy validateAnalysisResponse with
	// Accessible defaulting to true, so the run would finalise a link titled
	// "Just a moment..." instead of retrying. Overlapping triggers with
	// opposite required outputs is a net regression, so the accessible
	// contract stays the sole authority on this path.
	parts := []string{urlDirectOutputContract, titleInstructions, summaryInstructions(in), tagInstructions}
	if len(candidates) > 0 {
		parts = append(parts, "【候选标签】（优先从中原样选择，最多新增 1 个）："+strings.Join(candidates, "、"))
	} else if len(existingTags) > 0 {
		parts = append(parts, "已有标签库（优先复用相关的规范写法）："+strings.Join(existingTags, "、"))
	}
	if hint := contentTypeHints[in.ContentType]; hint != "" {
		parts = append(parts, hint)
	}
	return strings.Join(parts, "\n")
}

// buildSystemPrompt composes the base instructions, an optional reuse hint
// for known tags, and a content-type hint. Empty inputs are skipped so the
// resulting prompt stays compact.
func buildSystemPrompt(existingTags []string, contentType string) string {
	return buildSystemPromptFor(existingTags, summarypolicy.Input{ContentType: contentType})
}

func buildSystemPromptFor(existingTags []string, in summarypolicy.Input) string {
	parts := []string{outputContract, interstitialGuardInstructions, titleInstructions, summaryInstructions(in), freeTagInstructions}
	if len(existingTags) > 0 {
		parts = append(parts, "已有标签库（优先从中选择相关的）："+strings.Join(existingTags, "、"))
	}
	if hint := contentTypeHints[in.ContentType]; hint != "" {
		parts = append(parts, hint)
	}
	return strings.Join(parts, "\n")
}

// buildRetrieveSelectPrompt composes the v3 retrieve-then-select system
// prompt: the selection-mode base instructions, the candidate concept
// list (one comma-separated block), and the same content-type hint the
// free-generation prompt uses. Callers only reach this with a non-empty
// candidate slice (the pipeline gates on len(candidates) > 0), but an
// empty list still degrades to the base prompt without a candidate block
// so the function is safe to call unconditionally.
func buildRetrieveSelectPrompt(candidates []string, contentType string) string {
	return buildRetrieveSelectPromptFor(candidates, summarypolicy.Input{ContentType: contentType})
}

func buildRetrieveSelectPromptFor(candidates []string, in summarypolicy.Input) string {
	parts := []string{outputContract, interstitialGuardInstructions, titleInstructions, summaryInstructions(in), retrieveTagInstructions}
	if len(candidates) > 0 {
		parts = append(parts, "【候选标签】（优先从中原样选择）："+strings.Join(candidates, "、"))
	}
	if hint := contentTypeHints[in.ContentType]; hint != "" {
		parts = append(parts, hint)
	}
	return strings.Join(parts, "\n")
}

// buildUserMessageContent returns either the plain text prompt (no images)
// or an OpenAI vision-style content array mixing the text with one
// image_url part per supplied URL. Empty / whitespace-only image URLs
// are filtered out so a stray empty string does not become a `null` part.
func (a *OpenAIAnalyzer) buildUserMessageContent(prompt string, imageURLs []string) any {
	if len(imageURLs) == 0 {
		return prompt
	}

	content := make([]map[string]any, 0, maxAnalysisImages+1)
	content = append(content, map[string]any{
		"type": "text",
		"text": prompt,
	})

	for _, imageURL := range imageURLs {
		imageURL = strings.TrimSpace(imageURL)
		if imageURL == "" {
			continue
		}
		content = append(content, map[string]any{
			"type": "image_url",
			"image_url": map[string]any{
				"url":    imageURL,
				"detail": "auto",
			},
		})
		if len(content) == maxAnalysisImages+1 {
			break
		}
	}

	if len(content) == 1 {
		return prompt
	}
	return content
}

// buildURLDirectUserPrompt is the user message for grok-direct mode: just
// the URL the model must fetch, plus the optional user note. No title/body
// (the model fetches those itself) and no image parts.
func (a *OpenAIAnalyzer) buildURLDirectUserPrompt(req AnalyzeRequest) string {
	descriptionLine := ""
	if req.UserDescription != nil && strings.TrimSpace(*req.UserDescription) != "" {
		descriptionLine = "\n用户备注: " + strings.TrimSpace(*req.UserDescription)
	}
	projectedURL, _ := security.ThirdPartyURLProjection(req.Content.URL)
	return "请抓取并分析该 URL: " + projectedURL + descriptionLine
}

// buildUserPrompt assembles the URL / title / description / body block fed
// to the model. The body is rune-truncated to bodyPreviewChars so an
// over-long fetch does not push us past the model context window.
func (a *OpenAIAnalyzer) buildUserPrompt(req AnalyzeRequest) string {
	profile := summarypolicy.For(summarypolicy.Input{
		URL:         req.Content.URL,
		ContentType: req.ContentType,
		FetcherType: req.Content.FetcherType,
	})
	sections := documentSections(req.Content.Body)
	body := sampleBody(req.Content.Body, a.bodyPreviewChars)
	if profile.Name == "digest" {
		body = sampleDigestLead(req.Content.Body, a.bodyPreviewChars)
	}
	outline := sectionOutline(sections)
	previews := sectionPreviews(sections)

	descriptionLine := ""
	if req.UserDescription != nil && strings.TrimSpace(*req.UserDescription) != "" {
		descriptionLine = "用户备注: " + strings.TrimSpace(*req.UserDescription) + "\n"
	}

	title := normalizedPromptTitle(req.Content)
	if strings.TrimSpace(title) == "" {
		title = "（无标题）"
	}

	searchSection := ""
	if req.Content.Metadata != nil {
		if rawSearch, ok := req.Content.Metadata["search_summary"].(string); ok && strings.TrimSpace(rawSearch) != "" {
			searchSection = "\n\n辅助搜索上下文:\n" + strings.TrimSpace(rawSearch)
		}
	}
	outlineSection := ""
	if outline != "" {
		outlineSection = "\n文档结构概览（用于确保摘要覆盖全文，不要只总结第一节）：\n" + outline + "\n"
	}
	previewSection := ""
	if previews != "" {
		previewSection = "\n章节代表片段（用于看见靠后的栏目与建议）：\n" + previews + "\n"
	}
	coverageSection := ""
	if profile.Name == "digest" {
		coverageSection = "\n本条是完整周刊。摘要第一句概括主文；第二句必须以“此外，本期还”承接，并从结构概览点名至少2个其他栏目及其代表内容。\n"
	}

	projectedURL, _ := security.ThirdPartyURLProjection(req.Content.URL)
	return fmt.Sprintf("URL: %s\n标题: %s\n%s%s%s%s\n正文:\n%s%s", projectedURL, title, descriptionLine, outlineSection, previewSection, coverageSection, body, searchSection)
}

const maxDigestLeadRunes = 2200

func sampleDigestLead(body string, promptLimit int) string {
	limit := maxDigestLeadRunes
	if promptLimit > 0 && promptLimit < limit {
		limit = promptLimit
	}
	if runeCount(body) <= limit {
		return body
	}
	return truncateRunes(body, limit) + "\n\n[主文后续已压缩；其他栏目见上方章节代表片段]"
}

const maxOutlineHeadings = 24

type documentSection struct {
	title   string
	preview string
}

func documentSections(body string) []documentSection {
	lines := strings.Split(strings.ReplaceAll(body, "\r\n", "\n"), "\n")
	sections := make([]documentSection, 0, maxOutlineHeadings)
	seen := make(map[string]struct{}, maxOutlineHeadings)
	for i := range lines {
		title, ok := documentHeading(lines, i)
		if !ok {
			continue
		}
		key := strings.ToLower(title)
		if _, duplicate := seen[key]; duplicate {
			continue
		}
		seen[key] = struct{}{}
		preview := ""
		for j := i + 1; j < len(lines); j++ {
			candidate := strings.TrimSpace(lines[j])
			if candidate == "" {
				continue
			}
			if _, nextHeading := documentHeading(lines, j); nextHeading {
				break
			}
			preview = strings.Join(strings.Fields(candidate), " ")
			break
		}
		sections = append(sections, documentSection{title: title, preview: preview})
		if len(sections) == maxOutlineHeadings {
			break
		}
	}
	if len(sections) < 3 {
		return nil
	}
	return sections
}

func documentHeading(lines []string, i int) (string, bool) {
	line := strings.TrimSpace(lines[i])
	explicitMarkdown := strings.HasPrefix(line, "#")
	if explicitMarkdown {
		line = strings.TrimSpace(strings.TrimLeft(line, "#"))
	} else {
		previousBlank := i == 0 || strings.TrimSpace(lines[i-1]) == ""
		nextBlank := i == len(lines)-1 || strings.TrimSpace(lines[i+1]) == ""
		if !previousBlank || !nextBlank {
			return "", false
		}
	}
	if !headingLike(line) {
		return "", false
	}
	return line, true
}

func sectionOutline(sections []documentSection) string {
	if len(sections) < 3 {
		return ""
	}
	headings := make([]string, 0, len(sections))
	for _, section := range sections {
		headings = append(headings, section.title)
	}
	return "- " + strings.Join(headings, "\n- ")
}

func sectionPreviews(sections []documentSection) string {
	if len(sections) < 3 {
		return ""
	}
	lines := make([]string, 0, len(sections))
	for _, section := range sections {
		if section.preview == "" {
			continue
		}
		lines = append(lines, "- ["+section.title+"] "+truncateRunes(section.preview, 120))
	}
	return strings.Join(lines, "\n")
}

func headingLike(line string) bool {
	count := runeCount(line)
	if count < 2 || count > 72 || strings.Contains(line, "://") {
		return false
	}
	if strings.ContainsAny(line, "。！？!?；;。") || strings.HasSuffix(line, ".") || strings.HasSuffix(line, ":") || strings.HasSuffix(line, "：") {
		return false
	}
	if strings.HasPrefix(line, "-") || strings.HasPrefix(line, "*") || strings.HasPrefix(line, "+") {
		return false
	}
	first := []rune(line)[0]
	if unicode.IsDigit(first) {
		return false
	}
	for _, r := range line {
		if unicode.IsLetter(r) || unicode.In(r, unicode.Han) {
			return true
		}
	}
	return false
}

// sampleBody preserves the setup, a representative middle window, and the
// conclusion of long content within the existing prompt budget. Long articles
// often put their concrete guidance between an introductory thesis and a final
// recap, so a head/tail-only preview still hides load-bearing sections.
func sampleBody(body string, limit int) string {
	if limit <= 0 || runeCount(body) <= limit {
		if limit <= 0 {
			return ""
		}
		return body
	}
	markerRunes := runeCount(bodyOmissionMarker)
	if limit <= markerRunes*2+12 {
		if limit <= markerRunes+8 {
			return truncateRunes(body, limit)
		}
		available := limit - markerRunes
		headRunes := available * 2 / 3
		tailRunes := available - headRunes
		runes := []rune(body)
		return string(runes[:headRunes]) + bodyOmissionMarker + string(runes[len(runes)-tailRunes:])
	}

	available := limit - markerRunes*2
	headRunes := available / 4
	middleRunes := available / 4
	tailRunes := available - headRunes - middleRunes
	runes := []rune(body)
	middleStart := (len(runes) - middleRunes) / 2
	tailStart := len(runes) - tailRunes
	tailEnd := len(runes)
	if conclusionStart := findConclusionStart(body); conclusionStart > headRunes+middleRunes && conclusionStart+tailRunes <= len(runes) {
		middleStart = headRunes + (conclusionStart-headRunes-middleRunes)/2
		tailStart = conclusionStart
		tailEnd = conclusionStart + tailRunes
	}
	return string(runes[:headRunes]) + bodyOmissionMarker +
		string(runes[middleStart:middleStart+middleRunes]) + bodyOmissionMarker +
		string(runes[tailStart:tailEnd])
}

func findConclusionStart(body string) int {
	offset := 0
	for _, line := range strings.SplitAfter(body, "\n") {
		heading := strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(line), "#"))
		heading = strings.TrimSpace(strings.TrimSuffix(strings.TrimSuffix(heading, ":"), "："))
		switch strings.ToLower(heading) {
		case "summary", "conclusion", "conclusions", "总结", "结论", "结语":
			return runeCount(body[:offset])
		}
		offset += len(line)
	}
	return -1
}

// normalizedPromptTitle prefers a non-generic title from Content.Title; if
// that is generic (e.g. "Untitled"), it tries best_title and search_title
// metadata before falling back to whatever was originally set.
func normalizedPromptTitle(content fetcher.Content) string {
	title := strings.TrimSpace(content.Title)
	if !textutil.IsGenericTitle(title) {
		return title
	}
	if content.Metadata != nil {
		if bestTitle, ok := content.Metadata["best_title"].(string); ok && !textutil.IsGenericTitle(bestTitle) {
			return strings.TrimSpace(bestTitle)
		}
		if searchTitle, ok := content.Metadata["search_title"].(string); ok && !textutil.IsGenericTitle(searchTitle) {
			return strings.TrimSpace(searchTitle)
		}
	}
	return title
}
