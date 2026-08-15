package repository

import (
	"context"
	"fmt"
	"strings"
	"webtag/internal/alloc"

	"github.com/google/uuid"
	"github.com/pgvector/pgvector-go"
)

const listSitesNeedingEmbeddingSQL = `SELECT s.id, s.revision, s.name, s.intro, s.site_key,
	COALESCE(ARRAY(SELECT st.tag FROM site_tags st WHERE st.site_id=s.id ORDER BY st.normalized_tag), '{}'),
	COALESCE(ARRAY(SELECT se.entry_name FROM site_entries se WHERE se.site_id=s.id ORDER BY se.first_collected_at, se.id), '{}'),
	COALESCE(ARRAY(SELECT se.purpose FROM site_entries se WHERE se.site_id=s.id ORDER BY se.first_collected_at, se.id), '{}')
FROM sites s
WHERE s.id > $1
  AND (s.embedding IS NULL OR s.embedding_model IS DISTINCT FROM $2)
ORDER BY s.id LIMIT $3`

// ListSitesNeedingEmbedding reads only the durable site profile tables. It
// deliberately does not join links: captured content belongs to the link
// retention boundary and must never become embedding input for a site.
func (r *PGXSiteRepository) ListSitesNeedingEmbedding(ctx context.Context, currentModel string, afterID uuid.UUID, limit int) ([]SiteEmbeddingCandidate, error) {
	currentModel = strings.TrimSpace(currentModel)
	if currentModel == "" {
		return nil, fmt.Errorf("list sites needing embedding: empty current model")
	}
	if limit < 1 {
		limit = 1
	}
	rows, err := r.db.Query(ctx, listSitesNeedingEmbeddingSQL, afterID, currentModel, limit)
	if err != nil {
		return nil, fmt.Errorf("list sites needing embedding: %w", err)
	}
	defer rows.Close()

	out := make([]SiteEmbeddingCandidate, 0, alloc.Hint(limit))
	for rows.Next() {
		var c SiteEmbeddingCandidate
		var siteKey string
		var entryNames, purposes []string
		if err := rows.Scan(&c.ID, &c.Revision, &c.Name, &c.Intro, &siteKey, &c.Tags, &entryNames, &purposes); err != nil {
			return nil, fmt.Errorf("scan site embedding candidate: %w", err)
		}
		c.DisplayHost = siteHost(siteKey)
		for i, name := range entryNames {
			purpose := ""
			if i < len(purposes) {
				purpose = purposes[i]
			}
			c.Entries = append(c.Entries, SiteEmbeddingEntryCandidate{Name: name, Purpose: purpose})
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate site embedding candidates: %w", err)
	}
	return out, nil
}

// UpdateSiteEmbedding changes derived retrieval data only. It does not update
// updated_at, revision, or any user-visible profile field.
func (r *PGXSiteRepository) UpdateSiteEmbedding(ctx context.Context, id uuid.UUID, expectedRevision int64, embedding []float32, embeddingModel string) (bool, error) {
	embeddingModel = strings.TrimSpace(embeddingModel)
	if id == uuid.Nil {
		return false, fmt.Errorf("update site embedding: nil id")
	}
	if expectedRevision < 1 {
		return false, fmt.Errorf("update site embedding: invalid revision")
	}
	if len(embedding) == 0 || embeddingModel == "" {
		return false, fmt.Errorf("update site embedding: empty embedding or model")
	}
	tag, err := r.db.Exec(ctx, `UPDATE sites SET embedding=$3, embedding_model=$4
WHERE id=$1 AND revision=$2 AND (embedding IS NULL OR embedding_model IS DISTINCT FROM $4)`, id, expectedRevision, pgvector.NewVector(embedding), embeddingModel)
	if err != nil {
		return false, fmt.Errorf("update site embedding %v: %w", id, err)
	}
	return tag.RowsAffected() == 1, nil
}

var _ SiteEmbeddingStore = (*PGXSiteRepository)(nil)
