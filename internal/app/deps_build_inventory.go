package app

const (
	runtimeBuildTracer                = "tracer"
	runtimeBuildPersistenceLayer      = "persistence"
	runtimeBuildHTTPClients           = "outbound HTTP clients"
	runtimeBuildRiverQueue            = "river queue"
	runtimeBuildReaderInboxReconciler = "reader Inbox orphan reconciler"
	runtimeBuildParseReconciler       = "parse terminal reconciler"
	runtimeBuildTranslationReconciler = "translation terminal reconciler"
	runtimeBuildSitePayloadCleaner    = "site payload cleaner"
	runtimeBuildSiteEmbeddingBackfill = "site embedding backfill"
	runtimeBuildFeedScheduler         = "feed scheduler"
	runtimeBuildIdempotencyCache      = "idempotency cache"
	runtimeBuildRateLimiter           = "rate limiter"
	runtimeBuildHistoricalMigration   = "historical migration"
)

// productionRuntimeBuildInventory is checked while BuildRuntime executes. A
// missing, duplicate, or reordered Acquire call fails the build before a
// partially assembled graph can transfer ownership to Runtime.
var productionRuntimeBuildInventory = [...]string{
	runtimeBuildTracer,
	runtimeBuildPersistenceLayer,
	runtimeBuildHTTPClients,
	runtimeBuildRiverQueue,
	runtimeBuildReaderInboxReconciler,
	runtimeBuildParseReconciler,
	runtimeBuildTranslationReconciler,
	runtimeBuildSitePayloadCleaner,
	runtimeBuildSiteEmbeddingBackfill,
	runtimeBuildFeedScheduler,
	runtimeBuildIdempotencyCache,
	runtimeBuildRateLimiter,
	runtimeBuildHistoricalMigration,
}

type runtimeStartResources struct {
	httpClients           runtimeManagedBackground
	idempotencyCache      runtimeManagedBackground
	rateLimiter           runtimeManagedBackground
	riverQueue            runtimeManagedBackground
	readerInboxReconciler runtimeManagedBackground
	parseReconciler       runtimeManagedBackground
	translationReconciler runtimeManagedBackground
	feedScheduler         runtimeManagedBackground
	sitePayloadCleaner    runtimeManagedBackground
	siteEmbeddingBackfill runtimeManagedBackground
	historicalMigration   runtimeManagedBackground
}

type runtimeStartResourceBinding struct {
	name       string
	background runtimeManagedBackground
	optional   bool
}

func (r runtimeStartResources) bindings() []runtimeStartResourceBinding {
	bindings := []runtimeStartResourceBinding{
		{name: runtimeBuildHTTPClients, background: r.httpClients},
		{name: runtimeBuildIdempotencyCache, background: r.idempotencyCache},
		{name: runtimeBuildRateLimiter, background: r.rateLimiter},
		{name: runtimeBuildRiverQueue, background: r.riverQueue},
		{name: runtimeBuildReaderInboxReconciler, background: r.readerInboxReconciler},
		{name: runtimeBuildParseReconciler, background: r.parseReconciler},
		{name: runtimeBuildTranslationReconciler, background: r.translationReconciler},
		{name: runtimeBuildFeedScheduler, background: r.feedScheduler},
		{name: runtimeBuildSitePayloadCleaner, background: r.sitePayloadCleaner},
		{name: runtimeBuildSiteEmbeddingBackfill, background: r.siteEmbeddingBackfill},
		{name: runtimeBuildHistoricalMigration, background: r.historicalMigration, optional: true},
	}
	return bindings
}

func (o runtimeBuildOptions) startInventory(resources runtimeStartResources) []namedRuntimeBackground {
	bindings := resources.bindings()
	backgrounds := make([]namedRuntimeBackground, 0, len(bindings))
	for _, binding := range bindings {
		if binding.optional && isNilRuntimeResource(binding.background) {
			continue
		}
		backgrounds = append(backgrounds, o.namedBackground(binding.name, binding.background))
	}
	return backgrounds
}
