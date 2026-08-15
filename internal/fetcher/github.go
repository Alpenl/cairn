package fetcher

import (
	"context"
	"fmt"
	"io"
	"net/http"
	nurl "net/url"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"webtag/internal/errsafe"
	"webtag/internal/jsonx"
)

var (
	githubURLPattern  = regexp.MustCompile(`^https?://(?:www\.)?github\.com/`)
	githubRepoPattern = regexp.MustCompile(`^https?://(?:www\.)?github\.com/([^/]+)/([^/?#]+?)/?(?:[?#].*)?$`)
)

var reservedGitHubFirstSegments = map[string]struct{}{
	"about":         {},
	"apps":          {},
	"collections":   {},
	"customers":     {},
	"enterprise":    {},
	"events":        {},
	"explore":       {},
	"features":      {},
	"issues":        {},
	"login":         {},
	"marketplace":   {},
	"notifications": {},
	"orgs":          {},
	"organizations": {},
	"pricing":       {},
	"pulls":         {},
	"search":        {},
	"sessions":      {},
	"settings":      {},
	"signup":        {},
	"sponsors":      {},
	"team":          {},
	"topics":        {},
	"trending":      {},
}

const (
	defaultGitHubTimeout        = 10 * time.Second
	defaultGitHubReadmeMaxChars = 6000
	defaultGitHubAPIBase        = "https://api.github.com"
	defaultGitHubRepoMaxBytes   = 1 << 20
)

// GitHubFetcher 通过 GitHub REST API 抓取仓库元信息和 README，输出比直接爬 HTML 更稳定的结构化结果。
type GitHubFetcher struct {
	client             *HTTPClient
	Timeout            time.Duration
	APIBaseURL         string
	Token              string
	ReadmeMaxChars     int
	RepositoryMaxBytes int64
}

// NewGitHubFetcher constructs a GitHubFetcher. Pass an empty token when no
// GitHub credential is configured — the fetcher will still operate against
// public endpoints but will be subject to GitHub's anonymous rate limit.
// Reading the token from the environment is intentionally not done here so
// configuration stays centralized in internal/config.
func NewGitHubFetcher(client *HTTPClient, token string) *GitHubFetcher {
	return &GitHubFetcher{
		client:             ensureHTTPClient(client),
		Timeout:            defaultGitHubTimeout,
		APIBaseURL:         defaultGitHubAPIBase,
		Token:              strings.TrimSpace(token),
		ReadmeMaxChars:     defaultGitHubReadmeMaxChars,
		RepositoryMaxBytes: defaultGitHubRepoMaxBytes,
	}
}

// CanHandle 只在 URL 形如 github.com/{owner}/{repo} 且 owner 不是 GitHub 保留路径（如 settings/issues）时返回 true。
func (f *GitHubFetcher) CanHandle(url string) bool {
	if !githubURLPattern.MatchString(url) {
		return false
	}

	parsed, err := nurl.Parse(url)
	if err != nil {
		return false
	}

	segments := splitGitHubPath(parsed.Path)
	if len(segments) != 2 {
		return false
	}

	_, reserved := reservedGitHubFirstSegments[strings.ToLower(segments[0])]
	return !reserved
}

// Fetch 调 GitHub REST API 取仓库信息和 README，拼成 Markdown 风格的 Body 返回。
func (f *GitHubFetcher) Fetch(ctx context.Context, url string) (Content, error) {
	matches := githubRepoPattern.FindStringSubmatch(url)
	if len(matches) < 3 {
		return Content{}, &FetchError{URL: url, Reason: "non-repository GitHub URL"}
	}

	owner := matches[1]
	repo := strings.TrimSuffix(matches[2], ".git")

	repoData, err := f.fetchRepository(ctx, owner, repo, url)
	if err != nil {
		return Content{}, err
	}

	readme := f.fetchReadme(ctx, owner, repo)

	parts := []string{fmt.Sprintf("# %s/%s", owner, repo)}
	if repoData.Description != "" {
		parts = append(parts, repoData.Description)
	}

	metaLine := make([]string, 0, 3)
	if repoData.Language != "" {
		metaLine = append(metaLine, fmt.Sprintf("Language: %s", repoData.Language))
	}
	metaLine = append(metaLine, fmt.Sprintf("Stars: %d Forks: %d", repoData.StargazersCount, repoData.ForksCount))
	if len(repoData.Topics) > 0 {
		metaLine = append(metaLine, "Topics: "+strings.Join(repoData.Topics, ", "))
	}
	if len(metaLine) > 0 {
		parts = append(parts, strings.Join(metaLine, "  "))
	}
	if readme != "" {
		parts = append(parts, "## README\n"+readme)
	}

	return Content{
		URL:   url,
		Title: firstNonEmpty(repoData.FullName, owner+"/"+repo),
		Body:  strings.Join(parts, "\n\n"),
		Metadata: map[string]any{
			"language": repoData.Language,
			"stars":    repoData.StargazersCount,
			"topics":   repoData.Topics,
		},
		FetcherType: "github",
	}, nil
}

