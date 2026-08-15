//go:build dbintegration

package database

import "github.com/jackc/pgx/v5/pgxpool"

// PoolConfigForDBIntegration exposes the production config assembly only to
// the isolated real-PostgreSQL test module. Normal builds do not export this
// test bridge.
func PoolConfigForDBIntegration(databaseURL string, opts Options) (*pgxpool.Config, error) {
	return newPoolConfig(databaseURL, opts)
}
