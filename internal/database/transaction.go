package database

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// TxBeginner is the smallest transaction entrypoint implemented by pgx pools,
// pgx transactions, and repository test doubles.
type TxBeginner interface {
	Begin(context.Context) (pgx.Tx, error)
}

// TxOptionsBeginner opens a transaction with explicit PostgreSQL isolation and
// access modes.
type TxOptionsBeginner interface {
	BeginTx(context.Context, pgx.TxOptions) (pgx.Tx, error)
}

// WithTx runs fn and commits exactly once when it succeeds. Every error path
// rolls back through the deferred cleanup.
func WithTx(ctx context.Context, beginner TxBeginner, fn func(pgx.Tx) error) error {
	tx, err := beginner.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit transaction: %w", err)
	}
	return nil
}
