package repository

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"webtag/internal/database"
)

const (
	readerContentHistoryCleanupBatchSize = 100
	readerContentHistoryKeepPerLink      = 20
)

// PGXReaderContentHistoryCleanupRepository executes one bounded retention
// transaction for the installation. The database function owns candidate
// ranking; this adapter owns the stricter runtime transaction limit.
type PGXReaderContentHistoryCleanupRepository struct {
	transactions database.TxBeginner
}

func NewPGXReaderContentHistoryCleanupRepository(transactions database.TxBeginner) *PGXReaderContentHistoryCleanupRepository {
	return &PGXReaderContentHistoryCleanupRepository{transactions: transactions}
}

// CleanupContentHistoryBatch removes at most 100 obsolete snapshots. Every
// invocation has its own transaction so a worker can make bounded progress
// without holding locks for an entire cleanup tick.
func (r *PGXReaderContentHistoryCleanupRepository) CleanupContentHistoryBatch(ctx context.Context, batchSize int) (int, error) {
	if r == nil || r.transactions == nil {
		return 0, errors.New("reader content history cleanup repository is not configured")
	}
	if batchSize <= 0 || batchSize > readerContentHistoryCleanupBatchSize {
		batchSize = readerContentHistoryCleanupBatchSize
	}

	deleted := 0
	err := database.WithTx(ctx, r.transactions, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx,
			`SELECT public.reader_cleanup_content_history($1,$2)`,
			batchSize,
			readerContentHistoryKeepPerLink,
		).Scan(&deleted); err != nil {
			return fmt.Errorf("cleanup reader content history batch: %w", err)
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return deleted, nil
}
