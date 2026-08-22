package database

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"
)

// ClosePool starts pgxpool's ordinary close path and lets the caller stop
// waiting when ctx expires. pgxpool.Close still owns the real connection
// cleanup and keeps running in the background until checked-out connections
// return.
func ClosePool(ctx context.Context, pool *pgxpool.Pool) error {
	if pool == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}

	started := make(chan struct{})
	done := make(chan struct{})
	go func() {
		close(started)
		pool.Close()
		close(done)
	}()
	<-started

	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
