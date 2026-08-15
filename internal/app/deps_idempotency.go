package app

import (
	"context"
	"time"

	"webtag/internal/middleware"
	"webtag/internal/repository"
)

// idempotencyStoreAdapter bridges the repository's PG idempotency store to the
// middleware.IdempotencyStore interface. The middleware layer deliberately
// declares its own StoredResponse type (instead of importing repository) to
// stay free of a repository dependency; this adapter does the one-field-per-
// field translation in the wiring layer where both packages are in scope.
type idempotencyStoreAdapter struct {
	repo *repository.PGXIdempotencyRepository
}

// Acquire delegates to the repo and translates the returned record type.
func (a idempotencyStoreAdapter) Acquire(ctx context.Context, key, ownerToken string, now, expiresAt time.Time) (bool, *middleware.StoredResponse, error) {
	acquired, rec, err := a.repo.Acquire(ctx, key, ownerToken, now, expiresAt)
	if err != nil {
		return false, nil, err
	}
	if rec == nil {
		return acquired, nil, nil
	}
	return acquired, &middleware.StoredResponse{
		Claim: middleware.IdempotencyClaim{
			OwnerToken: rec.OwnerToken,
			Generation: rec.Generation,
		},
		Status:      rec.Status,
		Body:        rec.Body,
		ContentType: rec.ContentType,
		InFlight:    rec.InFlight,
		ExpiresAt:   rec.ExpiresAt,
	}, nil
}

func (a idempotencyStoreAdapter) Get(ctx context.Context, key string) (*middleware.StoredResponse, error) {
	rec, err := a.repo.Get(ctx, key)
	if err != nil || rec == nil {
		return nil, err
	}
	return &middleware.StoredResponse{
		Claim: middleware.IdempotencyClaim{
			OwnerToken: rec.OwnerToken,
			Generation: rec.Generation,
		},
		Status:      rec.Status,
		Body:        rec.Body,
		ContentType: rec.ContentType,
		InFlight:    rec.InFlight,
		ExpiresAt:   rec.ExpiresAt,
	}, nil
}

func (a idempotencyStoreAdapter) Store(ctx context.Context, key string, claim middleware.IdempotencyClaim, status int, body []byte, contentType string, expiresAt time.Time) error {
	return a.repo.Store(ctx, key, claim.OwnerToken, claim.Generation, status, body, contentType, expiresAt)
}

func (a idempotencyStoreAdapter) Delete(ctx context.Context, key string, claim middleware.IdempotencyClaim) error {
	return a.repo.Delete(ctx, key, claim.OwnerToken, claim.Generation)
}

func (a idempotencyStoreAdapter) PurgeExpired(ctx context.Context, now time.Time) (int64, error) {
	return a.repo.PurgeExpired(ctx, now)
}

// 编译期断言：adapter 满足中间件的 store 契约。
var _ middleware.IdempotencyStore = idempotencyStoreAdapter{}
