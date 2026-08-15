package app

import (
	"log/slog"
	"net/http"

	"webtag/internal/config"
	"webtag/internal/embedding"
	"webtag/internal/fetcher"
	"webtag/internal/observability"
	"webtag/internal/service/analyzer"
	"webtag/internal/service/translator"
)

// fetcherStack groups the outbound HTTP clients and the *Manager so the
// pipeline / analyzer constructors can take exactly the surface they
// need without re-deriving HTTP options.
type fetcherStack struct {
	fetchClient    *fetcher.HTTPClient
	analyzerClient *fetcher.HTTPClient
	visionClient   *fetcher.HTTPClient
	manager        *fetcher.Manager
}

func (o *runtimeHTTPClientOwner) buildFetcherStack(cfg config.Config, metrics *observability.Metrics) fetcherStack {
	fetchClient, analyzerClient := o.newRuntimeHTTPClients(cfg, metrics)
	visionClient := o.New(fetcher.HTTPClientOptions{
		WrapTransport: func(inner http.RoundTripper) http.RoundTripper {
			return fetcher.NewFixedHostMetricsTransport(inner, metrics, "vision", "untrusted_image")
		},
	})
	manager := fetcher.NewDefaultManager(fetchClient, fetcher.ManagerOptions{
		YtdlpBinaryPath: cfg.YtdlpBinaryPath,
		YtdlpTimeout:    durationMS(cfg.YtdlpTimeoutMS),
		GitHubToken:     cfg.GitHubToken,
		Metrics:         metrics,
	})
	return fetcherStack{
		fetchClient:    fetchClient,
		analyzerClient: analyzerClient,
		visionClient:   visionClient,
		manager:        manager,
	}
}

func buildAnalyzer(cfg config.Config, providerClient, visionClient *fetcher.HTTPClient, logger *slog.Logger) *analyzer.OpenAIAnalyzer {
	return analyzer.NewOpenAIAnalyzer(analyzer.OpenAIAnalyzerOptions{
		BaseURL:              cfg.Analyzer.BaseURL,
		APIKey:               cfg.Analyzer.APIKey,
		Model:                cfg.Analyzer.Model,
		HTTPClient:           providerClient.Raw(),
		VisionHTTPClient:     visionClient,
		RequestTimeout:       durationMS(cfg.Analyzer.RequestTimeoutMS),
		EmptyResponseRetries: cfg.Analyzer.RetryAttempts,
		RetryDelay:           durationMS(cfg.Analyzer.RetryDelayMS),

		DisableStructuredOutput: cfg.Analyzer.DisableStructuredOutput,
		Logger:                  logger,
	})
}

func buildTranslator(cfg config.Config, client *fetcher.HTTPClient) *translator.OpenAITranslator {
	return translator.NewOpenAITranslator(translator.Options{
		BaseURL:        cfg.Analyzer.BaseURL,
		APIKey:         cfg.Analyzer.APIKey,
		Model:          cfg.Analyzer.Model,
		HTTPClient:     client.Raw(),
		RequestTimeout: durationMS(cfg.Analyzer.RequestTimeoutMS),
	})
}

func (o *runtimeHTTPClientOwner) newRuntimeHTTPClients(cfg config.Config, metrics *observability.Metrics) (*fetcher.HTTPClient, *fetcher.HTTPClient) {
	// host_class 标签是抓取目标的真实 eTLD+1，也就是这台安装读过哪些站点。
	// /metrics 在 METRICS_AUTH_TOKEN 留空时是刻意开放的（见 .env.example，
	// 为了兼容单机部署），那种配置下不能把阅读域名史一并暴露出去——vision
	// 客户端早就用固定标签规避了同一个问题。配了 token 才记录真实域名。
	fetchHostClassKnown := cfg.MetricsAuthToken != ""
	fetchHTTPClient := o.New(fetcher.HTTPClientOptions{
		RetryAttempts: cfg.Fetcher.RetryAttempts,
		RetryDelay:    durationMS(cfg.Fetcher.RetryDelayMS),
		WrapTransport: func(inner http.RoundTripper) http.RoundTripper {
			if fetchHostClassKnown {
				return fetcher.NewMetricsTransport(inner, metrics, "fetch")
			}
			return fetcher.NewFixedHostMetricsTransport(inner, metrics, "fetch", "unreported")
		},
	})
	analyzerHTTPClient := o.New(fetcher.HTTPClientOptions{
		AllowUnsafeTargets: cfg.Analyzer.AllowUnsafeTargets,
		WrapTransport: func(inner http.RoundTripper) http.RoundTripper {
			return fetcher.NewMetricsTransport(inner, metrics, "openai")
		},
	})
	return fetchHTTPClient, analyzerHTTPClient
}

// buildEmbeddingClient constructs the client used for parse-time link vectors
// and semantic link queries. The transport mirrors the analyzer: SSRF-hardened
// (gated by AI_ALLOW_UNSAFE_TARGETS) and metrics-instrumented. With an empty
// EMBEDDING_MODEL, Enabled reports false and both call sites no-op or fall back.
func (o *runtimeHTTPClientOwner) buildEmbeddingClient(cfg config.Config, metrics *observability.Metrics) *embedding.Client {
	httpClient := o.New(fetcher.HTTPClientOptions{
		AllowUnsafeTargets: cfg.Embedding.AllowUnsafeTargets,
		WrapTransport: func(inner http.RoundTripper) http.RoundTripper {
			return fetcher.NewMetricsTransport(inner, metrics, "embedding")
		},
	}).Raw()
	return embedding.NewClient(embedding.Options{
		Model:          cfg.Embedding.Model,
		BaseURL:        cfg.Embedding.BaseURL,
		APIKey:         cfg.Embedding.APIKey,
		Dimensions:     cfg.Embedding.Dimensions,
		HTTPClient:     httpClient,
		RequestTimeout: durationMS(cfg.Embedding.RequestTimeoutMS),
		RetryAttempts:  cfg.Embedding.RetryAttempts,
		RetryDelay:     durationMS(cfg.Embedding.RetryDelayMS),
	})
}
