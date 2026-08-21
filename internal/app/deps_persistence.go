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
	pool            *pgxpool.Pool
	acquisitionGate persistenceAcquisitionGate
	poolShutdown    persistencePoolShutdown
	links           *repository.PGXLinkRepository
	tags            *repository.PGXTagRepository
	tree            *repository.PGXTreeRepository
	translations    *repository.PGXTranslationRepository
	idempotency     *repository.PGXIdempotencyRepository
	feeds           *repository.PGXFeedRepository
	sites           *repository.PGXSiteRepository
	reader          *repository.PGXReaderVNextRepository

	// installationIdentity resolves the namespace used to partition client data.
	installationIdentity *service.InstallationIdentityService
	logger               *slog.Logger
}

// persistenceAcquisitionGate is the exact gate capability retained by the
// production persistence adapter. Keeping the pool argument on Drain makes the
// adapter's pool-identity handoff observable without duplicating gate logic in
// tests.
type persistenceAcquisitionGate interface {
	AdmitOwner(context.Context) (context.Context, *database.AcquisitionOwner, error)
	CloseAdmission()
	Drain(context.Context, *pgxpool.Pool) error
}

// persistencePoolShutdown is the exact destructor capability installed into
// database.Options and later invoked by the production persistence adapter.
type persistencePoolShutdown interface {
	BeginShutdown()
	Close(context.Context, *pgxpool.Pool) error
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
	acquisitionGate := database.NewAcquisitionGate()
	poolShutdown := database.NewPoolShutdown()

	pool, err := openDatabase(ctx, cfg.DatabaseURL, database.Options{
		MaxConns:          cfg.DB.MaxConns,
		MinConns:          cfg.DB.MinConns,
		MaxConnLifetime:   durationMS(cfg.DB.MaxConnLifetimeMS),
		MaxConnIdleTime:   durationMS(cfg.DB.MaxConnIdleTimeMS),
		HealthCheckPeriod: durationMS(cfg.DB.HealthCheckPeriodMS),
		AcquisitionGate:   acquisitionGate,
		PoolShutdown:      poolShutdown,
	})
	if err != nil {
		acquisitionGate.CloseAdmission()
		return nil, err
	}
	layer := &persistenceLayer{
		pool:            pool,
		acquisitionGate: acquisitionGate,
		poolShutdown:    poolShutdown,
	}

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

var _ runtimePersistence = (*persistenceLayer)(nil)

func (l *persistenceLayer) AdmitOwner(ctx context.Context) (context.Context, func(), error) {
	if l == nil || l.acquisitionGate == nil {
		return ctx, nil, database.ErrPersistenceAdmissionClosed
	}
	ownerCtx, owner, err := l.acquisitionGate.AdmitOwner(ctx)
	if err != nil {
		return ctx, nil, err
	}
	return ownerCtx, owner.Revoke, nil
}

func (l *persistenceLayer) CloseAdmission() {
	if l == nil {
		return
	}
	if l.acquisitionGate != nil {
		l.acquisitionGate.CloseAdmission()
	}
	if l.poolShutdown != nil {
		l.poolShutdown.BeginShutdown()
	}
}

func (l *persistenceLayer) Drain(ctx context.Context) error {
	if l == nil || l.acquisitionGate == nil {
		return nil
	}
	return l.acquisitionGate.Drain(ctx, l.pool)
}

func (l *persistenceLayer) Close(ctx context.Context) error {
	if l == nil || l.pool == nil {
		return ctx.Err()
	}
	if l.poolShutdown == nil {
		// Preserve PoolShutdown's actionable construction invariant now that the
		// field is an interface. Calling a nil interface would otherwise panic.
		return (*database.PoolShutdown)(nil).Close(ctx, l.pool)
	}
	return l.poolShutdown.Close(ctx, l.pool)
}

// cleanupBuildFailure is the persistence acquisition's bound cleanup action.
// The acquired layer is the only value retained by the ownership adapter, so
// the gate, drain, pool, and destructor target cannot be paired independently.
func (l *persistenceLayer) cleanupBuildFailure(ctx context.Context) error {
	if l == nil {
		return nil
	}
	l.CloseAdmission()
	if err := l.Drain(ctx); err != nil {
		return err
	}
	return l.Close(ctx)
}