func splitGitHubPath(path string) []string {
	path = strings.Trim(path, "/")
	if path == "" {
		return nil
	}

	parts := strings.Split(path, "/")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}

type githubRepository struct {
	FullName        string   `json:"full_name"`
	Description     string   `json:"description"`
	Language        string   `json:"language"`
	Topics          []string `json:"topics"`
	StargazersCount int      `json:"stargazers_count"`
	ForksCount      int      `json:"forks_count"`
}

func (f *GitHubFetcher) fetchRepository(ctx context.Context, owner string, repo string, rawURL string) (githubRepository, error) {
	target := fmt.Sprintf("%s/repos/%s/%s", strings.TrimRight(f.APIBaseURL, "/"), owner, repo)
	req, cancel, err := f.client.NewRequest(ctx, f.Timeout, http.MethodGet, target, nil)
	if err != nil {
		return githubRepository{}, &FetchError{URL: rawURL, Reason: "build GitHub request failed", Err: err}
	}
	defer cancel()

	f.setAPIHeaders(req)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	resp, err := f.client.DoWithRetry(req)
	if err != nil {
		return githubRepository{}, &FetchError{URL: rawURL, Reason: "GitHub API request failed", Err: err}
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusNotFound:
		return githubRepository{}, &FetchError{URL: rawURL, Reason: "GitHub repository not found or private", Err: errsafe.ErrUpstreamHTTP}
	case http.StatusForbidden, http.StatusTooManyRequests:
		return githubRepository{}, &FetchError{URL: rawURL, Reason: "GitHub API rate limited", Err: errsafe.ErrUpstreamHTTP}
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return githubRepository{}, &FetchError{URL: rawURL, Reason: fmt.Sprintf("GitHub API HTTP %d", resp.StatusCode), Err: errsafe.ErrUpstreamHTTP}
	}
	contentType := strings.ToLower(strings.TrimSpace(resp.Header.Get("Content-Type")))
	if contentType != "" && !strings.Contains(contentType, "application/json") {
		return githubRepository{}, &FetchError{URL: rawURL, Reason: "GitHub repository response content-type is not JSON", Err: errsafe.ErrContentType}
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, f.RepositoryMaxBytes+1))
	if err != nil {
		return githubRepository{}, &FetchError{URL: rawURL, Reason: "read GitHub response failed", Err: err}
	}
	if f.RepositoryMaxBytes > 0 && int64(len(body)) > f.RepositoryMaxBytes {
		return githubRepository{}, &FetchError{URL: rawURL, Reason: "GitHub repository response too large", Err: errsafe.ErrResponseTooLarge}
	}

	var repoData githubRepository
	if err := jsonx.Unmarshal(body, &repoData); err != nil {
		return githubRepository{}, &FetchError{URL: rawURL, Reason: "decode GitHub response failed", Err: err}
	}
	return repoData, nil
}

func (f *GitHubFetcher) fetchReadme(ctx context.Context, owner string, repo string) string {
	target := fmt.Sprintf("%s/repos/%s/%s/readme", strings.TrimRight(f.APIBaseURL, "/"), owner, repo)
	req, cancel, err := f.client.NewRequest(ctx, f.Timeout, http.MethodGet, target, nil)
	if err != nil {
		return ""
	}
	defer cancel()

	f.setAPIHeaders(req)
	req.Header.Set("Accept", "application/vnd.github.raw+json")

	resp, err := f.client.DoWithRetry(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()

	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return ""
	}
	contentType := strings.ToLower(strings.TrimSpace(resp.Header.Get("Content-Type")))
	if contentType != "" &&
		!strings.Contains(contentType, "text/") &&
		!strings.Contains(contentType, "markdown") &&
		!strings.Contains(contentType, "json") &&
		!strings.Contains(contentType, "xml") {
		return ""
	}

	data, err := io.ReadAll(io.LimitReader(resp.Body, int64(f.ReadmeMaxChars*utf8.UTFMax)+1))
	if err != nil {
		return ""
	}

	return limitTextByRunes(string(data), f.ReadmeMaxChars)
}

func (f *GitHubFetcher) setAPIHeaders(req *http.Request) {
	req.Header.Set("User-Agent", defaultUserAgent)
	if f.Token != "" {
		req.Header.Set("Authorization", "Bearer "+f.Token)
	}
}
