package app

import (
	"log/slog"

	"webtag/internal/config"
	"webtag/internal/fetcher"
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

func (o *runtimeHTTPClientOwner) buildFetcherStack(cfg config.Config) fetcherStack {
	fetchClient, analyzerClient := o.newRuntimeHTTPClients(cfg)
	visionClient := o.New(fetcher.HTTPClientOptions{})
	manager := fetcher.NewDefaultManager(fetchClient, cfg.GitHubToken)
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
		Logger:               logger,
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

func (o *runtimeHTTPClientOwner) newRuntimeHTTPClients(cfg config.Config) (*fetcher.HTTPClient, *fetcher.HTTPClient) {
	fetchHTTPClient := o.New(fetcher.HTTPClientOptions{
		RetryAttempts: cfg.Fetcher.RetryAttempts,
		RetryDelay:    durationMS(cfg.Fetcher.RetryDelayMS),
	})
	analyzerHTTPClient := o.New(fetcher.HTTPClientOptions{})
	return fetchHTTPClient, analyzerHTTPClient
}
