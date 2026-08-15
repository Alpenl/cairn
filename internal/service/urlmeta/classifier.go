package urlmeta

import (
	"regexp"
	"strings"

	"webtag/internal/model"
)

var (
	numericSegmentRE = regexp.MustCompile(`^\d+$`)
	dateSegmentRE    = regexp.MustCompile(`^(19|20)\d{2}[-/]\d{1,2}([-/]\d{1,2})?$`)
	datePartRE       = regexp.MustCompile(`^(19|20)\d{2}$`)
	slugSegmentRE    = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+){2,}$`)
	shortSlugRE      = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)+$`)
	weeklyIssueRE    = regexp.MustCompile(`^issue-\d+(\.(md|markdown))?$`)
)

var articleParentKeywords = map[string]struct{}{
	"article":  {},
	"articles": {},
	"blog":     {},
	"blogs":    {},
	"news":     {},
	"post":     {},
	"posts":    {},
}

var listingKeywords = map[string]struct{}{
	"archive":    {},
	"archives":   {},
	"articles":   {},
	"blog":       {},
	"categories": {},
	"category":   {},
	"course":     {},
	"courses":    {},
	"forum":      {},
	"forums":     {},
	"gallery":    {},
	"index":      {},
	"list":       {},
	"news":       {},
	"page":       {},
	"posts":      {},
	"products":   {},
	"search":     {},
	"shop":       {},
	"store":      {},
	"tag":        {},
	"tags":       {},
	"topic":      {},
	"topics":     {},
}

// URLMetadata 是 ClassifyURL 的输出：基于 URL 字面量推断出的域名 / 内容类型
// / 路径深度 / 父路径，pipeline 用它构造链接树和落库字段。
type URLMetadata struct {
	Domain      string
	ContentType model.ContentType
	PathDepth   int
	ParentPath  string
}

// ClassifyURL 仅靠 URL 字符串推断内容类型，不发起网络请求。命中规则按顺序：
// 周刊 issue 文档 → listing；末段数字 / 日期 / 文章 slug → article；
// 命中 listing 关键词 → listing；空路径 → homepage；否则 unknown。
func ClassifyURL(rawURL string) URLMetadata {
	if rawURL == "" {
		return URLMetadata{
			ContentType: model.ContentTypeUnknown,
			ParentPath:  "/",
		}
	}

	parts, ok := splitRawURL(rawURL)
	if !ok {
		return URLMetadata{
			ContentType: model.ContentTypeUnknown,
			ParentPath:  "/",
		}
	}

	domain := strings.ToLower(parts.Netloc)
	trimmedPath := strings.TrimRight(parts.Path, "/")
	segments := splitPathSegments(trimmedPath)
	depth := len(segments)
	if depth == 0 {
		return URLMetadata{
			Domain:      domain,
			ContentType: model.ContentTypeHomepage,
			PathDepth:   0,
			ParentPath:  "/",
		}
	}

	parentPath := "/"
	if depth >= 2 {
		parentPath = "/" + strings.Join(segments[:depth-1], "/") + "/"
	}

	last := strings.ToLower(segments[depth-1])
	return URLMetadata{
		Domain:      domain,
		ContentType: inferContentType(segments, last, depth),
		PathDepth:   depth,
		ParentPath:  parentPath,
	}
}

// AncestorURLs 返回 rawURL 从根到自身的所有祖先 URL（不含 rawURL 自身），
// 用于 pipeline.ensureParent 自动补建中间层级链接行。maxDepth 控制最大返回
// 数量，避免病态长路径吞掉过多 DB 调用。
func AncestorURLs(rawURL string, maxDepth int) []string {
	parts, ok := splitRawURL(rawURL)
	if !ok || parts.Netloc == "" {
		return nil
	}

	segments := splitPathSegments(strings.TrimRight(parts.Path, "/"))
	if len(segments) == 0 || maxDepth <= 0 {
		return nil
	}

	base := parts.Scheme + "://" + parts.Netloc
	limit := min(len(segments), maxDepth)
	ancestors := make([]string, 0, limit)
	ancestors = append(ancestors, base+"/")
	for i := 1; i < limit; i++ {
		ancestors = append(ancestors, base+"/"+strings.Join(segments[:i], "/")+"/")
	}

	return ancestors
}

