package dbintegration

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"webtag/internal/database"
	"webtag/internal/model"
	"webtag/internal/repository"
)

// submitLinkForTest exercises the transaction-bound repository primitive in
// tests that intentionally do not need River. Durable command tests use the
// real adapter instead so atomic product-state/queue behavior remains covered.
func submitLinkForTest(
	ctx context.Context,
	pool *pgxpool.Pool,
	repo *repository.PGXLinkRepository,
	params repository.CreateLinkParams,
) (*model.Link, *model.ParseAttempt, error) {
	var result repository.LinkSubmitResult
	err := database.WithTx(ctx, pool, func(tx pgx.Tx) error {
		var err error
		result, err = repo.SubmitTx(ctx, tx, params)
		if err != nil {
			return err
		}
		if result.Link == nil {
			return fmt.Errorf("submit test link: repository returned nil Link")
		}
		return nil
	})
	return result.Link, result.Attempt, err
}

func requeueLinkForTest(
	ctx context.Context,
	pool *pgxpool.Pool,
	repo *repository.PGXLinkRepository,
	linkID uuid.UUID,
	capture *repository.CreateLinkParams,
) (model.ParseAttempt, error) {
	var attempt model.ParseAttempt
	err := database.WithTx(ctx, pool, func(tx pgx.Tx) error {
		var err error
		attempt, err = repo.RequeueExistingTx(ctx, tx, linkID, capture)
		return err
	})
	return attempt, err
}

func requireParseAttempt(link *model.Link, attempt *model.ParseAttempt) (model.ParseAttempt, error) {
	if link == nil || attempt == nil {
		return model.ParseAttempt{}, errors.New("test link submission did not create a parse attempt")
	}
	return *attempt, nil
}
