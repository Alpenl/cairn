package app

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/exaring/otelpgx"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"webtag/internal/config"
	"webtag/internal/database"
	"webtag/internal/migrate"
	"webtag/internal/observability"
	"webtag/internal/repository"
	"webtag/internal/service"
)

// persistenceLayer bundles the foundational dependencies that every
// later phase needs: the pgxpool, the four PG repositories, the
// shared tag cache, plus the metrics + logger handles. Carving these
// off into one struct lets sub-constructors take a single argument
// instead of five.
type persistenceLayer struct {
	pool                *pgxpool.Pool
	acquisitionGate     persistenceAcquisitionGate
	poolShutdown        persistencePoolShutdown
	links               *repository.PGXLinkRepository
	jobs                *repository.PGXJobRepository
	tags                *repository.PGXTagRepository
	tree                *repository.PGXTreeRepository
	translations        *repository.PGXTranslationRepository
	concepts            *repository.PGXConceptRepository
	conceptProposals    *repository.PGXConceptProposalRepository
	idempotency         *repository.PGXIdempotencyRepository
	feeds               *repository.PGXFeedRepository
	sites               *repository.PGXSiteRepository
	classificationRules *repository.PGXClassificationRuleRepository
	libraryReviews      *repository.PGXLibraryReviewRepository
	reader              *repository.PGXReaderVNextRepository

	tagCache *service.TagCache
	// domainCache 与 tagCache 并列：两份聚合由同一批写事件改变，装配层要能
	// 把同一个实例既接给读服务、又接进写路径的失效器。
	domainCache *service.DomainSummaryCache
	// readRevisions 提供条件请求（ETag）所需的安装级组件版本号。
	readRevisions *service.ReadRevisionService
	metrics       *observability.Metrics
	logger        *slog.Logger
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
	metrics := observability.NewMetrics()
	acquisitionGate := database.NewAcquisitionGate()
	poolShutdown := database.NewPoolShutdown()

	pool, err := openDatabase(ctx, cfg.DatabaseURL, database.Options{
		MaxConns:          cfg.DB.MaxConns,
		MinConns:          cfg.DB.MinConns,
		MaxConnLifetime:   durationMS(cfg.DB.MaxConnLifetimeMS),
		MaxConnIdleTime:   durationMS(cfg.DB.MaxConnIdleTimeMS),
		HealthCheckPeriod: durationMS(cfg.DB.HealthCheckPeriodMS),
		Tracer:            database.NewQueryTracer(metrics),
		AdditionalTracers: []pgx.QueryTracer{otelpgx.NewTracer()},
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
		metrics:         metrics,
	}
	if err := validateTranslationSourceRolloutPreflight(ctx, cfg.TranslationSourceRollout, pool); err != nil {
		return layer, err
	}

	metrics.RegisterPgxpoolGauges(func() observability.PgxpoolStat {
		s := pool.Stat()
		return observability.PgxpoolStat{
			AcquireCount: s.AcquireCount(),
			IdleConns:    s.IdleConns(),
			TotalConns:   s.TotalConns(),
		}
	})

	layer.links = repository.NewPGXLinkRepository(pool)
	layer.jobs = repository.NewPGXJobRepository(pool)
	layer.tags = repository.NewPGXTagRepository(pool)
	layer.tree = repository.NewPGXTreeRepository(pool)
	layer.translations = repository.NewPGXTranslationRepository(pool)
	layer.concepts = repository.NewPGXConceptRepository(pool)
	layer.conceptProposals = repository.NewPGXConceptProposalRepository(pool)
	layer.idempotency = repository.NewPGXIdempotencyRepository(pool)
	layer.feeds = repository.NewPGXFeedRepository(pool, pool)
	layer.sites = repository.NewPGXSiteRepository(pool)
	layer.classificationRules = repository.NewPGXClassificationRuleRepository(pool)
	layer.libraryReviews = repository.NewPGXLibraryReviewRepository(pool)
	layer.reader = repository.NewPGXReaderVNextRepository(pool)
	layer.tagCache = service.NewTagCache(durationMS(cfg.TagCacheTTLMS), nil)
	layer.domainCache = service.NewDomainSummaryCache(durationMS(cfg.TreeCacheTTLMS), nil)
	layer.readRevisions = service.NewReadRevisionService(repository.NewPGXReadRevisionRepository(pool))
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

// validateTranslationSourceRolloutPreflight prevents the compatibility mode
// from reopening unverified writes after the RF5A contract removed the legacy
// uniqueness constraint. Strict mode needs no database marker lookup and stays
// usable for fresh installs after their migration command has completed.
func validateTranslationSourceRolloutPreflight(
	ctx context.Context,
	stage config.TranslationSourceRolloutStage,
	db database.Querier,
) error {
	if stage != config.TranslationSourceRolloutCompat {
		return nil
	}
	if db == nil {
		return fmt.Errorf("translation source rollout preflight: nil database")
	}

	var contractApplied bool
	if err := db.QueryRow(ctx, `SELECT EXISTS (
		SELECT 1 FROM schema_migrations WHERE version = $1
	)`, migrate.TranslationSourceContractMigrationID).Scan(&contractApplied); err != nil {
		return fmt.Errorf("translation source rollout preflight: read migration %s: %w", migrate.TranslationSourceContractMigrationID, err)
	}
	if contractApplied {
		return fmt.Errorf(
			"TRANSLATION_SOURCE_ROLLOUT=compat is forbidden after contract migration %s; use TRANSLATION_SOURCE_ROLLOUT=strict",
			migrate.TranslationSourceContractMigrationID,
		)
	}
	return nil
}

// aggregateCacheInvalidator 返回把一次失效同时打到标签与域名两份聚合缓存上的
// 失效器。
//
// 两者必须成对失效：链接的增、删、以及解析完成（pending → done，而域名聚合
// 的 SQL 带 status='done' 过滤）都会同时改变两份聚合。只打掉其中一个，侧栏的
// 标签计数与域名计数就会在整个 TTL 窗口里互相矛盾。
func (l *persistenceLayer) aggregateCacheInvalidator() service.MultiCacheInvalidator {
	invalidators := service.MultiCacheInvalidator{l.tagCache}
	if l.domainCache != nil {
		invalidators = append(invalidators, l.domainCache)
	}
	return invalidators
}
