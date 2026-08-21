package repository

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"webtag/internal/database"
	"webtag/internal/representation"
)

// PGXInstallationIdentityRepository reads the installation namespace. The
// namespace is the only representation value consumed by the current server.
type PGXInstallationIdentityRepository struct {
	db database.Querier
}

func NewPGXInstallationIdentityRepository(db database.Querier) *PGXInstallationIdentityRepository {
	return &PGXInstallationIdentityRepository{db: db}
}

const currentInstallationIdentitySQL = `
	SELECT representation_namespace
	FROM installation_state
	WHERE singleton`

func (r *PGXInstallationIdentityRepository) Current(ctx context.Context) (representation.ClientIdentity, error) {
	var namespace uuid.UUID
	if err := r.db.QueryRow(ctx, currentInstallationIdentitySQL).Scan(&namespace); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return representation.ClientIdentity{}, errors.New("read installation identity: installation state is missing")
		}
		return representation.ClientIdentity{}, fmt.Errorf("read installation identity: %w", err)
	}
	identity, err := representation.NewClientIdentity(namespace)
	if err != nil {
		return representation.ClientIdentity{}, fmt.Errorf("read installation identity: %w", err)
	}
	return identity, nil
}
