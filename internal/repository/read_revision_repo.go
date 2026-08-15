package repository

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"webtag/internal/database"
	"webtag/internal/representation"
)

// PGXReadRevisionRepository reads the installation-wide representation base.
//
// The fresh migration creates singleton library, global, and feed revision
// rows. Statement triggers advance those components. Go only reads this
// projection because manually wiring every
// write path would inevitably miss one and produce stale 304 responses.
type PGXReadRevisionRepository struct {
	db database.Querier
}

func NewPGXReadRevisionRepository(db database.Querier) *PGXReadRevisionRepository {
	return &PGXReadRevisionRepository{db: db}
}

const currentRepresentationBaseSQL = `
	SELECT state.representation_namespace,
		COALESCE(library.revision, 0),
		COALESCE((SELECT revision FROM global_read_revision WHERE singleton), 0),
		COALESCE(feed.revision, 0)
	FROM installation_state state
	LEFT JOIN library_read_revision library ON library.singleton
	LEFT JOIN feed_read_revision feed ON feed.singleton
	WHERE state.singleton`

// Current executes one authoritative query for the selected component set.
// Missing component rows coalesce to zero, while a missing installation_state
// row fails closed because it owns the representation namespace.
func (r *PGXReadRevisionRepository) Current(ctx context.Context, components representation.ComponentSet) (representation.VersionBase, error) {
	var (
		base            representation.VersionBase
		libraryRevision int64
		globalRevision  int64
		feedRevision    int64
	)
	err := r.db.QueryRow(ctx, currentRepresentationBaseSQL).Scan(
		&base.RepresentationNamespace,
		&libraryRevision,
		&globalRevision,
		&feedRevision,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return representation.VersionBase{}, errors.New("read representation version base: installation state is missing")
		}
		return representation.VersionBase{}, fmt.Errorf("read representation version base: %w", err)
	}
	revisions := map[representation.ComponentName]int64{
		representation.LibraryComponent: libraryRevision,
		representation.GlobalComponent:  globalRevision,
		representation.FeedComponent:    feedRevision,
	}
	for _, name := range components.Names() {
		base.Components = append(base.Components, representation.Component{
			Name:     name,
			Revision: revisions[name],
		})
	}
	if !base.ValidFor(components) {
		return representation.VersionBase{}, errors.New("read representation version base: invalid namespace or component vector")
	}
	return base, nil
}
