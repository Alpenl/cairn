package repository

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"webtag/internal/database"
	"webtag/internal/model"
)

type PGXReaderVNextRepository struct {
	db                 database.Querier
	linkLifecycleQueue ReaderLinkLifecycleQueue
}

func NewPGXReaderVNextRepository(db database.Querier) *PGXReaderVNextRepository {
	return &PGXReaderVNextRepository{db: db}
}

func NewPGXReaderVNextRepositoryWithLinkLifecycle(
	db database.Querier,
	queue ReaderLinkLifecycleQueue,
) *PGXReaderVNextRepository {
	if queue == nil {
		panic("repository: nil Reader Link lifecycle queue")
	}
	return &PGXReaderVNextRepository{db: db, linkLifecycleQueue: queue}
}

// ReaderLinkLifecycleQueue is the transaction-bound River port required when
// Reader entry points trash or restore a Link. It stays narrow so the
// repository does not depend on the worker package.
type ReaderLinkLifecycleQueue interface {
	EnqueueTx(context.Context, pgx.Tx, model.ParseAttempt) error
	CancelAllActiveTx(context.Context, pgx.Tx, uuid.UUID) error
}

type readerTxBeginner interface {
	Begin(context.Context) (pgx.Tx, error)
}

func (r *PGXReaderVNextRepository) withTx(ctx context.Context, fn func(database.Querier) error) error {
	beginner, ok := r.db.(readerTxBeginner)
	if !ok {
		return fn(r.db)
	}
	tx, err := beginner.Begin(ctx)
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback(ctx)
		return err
	}
	return tx.Commit(ctx)
}

func rawJSON(value json.RawMessage) []byte {
	if len(value) == 0 {
		return []byte(`{}`)
	}
	return value
}

func parseReaderCursor(raw string) (time.Time, string, error) {
	if strings.TrimSpace(raw) == "" {
		return time.Time{}, "", nil
	}
	decoded, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return time.Time{}, "", fmt.Errorf("%w: %w", ErrInvalidReaderCursor, err)
	}
	parts := strings.SplitN(string(decoded), "|", 2)
	if len(parts) != 2 {
		return time.Time{}, "", ErrInvalidReaderCursor
	}
	at, err := time.Parse(time.RFC3339Nano, parts[0])
	if err != nil || parts[1] == "" {
		return time.Time{}, "", ErrInvalidReaderCursor
	}
	return at, parts[1], nil
}

func readerCursor(at time.Time, id string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(at.UTC().Format(time.RFC3339Nano) + "|" + id))
}

type readerScanner interface {
	Scan(...any) error
}

// readerInboxPreview turns the SQL-truncated card source into the bounded
// single-line preview the queue renders. The input is already clamped to
// readerInboxPreviewSourceLimit characters, so the rune conversion here is
// bounded regardless of how large the stored body is.
func readerInboxPreview(raw string) string {
	preview := []rune(strings.Join(strings.Fields(raw), " "))
	if len(preview) <= readerInboxPreviewLimit {
		return string(preview)
	}
	return string(preview[:readerInboxPreviewLimit]) + "…"
}