func inferContentType(segments []string, last string, depth int) model.ContentType {
	switch {
	case isWeeklyIssueDocument(segments, last):
		return model.ContentTypeListing
	case numericSegmentRE.MatchString(last):
		return model.ContentTypeArticle
	case dateSegmentRE.MatchString(last):
		return model.ContentTypeArticle
	case hasDatePart(segments) && !isListingKeyword(last):
		return model.ContentTypeArticle
	case slugSegmentRE.MatchString(last):
		return model.ContentTypeArticle
	case shortSlugRE.MatchString(last) && hasArticleParent(segments[:depth-1]):
		return model.ContentTypeArticle
	case isListingKeyword(last):
		return model.ContentTypeListing
	case depth == 1 && isListingKeyword(strings.ToLower(segments[0])):
		return model.ContentTypeListing
	default:
		return model.ContentTypeUnknown
	}
}

func isWeeklyIssueDocument(segments []string, last string) bool {
	if !weeklyIssueRE.MatchString(last) {
		return false
	}
	for _, segment := range segments[:len(segments)-1] {
		if strings.EqualFold(segment, "weekly") {
			return true
		}
	}
	return false
}

func hasArticleParent(segments []string) bool {
	for _, segment := range segments {
		if _, ok := articleParentKeywords[strings.ToLower(segment)]; ok {
			return true
		}
	}
	return false
}

func hasDatePart(segments []string) bool {
	for _, segment := range segments {
		if datePartRE.MatchString(segment) {
			return true
		}
	}
	return false
}

func isListingKeyword(segment string) bool {
	_, ok := listingKeywords[segment]
	return ok
}

func splitPathSegments(path string) []string {
	parts := strings.Split(path, "/")
	segments := make([]string, 0, len(parts))
	for _, part := range parts {
		if part == "" {
			continue
		}
		segments = append(segments, part)
	}
	return segments
}

type rawURLParts struct {
	Scheme string
	Netloc string
	Path   string
}

func splitRawURL(rawURL string) (rawURLParts, bool) {
	rawURL = strings.TrimSpace(rawURL)
	schemeIdx := strings.Index(rawURL, "://")
	if schemeIdx <= 0 {
		return rawURLParts{}, false
	}

	scheme := strings.ToLower(rawURL[:schemeIdx])
	rest := rawURL[schemeIdx+3:]
	if rest == "" {
		return rawURLParts{}, false
	}

	authorityEnd := strings.IndexAny(rest, "/?#")
	netloc := rest
	pathAndMore := ""
	if authorityEnd >= 0 {
		netloc = rest[:authorityEnd]
		pathAndMore = rest[authorityEnd:]
	}
	if netloc == "" {
		return rawURLParts{}, false
	}

	path := "/"
	if pathAndMore != "" {
		if pathAndMore[0] == '/' {
			path = pathAndMore
		}
		if cut := strings.IndexAny(path, "?#"); cut >= 0 {
			path = path[:cut]
		}
	}

	return rawURLParts{
		Scheme: scheme,
		Netloc: netloc,
		Path:   path,
	}, true
}

// HasArticleParentSegment 报告某个父路径里是否含「文章类」路径段
// （blog / posts / articles …）。
//
// 导出的是谓词而不是那张 map：library 分类器只需要这个判断，把 map 本身放出去
// 等于让调用方也能改它——一个包级 map 被外部写一次，所有 URL 的判定都会跟着变，
// 而且改的人不会知道自己动了什么。
func HasArticleParentSegment(parent string) bool {
	return hasArticleParent(strings.Split(strings.ToLower(parent), "/"))
}
