package dbintegration

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"webtag/internal/model"
	"webtag/internal/repository"
)

// mustCreateDoneLink creates one installation-owned link and populates the
// fields exercised by list, search, tag, sidebar, and translation tests.
func mustCreateDoneLink(
	t *testing.T,
	repo *repository.PGXLinkRepository,
	ctx context.Context,
	rawURL, tag, domain string,
) uuid.UUID {
	t.Helper()
	link, err := repo.Create(ctx, repository.CreateLinkParams{
		URL:        rawURL,
		SourceKind: "url",
		SourceKey:  rawURL,
		Status:     model.LinkStatusDone,
		Domain:     &domain,
	})
	if err != nil {
		t.Fatalf("create link %q: %v", rawURL, err)
	}
	title := "t"
	contentType := "article"
	if err := repo.UpdateAnalysis(ctx, repository.UpdateLinkAnalysisParams{
		ID:          link.ID,
		Title:       &title,
		Tags:        []string{tag},
		Status:      model.LinkStatusDone,
		Domain:      &domain,
		ContentType: &contentType,
	}); err != nil {
		t.Fatalf("update analysis %q: %v", rawURL, err)
	}
	return link.ID
}
