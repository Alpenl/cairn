package dbintegration

import (
	"testing"

	"github.com/google/uuid"

	"webtag/internal/repository"
)

func TestSiteEmbeddingInvalidationAndRevisionCAS(t *testing.T) {
	pool := StartPostgres(t)
	repo := repository.NewPGXSiteRepository(pool)
	ctx := t.Context()
	siteID := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO sites
		(id,site_key,name,name_source,intro_source)
		VALUES ($1,$2,'Before','user','user')`, siteID, "site-embedding:"+siteID.String()); err != nil {
		t.Fatalf("seed site: %v", err)
	}

	vector := make([]float32, 1536)
	vector[0] = 1
	updated, err := repo.UpdateSiteEmbedding(ctx, siteID, 1, vector, "site-model")
	if err != nil || !updated {
		t.Fatalf("initial UpdateSiteEmbedding() = %v, %v", updated, err)
	}

	name := "After"
	if ok, err := repo.UpdateSiteProfile(ctx, repository.UpdateSiteProfileParams{
		ID: siteID, Revision: 1, Name: &name,
	}); err != nil || !ok {
		t.Fatalf("UpdateSiteProfile() = %v, %v", ok, err)
	}
	var revision int64
	var embeddingMissing, modelMissing bool
	if err := pool.QueryRow(ctx, `SELECT revision,embedding IS NULL,embedding_model IS NULL FROM sites WHERE id=$1`, siteID).
		Scan(&revision, &embeddingMissing, &modelMissing); err != nil {
		t.Fatalf("read invalidated site: %v", err)
	}
	if revision != 2 || !embeddingMissing || !modelMissing {
		t.Fatalf("profile invalidation = revision %d, embedding/model missing %v/%v", revision, embeddingMissing, modelMissing)
	}

	updated, err = repo.UpdateSiteEmbedding(ctx, siteID, 1, vector, "site-model")
	if err != nil || updated {
		t.Fatalf("stale UpdateSiteEmbedding() = %v, %v, want CAS miss", updated, err)
	}
	candidates, err := repo.ListSitesNeedingEmbedding(ctx, "site-model", uuid.Nil, 10)
	if err != nil {
		t.Fatalf("ListSitesNeedingEmbedding() error = %v", err)
	}
	if len(candidates) != 1 || candidates[0].ID != siteID || candidates[0].Revision != 2 || candidates[0].Name != name {
		t.Fatalf("embedding candidates = %#v", candidates)
	}

	updated, err = repo.UpdateSiteEmbedding(ctx, siteID, 2, vector, "site-model")
	if err != nil || !updated {
		t.Fatalf("current UpdateSiteEmbedding() = %v, %v", updated, err)
	}
	if ok, err := repo.UpdateSiteProfileAndTags(ctx, repository.UpdateSiteProfileParams{
		ID: siteID, Revision: 2,
		TagAdds: []repository.SiteTagMutation{{Tag: "Search", NormalizedTag: "search"}},
	}); err != nil || !ok {
		t.Fatalf("UpdateSiteProfileAndTags() = %v, %v", ok, err)
	}
	if err := pool.QueryRow(ctx, `SELECT revision,embedding IS NULL,embedding_model IS NULL FROM sites WHERE id=$1`, siteID).
		Scan(&revision, &embeddingMissing, &modelMissing); err != nil {
		t.Fatalf("read tag-invalidated site: %v", err)
	}
	if revision != 3 || !embeddingMissing || !modelMissing {
		t.Fatalf("tag invalidation = revision %d, embedding/model missing %v/%v", revision, embeddingMissing, modelMissing)
	}
}
