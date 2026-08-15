package eval

import (
	"context"
	"fmt"
	"strings"
	"time"

	"webtag/internal/fetcher"
)

// FetcherName is the operator-facing identifier (matches CLI flag
// values like --fetcher basic,light,wechat). Keeping it a typed
// string forces the dispatcher's switch statement to enumerate every
// supported value explicitly — adding a new fetcher to production
// then surfaces here at compile time.
type FetcherName string

// FetcherX 常量穷举 eval CLI 接受的 --fetcher 取值，与生产端 fetcher.Manager 的分流名称一一对应。
const (
	FetcherBasic  FetcherName = "basic"
	FetcherLight  FetcherName = "light"
	FetcherWeChat FetcherName = "wechat"
	FetcherGitHub FetcherName = "github"
	FetcherArxiv  FetcherName = "arxiv"
	FetcherYtdlp  FetcherName = "ytdlp"
	FetcherRouter FetcherName = "router" // production routing — let manager.Fetch decide
	FetcherGrok   FetcherName = "grok"   // production URL-direct path — analyzer fetches the URL
)

// FetcherBuildOptions carries the runtime knobs the production
// fetcher constructors require. The eval CLI fills these from env
// (GITHUB_TOKEN / YTDLP_BINARY_PATH / YTDLP_TIMEOUT_MS) so a
// developer can run `eval run` with the same configuration they'd
// use for the live service.
type FetcherBuildOptions struct {
	GitHubToken     string
	YtdlpBinaryPath string
	YtdlpTimeout    time.Duration
	HTTPClient      *fetcher.HTTPClient
}

// BuildFetcher returns a Fetcher matching the requested name. The
// "router" name returns a Fetcher-like wrapper around the production
// Manager so the eval harness can compare the routed (production)
// behaviour against an individual fetcher in the same run.
//
// CanHandle is intentionally NOT consulted — the eval harness's
// whole point is to *force* a fetcher onto a URL it might not
// natively handle, so we can see what falls out. Production callers
// should never reach this code path.
func BuildFetcher(name FetcherName, opts FetcherBuildOptions) (fetcher.Fetcher, error) {
	client := opts.HTTPClient
	if client == nil {
		client = fetcher.NewHTTPClient(nil)
	}
	switch name {
	case FetcherBasic:
		return fetcher.NewBasicFetcher(client), nil
	case FetcherLight:
		// LightFetcher does Range-limited GET + readability truncation;
		// the eval calls it directly via the Fetcher interface, so we
		// wrap CanHandle to always-true (see forcedFetcher below).
		return forceFetcher(fetcher.NewLightFetcher(client)), nil
	case FetcherWeChat:
		return forceFetcher(fetcher.NewWeChatFetcher(client)), nil
	case FetcherGitHub:
		return forceFetcher(fetcher.NewGitHubFetcher(client, opts.GitHubToken)), nil
	case FetcherArxiv:
		return forceFetcher(fetcher.NewArxivFetcher(client)), nil
	case FetcherYtdlp:
		binPath := opts.YtdlpBinaryPath
		if strings.TrimSpace(binPath) == "" {
			binPath = "yt-dlp"
		}
		timeout := opts.YtdlpTimeout
		if timeout <= 0 {
			timeout = 30 * time.Second
		}
		return forceFetcher(fetcher.NewYtdlpFetcher(binPath, timeout)), nil
	case FetcherRouter:
		return routedFetcher{manager: fetcher.NewDefaultManager(client, fetcher.ManagerOptions{
			YtdlpBinaryPath: firstNonEmpty(opts.YtdlpBinaryPath, "yt-dlp"),
			YtdlpTimeout:    nonZeroOr(opts.YtdlpTimeout, 30*time.Second),
			GitHubToken:     opts.GitHubToken,
		})}, nil
	case FetcherGrok:
		return nil, fmt.Errorf("eval: grok is an analyzer URL-direct mode and is not built as a local fetcher")
	default:
		return nil, fmt.Errorf("eval: unknown fetcher %q (supported: basic, light, wechat, github, arxiv, ytdlp, router, grok)", name)
	}
}

// routedFetcher adapts *fetcher.Manager.Fetch into the Fetcher
// interface so the eval runner does not need a separate dispatch
// branch for the "production routing" case.
type routedFetcher struct {
	manager *fetcher.Manager
}

func (r routedFetcher) CanHandle(string) bool { return true }
func (r routedFetcher) Fetch(ctx context.Context, url string) (fetcher.Content, error) {
	return r.manager.Fetch(ctx, url)
}

// forceFetcher wraps an existing Fetcher to always answer
// CanHandle=true. The eval harness forces every fetcher to attempt
// every URL — that's the point of cross-fetcher comparison. Without
// this wrapper, the github fetcher would refuse a zhihu URL and
// produce an empty Content the runner could not tell apart from a
// real fetch failure.
func forceFetcher(f fetcher.Fetcher) fetcher.Fetcher {
	return forcedFetcher{inner: f}
}

type forcedFetcher struct {
	inner fetcher.Fetcher
}

func (f forcedFetcher) CanHandle(string) bool { return true }
func (f forcedFetcher) Fetch(ctx context.Context, url string) (fetcher.Content, error) {
	return f.inner.Fetch(ctx, url)
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func nonZeroOr(d, fallback time.Duration) time.Duration {
	if d > 0 {
		return d
	}
	return fallback
}
