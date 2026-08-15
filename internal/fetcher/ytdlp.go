package fetcher

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os/exec"
	"regexp"
	"sort"
	"strings"
	"time"

	"webtag/internal/jsonx"
)

const (
	defaultYtdlpTimeout       = 30 * time.Second
	defaultYtdlpBodyMaxRunes  = 4000
	defaultYtdlpBinaryPath    = "yt-dlp"
	defaultYtdlpMaxOutputSize = 8 << 20 // 8 MiB cap on yt-dlp stdout
)

var ytdlpURLPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)^https?://(?:www\.|m\.)?youtube\.com/(?:watch\?v=|shorts/|live/|embed/)`),
	regexp.MustCompile(`(?i)^https?://youtu\.be/[^/?#]+`),
	regexp.MustCompile(`(?i)^https?://(?:www\.|player\.)?vimeo\.com/(?:\d+|video/\d+|channels/[^/]+/\d+)`),
	regexp.MustCompile(`(?i)^https?://(?:www\.|m\.)?bilibili\.com/video/`),
	regexp.MustCompile(`(?i)^https?://(?:www\.|go\.)?twitch\.tv/(?:videos/\d+|[^/]+/clip/)`),
}

// YtdlpFetcher 通过外部 yt-dlp 二进制抓取视频站点（YouTube / Vimeo / B 站 / Twitch 等）的元信息和描述。
type YtdlpFetcher struct {
	BinaryPath    string
	Timeout       time.Duration
	BodyMaxRunes  int
	MaxOutputSize int64
}

// NewYtdlpFetcher 构造 YtdlpFetcher；binaryPath 为空时使用 PATH 中的 "yt-dlp"，timeout<=0 时使用默认 30s。
func NewYtdlpFetcher(binaryPath string, timeout time.Duration) *YtdlpFetcher {
	if strings.TrimSpace(binaryPath) == "" {
		binaryPath = defaultYtdlpBinaryPath
	}
	if timeout <= 0 {
		timeout = defaultYtdlpTimeout
	}
	return &YtdlpFetcher{
		BinaryPath:    binaryPath,
		Timeout:       timeout,
		BodyMaxRunes:  defaultYtdlpBodyMaxRunes,
		MaxOutputSize: defaultYtdlpMaxOutputSize,
	}
}

// CanHandle 匹配已知视频平台的 URL 形态（YouTube watch/shorts、youtu.be、Vimeo、Bilibili、Twitch 等）。
func (f *YtdlpFetcher) CanHandle(url string) bool {
	for _, pattern := range ytdlpURLPatterns {
		if pattern.MatchString(url) {
			return true
		}
	}
	return false
}

