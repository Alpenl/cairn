package app

import (
	"context"
	"log/slog"

	"github.com/jackc/pgx/v5/pgxpool"

	"webtag/internal/config"
	"webtag/internal/database"
	"webtag/internal/observability"
	"webtag/internal/repository"
	"webtag/internal/service"
)

// persistenceLayer bundles the foundational dependencies that every
// later phase needs: the pgxpool, the four PG repositories, the
// installation identity and logger. Carving these
// off into one struct lets sub-constructors take a single argument
// instead of five.
type persistenceLayer struct {
	pool         *pgxpool.Pool
	links        *repository.PGXLinkRepository
	tags         *repository.PGXTagRepository
	tree         *repository.PGXTreeRepository
	translations *repository.PGXTranslationRepository
	idempotency  *repository.PGXIdempotencyRepository
	feeds        *repository.PGXFeedRepository
	sites        *repository.PGXSiteRepository
	reader       *repository.PGXReaderVNextRepository

	// installationIdentity resolves the namespace used to partition client data.
	installationIdentity *service.InstallationIdentityService
	logger               *slog.Logger
}

type persistenceDatabaseOpener func(
	context.Context,
	string,
	database.Options,
) (*pgxpool.Pool, error)

func openPersistenceLayer(ctx context.Context, cfg config.Config) (*persistenceLayer, error) {
	return openPersistenceLayerWithDatabase(ctx, cfg, database.Open)
}

func openPersistenceLayerWithDatabase(
	ctx context.Context,
	cfg config.Config,
	openDatabase persistenceDatabaseOpener,
) (*persistenceLayer, error) {
	pool, err := openDatabase(ctx, cfg.DatabaseURL, database.Options{
		MaxConns:          cfg.DB.MaxConns,
		MinConns:          cfg.DB.MinConns,
		MaxConnLifetime:   durationMS(cfg.DB.MaxConnLifetimeMS),
		MaxConnIdleTime:   durationMS(cfg.DB.MaxConnIdleTimeMS),
		HealthCheckPeriod: durationMS(cfg.DB.HealthCheckPeriodMS),
	})
	if err != nil {
		return nil, err
	}
	layer := &persistenceLayer{pool: pool}

	layer.links = repository.NewPGXLinkRepository(pool)
	layer.tags = repository.NewPGXTagRepository(pool)
	layer.tree = repository.NewPGXTreeRepository(pool)
	layer.translations = repository.NewPGXTranslationRepository(pool)
	layer.idempotency = repository.NewPGXIdempotencyRepository(pool)
	layer.feeds = repository.NewPGXFeedRepository(pool, pool)
	layer.sites = repository.NewPGXSiteRepository(pool)
	layer.reader = repository.NewPGXReaderVNextRepository(pool)
	layer.installationIdentity = service.NewInstallationIdentityService(repository.NewPGXInstallationIdentityRepository(pool))
	layer.logger = observability.NewLoggerWithOptions(observability.LoggerOptions{
		Level:  observability.ParseLevel(cfg.LogLevel),
		Format: observability.ParseLogFormat(cfg.LogFormat),
	})
	return layer, nil
}

func (l *persistenceLayer) Close(_ context.Context) error {
	if l == nil || l.pool == nil {
		return nil
	}
	l.pool.Close()
	return nil
}
