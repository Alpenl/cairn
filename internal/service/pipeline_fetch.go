package service

import (
	"context"

	"webtag/internal/fetcher"
	"webtag/internal/repository"
)

// acquireContent returns the content the analyzer should consume. Multimodal
// ingest already carries its payload, while ordinary URL captures use the one
// production fetch chain.
func (p *ParsePipeline) acquireContent(ctx context.Context, link *repository.LinkParseInput) (fetcher.Content, error) {
	if isParseInputIngestSource(link) {
		return parseInputContent(*link), nil
	}
	return p.fetcher.Fetch(ctx, link.URL)
}