// Fetch 调用 yt-dlp --dump-json --skip-download 仅取元数据，不拉视频流，解析后返回 Content。
//
//nolint:gocyclo // reason: 子进程编排 + JSON 解析 + 多元数据字段抽取的线性流程，拆函数会把 metaJSON / Content 在多 helper 间反复传递。
func (f *YtdlpFetcher) Fetch(ctx context.Context, url string) (Content, error) {
	timeout := f.Timeout
	if timeout <= 0 {
		timeout = defaultYtdlpTimeout
	}

	cmdCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// gosec G204：URL 是用户输入，但是
	//   1. 调用方 SupportsURL/ytdlpURLPatterns 已用正则把可接受 host 限制到
	//      白名单（YouTube/B 站等具名站点）；
	//   2. 这里用 "--" 终止 yt-dlp 的 flag 解析，URL 一律作为 positional 参数；
	//   3. exec.CommandContext 走 argv 而非 shell，URL 不会被 shell 转义。
	// 三层防御足够，gosec 的"tainted input"是过度保守。
	cmd := exec.CommandContext(cmdCtx, f.BinaryPath, //nolint:gosec // reason: URL 受 host 白名单限制 + "--" 终止 flag + argv 调用，无 shell 注入面
		"--dump-json",
		"--no-warnings",
		"--no-playlist",
		"--skip-download",
		"--",
		url,
	)
	// Without WaitDelay, exec.CommandContext only sends SIGKILL on context
	// cancellation and then waits indefinitely for the kernel to finish
	// reaping. yt-dlp can stall on a slow / dead upstream and pin the
	// goroutine even after the deadline has fired. Cap the post-cancel grace
	// at 5s so cmd.Run unblocks promptly.
	cmd.WaitDelay = 5 * time.Second

	maxBytes := f.MaxOutputSize
	if maxBytes <= 0 {
		maxBytes = defaultYtdlpMaxOutputSize
	}
	stdout := &cappedBuffer{limit: maxBytes}
	var stderr bytes.Buffer
	cmd.Stdout = stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		if errors.Is(cmdCtx.Err(), context.DeadlineExceeded) {
			return Content{}, &FetchError{URL: url, Reason: "yt-dlp timed out", Err: cmdCtx.Err()}
		}
		if errors.Is(err, exec.ErrNotFound) || errors.Is(err, fs.ErrNotExist) {
			return Content{}, &FetchError{URL: url, Reason: fmt.Sprintf("yt-dlp binary unavailable: %s", f.BinaryPath), Err: err}
		}
		var execErr *exec.Error
		if errors.As(err, &execErr) {
			return Content{}, &FetchError{URL: url, Reason: fmt.Sprintf("yt-dlp binary unavailable: %s", f.BinaryPath), Err: err}
		}
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			msg := strings.TrimSpace(stderr.String())
			if msg == "" {
				msg = exitErr.Error()
			}
			return Content{}, &FetchError{URL: url, Reason: fmt.Sprintf("yt-dlp exited non-zero: %s", limitTextByRunes(msg, 200)), Err: err}
		}
		return Content{}, &FetchError{URL: url, Reason: "run yt-dlp failed", Err: err}
	}

	if stdout.truncated {
		return Content{}, &FetchError{URL: url, Reason: fmt.Sprintf("yt-dlp output exceeded %d bytes", maxBytes)}
	}
	out := stdout.Bytes()

	var raw ytdlpRawOutput
	if err := jsonx.Unmarshal(out, &raw); err != nil {
		return Content{}, &FetchError{URL: url, Reason: "decode yt-dlp output failed", Err: err}
	}

	title := normalizeSpace(raw.Title)
	if title == "" {
		return Content{}, &FetchError{URL: url, Reason: "yt-dlp output missing title"}
	}

	body := limitTextByRunes(strings.TrimSpace(raw.Description), f.BodyMaxRunes)

	metadata := map[string]any{}
	if extractor := normalizeSpace(raw.Extractor); extractor != "" {
		metadata["extractor"] = extractor
	}
	if uploader := normalizeSpace(raw.Uploader); uploader != "" {
		metadata["uploader"] = uploader
	}
	if raw.Duration > 0 {
		metadata["duration_seconds"] = int(raw.Duration)
	}
	if raw.ViewCount > 0 {
		metadata["view_count"] = raw.ViewCount
	}
	if upload := normalizeSpace(raw.UploadDate); upload != "" {
		metadata["upload_date"] = upload
	}
	if channel := strings.TrimSpace(raw.ChannelURL); channel != "" {
		metadata["channel_url"] = channel
	}
	thumbnail := strings.TrimSpace(raw.Thumbnail)
	if thumbnail != "" {
		metadata["thumbnail"] = thumbnail
	}
	if webpage := strings.TrimSpace(raw.WebpageURL); webpage != "" {
		metadata["webpage_url"] = webpage
	}
	if langs := mergeSubtitleLanguages(raw.Subtitles, raw.AutoCaptions); len(langs) > 0 {
		metadata["subtitle_languages"] = langs
	}

	images := []string{}
	if thumbnail != "" {
		images = append(images, thumbnail)
	}

	return Content{
		URL:         url,
		Title:       title,
		Body:        body,
		ImageURLs:   images,
		Metadata:    metadata,
		FetcherType: "ytdlp",
	}, nil
}

func mergeSubtitleLanguages(maps ...map[string][]ytdlpSubtitleTrack) []string {
	seen := make(map[string]struct{})
	for _, m := range maps {
		for lang := range m {
			lang = strings.TrimSpace(lang)
			if lang == "" {
				continue
			}
			seen[lang] = struct{}{}
		}
	}
	if len(seen) == 0 {
		return nil
	}
	out := make([]string, 0, len(seen))
	for lang := range seen {
		out = append(out, lang)
	}
	sort.Strings(out)
	return out
}

type ytdlpRawOutput struct {
	Title        string                          `json:"title"`
	Description  string                          `json:"description"`
	Uploader     string                          `json:"uploader"`
	Duration     float64                         `json:"duration"`
	ViewCount    int64                           `json:"view_count"`
	UploadDate   string                          `json:"upload_date"`
	WebpageURL   string                          `json:"webpage_url"`
	ChannelURL   string                          `json:"channel_url"`
	Thumbnail    string                          `json:"thumbnail"`
	Extractor    string                          `json:"extractor"`
	Subtitles    map[string][]ytdlpSubtitleTrack `json:"subtitles"`
	AutoCaptions map[string][]ytdlpSubtitleTrack `json:"automatic_captions"`
}

type ytdlpSubtitleTrack struct {
	URL string `json:"url"`
	Ext string `json:"ext"`
}

// cappedBuffer collects up to limit bytes from cmd.Stdout. Writes beyond the
// cap are discarded and truncated is set so the caller can reject the result.
// It implements io.Writer so it can be assigned to exec.Cmd.Stdout directly.
type cappedBuffer struct {
	buf       bytes.Buffer
	limit     int64
	truncated bool
}

func (c *cappedBuffer) Write(p []byte) (int, error) {
	if c.truncated {
		return len(p), nil
	}
	remaining := c.limit - int64(c.buf.Len())
	if remaining <= 0 {
		c.truncated = true
		return len(p), nil
	}
	if int64(len(p)) > remaining {
		c.buf.Write(p[:remaining])
		c.truncated = true
		return len(p), nil
	}
	return c.buf.Write(p)
}

func (c *cappedBuffer) Bytes() []byte {
	return c.buf.Bytes()
}

// Compile-time interface assertion.
var _ io.Writer = (*cappedBuffer)(nil)
