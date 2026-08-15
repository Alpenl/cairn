package concept

import (
	"context"
	"fmt"

	"github.com/google/uuid"
)

// createLocalConcept atomically creates a concept and its identity alias.
// The store resolves a concurrent alias conflict to the existing canonical
// concept, so the returned ID is always the authoritative alias owner.
// embedding / embeddingModel are written only when the vector gate admitted
// a concept with a fresh vector; the local-only fallback path passes
// (nil, "") so a genuinely new row stays vector-less until backfill.
func (r *Resolver) createLocalConcept(ctx context.Context, surfaceTag string, embedding []float32, embeddingModel string) (uuid.UUID, error) {
	id, err := r.store.CreateConcept(ctx, CreateConceptParams{
		PrimaryName:    surfaceTag,
		Embedding:      embedding,
		EmbeddingModel: embeddingModel,
	})
	if err != nil {
		return uuid.Nil, fmt.Errorf("create local concept %q: %w", surfaceTag, err)
	}
	r.cache.Put(surfaceTag, id)
	return id, nil
}
