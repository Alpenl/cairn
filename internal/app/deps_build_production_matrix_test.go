package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"webtag/internal/config"
	"webtag/internal/database"
	"webtag/internal/middleware"
	"webtag/internal/observability"
	"webtag/internal/repository"
	"webtag/internal/service"
	"webtag/internal/worker"
)

var productionBuildCheckpoints = []string{
	"tracer",
	"persistence",
	"outbound HTTP clients",
	"river queue",
	"reader Inbox orphan reconciler",
	"parse terminal reconciler",
	"translation terminal reconciler",
	"site payload cleaner",
	"site embedding backfill",
	"feed scheduler",
	"idempotency cache",
	"rate limiter",
	"historical migration",
}

var productionStartCheckpoints = []string{
	"outbound HTTP clients",
	"idempotency cache",
	"rate limiter",
	"river queue",
	"reader Inbox orphan reconciler",
	"parse terminal reconciler",
	"translation terminal reconciler",
	"feed scheduler",
	"site payload cleaner",
	"site embedding backfill",
	"historical migration",
}

var productionRuntimeResourceTypes = map[string]reflect.Type{
	runtimeBuildTracer:                reflect.TypeOf(observability.TracerShutdown(nil)),
	runtimeBuildPersistenceLayer:      reflect.TypeOf((*persistenceLayer)(nil)),
	runtimeBuildHTTPClients:           reflect.TypeOf((*runtimeHTTPClientOwner)(nil)),
	runtimeBuildRiverQueue:            reflect.TypeOf((*worker.RiverQueue)(nil)),
	runtimeBuildReaderInboxReconciler: reflect.TypeOf((*worker.ReaderInboxOrphanReconciler)(nil)),
	runtimeBuildParseReconciler:       reflect.TypeOf((*worker.ParseTerminalReconciler)(nil)),
	runtimeBuildTranslationReconciler: reflect.TypeOf((*worker.TranslationTerminalReconciler)(nil)),
	runtimeBuildSitePayloadCleaner:    reflect.TypeOf((*worker.SitePayloadCleaner)(nil)),
	runtimeBuildSiteEmbeddingBackfill: reflect.TypeOf((*worker.SiteEmbeddingBackfillWorker)(nil)),
	runtimeBuildFeedScheduler:         reflect.TypeOf((*worker.FeedScheduler)(nil)),
	runtimeBuildIdempotencyCache:      reflect.TypeOf((*middleware.PGIdempotencyCache)(nil)),
	runtimeBuildRateLimiter:           reflect.TypeOf((*middleware.RateLimiterController)(nil)),
	runtimeBuildHistoricalMigration:   reflect.TypeOf((*worker.HistoricalMigrationWorker)(nil)),
}

func TestProductionStartInventoryOmitsDisabledHistoricalMigration(t *testing.T) {
	t.Parallel()

	backgrounds := (runtimeBuildOptions{}).startInventory(runtimeStartResources{
		// Preserve the typed-nil shape produced when an optional concrete
		// pointer is assigned to the lifecycle interface.
		historicalMigration: (*runtimeMatrixBackgroundProbe)(nil),
	})
	if len(backgrounds) != len(productionStartCheckpoints)-1 {
		t.Fatalf("disabled start inventory length = %d, want %d", len(backgrounds), len(productionStartCheckpoints)-1)
	}
	for index, wantName := range productionStartCheckpoints[:len(productionStartCheckpoints)-1] {
		if got := backgrounds[index].name; got != wantName {
			t.Fatalf("disabled start inventory[%d] = %q, want %q", index, got, wantName)
		}
	}
}

func TestBuildRuntimeProductionStartOmitsDisabledHistoricalMigration(t *testing.T) {
	t.Parallel()

	probe := newProductionRuntimeStartProbe("", nil)
	cfg := productionRuntimeMatrixConfig()
	cfg.SiteMigrationEnabled = false
	runtime, err := buildRuntimeWithOptions(t.Context(), cfg, probe.options())
	if err != nil {
		t.Fatalf("buildRuntimeWithOptions() error = %v", err)
	}
	if err := runtime.Start(t.Context()); err != nil {
		t.Fatalf("Runtime.Start() error = %v", err)
	}
	if err := runtime.Close(t.Context()); err != nil {
		t.Fatalf("Runtime.Close() error = %v", err)
	}

	enabled := productionStartCheckpoints[:len(productionStartCheckpoints)-1]
	wantEvents := []string{"admit persistence owner"}
	for _, name := range enabled {
		wantEvents = append(wantEvents, "start "+name)
	}
	wantEvents = append(wantEvents, "close persistence admission")
	for index := len(enabled) - 1; index >= 0; index-- {
		wantEvents = append(wantEvents, "stop "+enabled[index])
	}
	wantEvents = append(wantEvents,
		"revoke persistence owner",
		"drain persistence",
		"close persistence",
		"shutdown tracer",
	)
	if got := probe.eventsSnapshot(); !reflect.DeepEqual(got, wantEvents) {
		t.Fatalf("disabled migration lifecycle events = %v, want %v", got, wantEvents)
	}
	if calls := probe.stopCallsFor(runtimeBuildHistoricalMigration); calls != 0 {
		t.Fatalf("disabled historical migration Stop callback calls = %d, want 0", calls)
	}
	if running := probe.runningSnapshot(); len(running) != 0 {
		t.Fatalf("running loops after Runtime.Close() = %v, want none", running)
	}
	if withoutOwner := probe.cleanupWithoutOwnerSnapshot(); len(withoutOwner) != 0 {
		t.Fatalf("background cleanup after persistence owner revoke = %v, want none", withoutOwner)
	}
}

func TestBuildRuntimeValidatesDisabledHistoricalMigrationHandoff(t *testing.T) {
	t.Parallel()

	probe := newProductionRuntimeBuildProbe()
	options := probe.options()
	options.acquisitions.beforeStartTransfer = func(resources *runtimeStartResources) {
		resources.historicalMigration = &lifecycleQueueStub{}
	}
	cfg := productionRuntimeMatrixConfig()
	cfg.SiteMigrationEnabled = false

	runtime, err := buildRuntimeWithOptions(t.Context(), cfg, options)
	if runtime != nil {
		_ = runtime.Close(t.Context())
		t.Fatalf("buildRuntimeWithOptions() runtime = %+v, want nil", runtime)
	}
	if err == nil || !strings.Contains(err.Error(), runtimeBuildHistoricalMigration) ||
		!strings.Contains(err.Error(), "different instance") {
		t.Fatalf("buildRuntimeWithOptions() error = %v, want disabled historical migration handoff mismatch", err)
	}
}

func TestBuildRuntimeProductionStartInventoryBindsConcreteResources(t *testing.T) {
	t.Parallel()

	probe := newProductionRuntimeStartProbe("", nil)
	runtime, err := buildRuntimeWithOptions(
		t.Context(),
		productionRuntimeMatrixConfig(),
		probe.options(),
	)
	if err != nil {
		t.Fatalf("buildRuntimeWithOptions() error = %v", err)
	}
	t.Cleanup(func() { _ = runtime.Close(t.Context()) })
	probe.assertBackgroundTypes(t, productionStartCheckpoints)
}

func TestBuildRuntimeRejectsStartResourceInstanceSubstitution(t *testing.T) {
	t.Parallel()

	probe := newProductionRuntimeBuildProbe()
	options := probe.options()
	var acquiredHTTPClients *runtimeHTTPClientOwner
	options.acquisitions.afterAcquire = func(name string, resource any) error {
		if name == runtimeBuildHTTPClients {
			acquiredHTTPClients = resource.(*runtimeHTTPClientOwner)
		}
		return nil
	}
	options.acquisitions.beforeStartTransfer = func(resources *runtimeStartResources) {
		resources.httpClients = newRuntimeHTTPClientOwner()
	}

	runtime, err := buildRuntimeWithOptions(t.Context(), productionRuntimeMatrixConfig(), options)
	if runtime != nil {
		_ = runtime.Close(t.Context())
		t.Fatalf("buildRuntimeWithOptions() runtime = %+v, want nil", runtime)
	}
	if err == nil || !strings.Contains(err.Error(), "outbound HTTP clients") ||
		!strings.Contains(err.Error(), "different instance") {
		t.Fatalf("buildRuntimeWithOptions() error = %v, want outbound HTTP client instance mismatch", err)
	}
	if acquiredHTTPClients == nil {
		t.Fatal("production acquisition did not expose the acquired HTTP client owner")
	}
	acquiredHTTPClients.mu.Lock()
	stopped := acquiredHTTPClients.stopped
	acquiredHTTPClients.mu.Unlock()
	if !stopped {
		t.Fatal("rejected start-resource transfer did not clean the acquired HTTP client owner")
	}
}

func TestBuildRuntimeRejectsRouterResourceInstanceSubstitution(t *testing.T) {
	t.Parallel()

	probe := newProductionRuntimeBuildProbe()
	options := probe.options()
	options.acquisitions.beforeRouterTransfer = func(dependencies *runtimeRouterDependencies) {
		dependencies.idempotencyCache = &middleware.PGIdempotencyCache{}
	}

	runtime, err := buildRuntimeWithOptions(t.Context(), productionRuntimeMatrixConfig(), options)
	if runtime != nil {
		_ = runtime.Close(t.Context())
		t.Fatalf("buildRuntimeWithOptions() runtime = %+v, want nil", runtime)
	}
	if err == nil || !strings.Contains(err.Error(), runtimeBuildIdempotencyCache) ||
		!strings.Contains(err.Error(), "different instance") {
		t.Fatalf("buildRuntimeWithOptions() error = %v, want idempotency cache router handoff mismatch", err)
	}
}

func TestRuntimeRouterBindingRevalidatesAtConsumption(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name   string
		mutate func(*runtimeRouterBinding)
		want   string
	}{
		{
			name: "idempotency cache",
			mutate: func(binding *runtimeRouterBinding) {
				binding.dependencies.idempotencyCache = &middleware.PGIdempotencyCache{}
			},
			want: runtimeBuildIdempotencyCache,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			idempotencyCache := &middleware.PGIdempotencyCache{}
			acquisitions := newProductionRuntimeBuildAcquisitions(runtimeBuildAcquisitionHooks{})
			acquisitions.rememberResource(runtimeBuildIdempotencyCache, idempotencyCache)

			binding, err := acquisitions.BindRouterDependencies(newRuntimeRouterDependencies(
				idempotencyCache,
			))
			if err != nil {
				t.Fatalf("BindRouterDependencies() error = %v", err)
			}
			tc.mutate(binding)
			if _, _, err := binding.buildRuntimeRouter(config.Config{}, nil, runtimeServices{}); err == nil ||
				!strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), "different instance") {
				t.Fatalf("buildRuntimeRouter() error = %v, want post-bind %s identity mismatch", err, tc.want)
			}
		})
	}
}

func TestProductionAcquisitionRejectsSplitResourceAndCleanupOwnership(t *testing.T) {
	t.Parallel()

	resource := &lifecycleQueueStub{}
	other := &lifecycleQueueStub{}
	acquisitions := newProductionRuntimeBuildAcquisitions(runtimeBuildAcquisitionHooks{})
	err := acquisitions.Acquire(runtimeBuildTracer, func() (runtimeAcquiredResource, error) {
		return runtimeAcquiredResource{
			resource: resource,
			cleanup:  other.Stop,
		}, nil
	})
	if err == nil || !strings.Contains(err.Error(), "bound ownership") {
		t.Fatalf("Acquire() error = %v, want bound ownership error", err)
	}

	finalErr, cleanupErr := acquisitions.CleanupFailure(t.Context(), time.Second, err)
	if finalErr == nil || cleanupErr != nil {
		t.Fatalf("CleanupFailure() = (%v, %v), want ownership error and successful cleanup", finalErr, cleanupErr)
	}
	if other.stops != 1 {
		t.Fatal("rejected split ownership did not retain its partial-construction cleanup")
	}
}

func TestBuildRuntimeProductionAcquisitionFailureMatrix(t *testing.T) {
	for failAt, failureName := range productionBuildCheckpoints {
		failAt := failAt
		failureName := failureName
		t.Run(failureName, func(t *testing.T) {
			constructorErr := errors.New(failureName + " constructor failed")
			probe := newProductionRuntimeBuildProbe()
			options := probe.options()
			options.acquisitions.afterAcquire = func(name string, resource any) error {
				probe.recordAcquiredResource(name, resource)
				probe.record("acquire " + name)
				if name == failureName {
					return constructorErr
				}
				return nil
			}
			options.acquisitions.beforeCleanup = func(name string, resource any) {
				probe.recordCleanedResource(name, resource)
				probe.record("cleanup " + name)
			}

			runtime, err := buildRuntimeWithOptions(t.Context(), productionRuntimeMatrixConfig(), options)
			if runtime != nil {
				_ = runtime.Close(t.Context())
				t.Fatalf("buildRuntimeWithOptions() runtime = %+v, want nil", runtime)
			}
			if !errors.Is(err, constructorErr) {
				t.Fatalf("buildRuntimeWithOptions() error = %v, want %v", err, constructorErr)
			}

			wantEvents := make([]string, 0, 2*(failAt+1))
			for _, name := range productionBuildCheckpoints[:failAt+1] {
				wantEvents = append(wantEvents, "acquire "+name)
			}
			for index := failAt; index >= 0; index-- {
				wantEvents = append(wantEvents, "cleanup "+productionBuildCheckpoints[index])
			}
			if got := probe.eventsSnapshot(); !reflect.DeepEqual(got, wantEvents) {
				t.Fatalf("production build events = %v, want %v", got, wantEvents)
			}
			probe.assertAcquisitionResources(t, productionBuildCheckpoints[:failAt+1])
		})
	}
}

func TestBuildRuntimeFeatureConstructorFailureUnwindsGlobalOwnershipInReverse(t *testing.T) {
	featureNames := []string{"link", "site", "feed"}
	for _, featureName := range featureNames {
		featureName := featureName
		t.Run(featureName, func(t *testing.T) {
			constructorErr := errors.New(featureName + " feature constructor failed")
			probe := newProductionRuntimeBuildProbe()
			options := probe.options()
			options.acquisitions.beforeCleanup = func(name string, _ any) {
				probe.record("cleanup " + name)
			}
			switch featureName {
			case "link":
				options.buildLinkFeature = func(input linkFeatureOptions) (linkFeature, error) {
					feature, err := buildLinkFeature(input)
					if err != nil {
						return feature, err
					}
					return feature, constructorErr
				}
			case "site":
				options.buildSiteFeature = func(input siteFeatureOptions) (siteFeature, error) {
					feature, err := buildSiteFeature(input)
					if err != nil {
						return feature, err
					}
					return feature, constructorErr
				}
			case "feed":
				options.buildFeedFeature = func(input feedFeatureOptions) (feedFeature, error) {
					feature, err := buildFeedFeature(input)
					if err != nil {
						return feature, err
					}
					return feature, constructorErr
				}
			}

			runtime, err := buildRuntimeWithOptions(
				t.Context(),
				productionRuntimeMatrixConfig(),
				options,
			)
			if runtime != nil {
				_ = runtime.Close(t.Context())
				t.Fatalf("buildRuntimeWithOptions() runtime = %+v, want nil", runtime)
			}
			if !errors.Is(err, constructorErr) {
				t.Fatalf("buildRuntimeWithOptions() error = %v, want %v", err, constructorErr)
			}

			lastOwned := 8 // site embedding backfill
			if featureName == "feed" {
				lastOwned = 9 // partially constructed scheduler belongs to the feed bundle
			}
			wantEvents := make([]string, 0, lastOwned+1)
			for index := lastOwned; index >= 0; index-- {
				wantEvents = append(wantEvents, "cleanup "+productionBuildCheckpoints[index])
			}
			if got := probe.eventsSnapshot(); !reflect.DeepEqual(got, wantEvents) {
				t.Fatalf("feature cleanup events = %v, want %v", got, wantEvents)
			}
		})
	}
}

func TestBuildRuntimeProductionPersistencePartialConstructionCleanup(t *testing.T) {
	constructorErr := errors.New("persistence constructor failed after opening resource")
	buildCtx, cancelBuild := context.WithCancel(t.Context())
	defer cancelBuild()
	var events []string
	gate := &persistenceAcquisitionGateProbe{}
	shutdown := &persistencePoolShutdownProbe{}
	partial := &persistenceLayer{
		pool:            new(pgxpool.Pool),
		acquisitionGate: gate,
		poolShutdown:    shutdown,
	}
	options := runtimeBuildOptions{
		initTracer: func(context.Context, observability.InitTracerOptions) (observability.TracerShutdown, error) {
			return func(context.Context) error {
				return nil
			}, nil
		},
		openPersistence: func(context.Context, config.Config) (*persistenceLayer, error) {
			cancelBuild()
			return partial, constructorErr
		},
		acquisitions: runtimeBuildAcquisitionHooks{
			beforeCleanup: func(name string, _ any) {
				events = append(events, "cleanup "+name)
			},
		},
	}

	runtime, err := buildRuntimeWithOptions(buildCtx, productionRuntimeMatrixConfig(), options)
	if runtime != nil {
		t.Fatalf("buildRuntimeWithOptions() runtime = %+v, want nil", runtime)
	}
	if !errors.Is(err, constructorErr) {
		t.Fatalf("buildRuntimeWithOptions() error = %v, want %v", err, constructorErr)
	}
	if want := []string{"cleanup persistence", "cleanup tracer"}; !reflect.DeepEqual(events, want) {
		t.Fatalf("partial construction cleanup events = %v, want %v", events, want)
	}
	if gate.drainCtx == nil || gate.drainErrAtCall != nil {
		t.Fatalf("partial persistence drain context error at call = %v, want detached active context", gate.drainErrAtCall)
	}
	if !gate.drainHadDeadlineAtCall {
		t.Fatal("partial persistence drain context has no deadline")
	}
	if gate.drainPool != partial.pool || shutdown.closePool != partial.pool {
		t.Fatal("partial persistence cleanup did not retain the acquired layer pool")
	}
}

func TestRuntimePersistenceAcquisitionDerivesCleanupFromSameLayer(t *testing.T) {
	t.Parallel()

	pool := new(pgxpool.Pool)
	gate := &persistenceAcquisitionGateProbe{}
	shutdown := &persistencePoolShutdownProbe{}
	layer := &persistenceLayer{pool: pool, acquisitionGate: gate, poolShutdown: shutdown}
	resource, cleanup, bound := newRuntimePersistenceAcquiredResource(layer).resolveOwnership()
	if !bound || resource != layer || cleanup == nil {
		t.Fatalf("persistence ownership = (%T, %v, %t), want the acquired layer and bound cleanup", resource, cleanup != nil, bound)
	}
	callerCtx := &persistenceCallerContext{Context: t.Context()}
	if err := cleanup(callerCtx); err != nil {
		t.Fatalf("bound persistence cleanup error = %v", err)
	}
	if gate.closed != 1 || shutdown.begun != 1 {
		t.Fatalf("bound persistence admission close calls = (%d, %d), want (1, 1)", gate.closed, shutdown.begun)
	}
	if gate.drainCtx != callerCtx || gate.drainPool != pool {
		t.Fatal("bound persistence drain did not receive the caller context and acquired pool")
	}
	if shutdown.closeCtx != callerCtx || shutdown.closePool != pool {
		t.Fatal("bound persistence destructor did not receive the caller context and acquired pool")
	}
}

func TestBuildRuntimeProductionStartFailureMatrix(t *testing.T) {
	for failAt, failureName := range productionStartCheckpoints {
		failAt := failAt
		failureName := failureName
		t.Run(failureName, func(t *testing.T) {
			startErr := errors.New(failureName + " start failed")
			probe := newProductionRuntimeStartProbe(failureName, startErr)

			runtime, err := buildRuntimeWithOptions(
				t.Context(),
				productionRuntimeMatrixConfig(),
				probe.options(),
			)
			if err != nil {
				t.Fatalf("buildRuntimeWithOptions() error = %v", err)
			}
			if runtime == nil {
				t.Fatal("buildRuntimeWithOptions() runtime = nil")
			}
			t.Cleanup(func() {
				cleanupCtx, cancel := context.WithTimeout(context.Background(), time.Second)
				defer cancel()
				_ = runtime.Close(cleanupCtx)
			})

			err = runtime.Start(t.Context())
			if !errors.Is(err, startErr) {
				t.Fatalf("Runtime.Start() error = %v, want %v", err, startErr)
			}
			if running := probe.runningSnapshot(); len(running) != 0 {
				t.Fatalf("running loops after failed Runtime.Start() = %v, want none", running)
			}
			if withoutOwner := probe.cleanupWithoutOwnerSnapshot(); len(withoutOwner) != 0 {
				t.Fatalf("background cleanup after persistence owner revoke = %v, want none", withoutOwner)
			}
			for _, name := range productionStartCheckpoints {
				if calls := probe.stopCallsFor(name); calls != 1 {
					t.Fatalf("%s Stop callback calls = %d, want exactly 1", name, calls)
				}
			}

			wantEvents := []string{"admit persistence owner"}
			for _, name := range productionStartCheckpoints[:failAt+1] {
				wantEvents = append(wantEvents, "start "+name)
			}
			for index := failAt - 1; index >= 0; index-- {
				wantEvents = append(wantEvents, "stop "+productionStartCheckpoints[index])
			}
			wantEvents = append(wantEvents, "close persistence admission")
			for index := len(productionStartCheckpoints) - 1; index >= failAt; index-- {
				wantEvents = append(wantEvents, "close "+productionStartCheckpoints[index])
			}
			wantEvents = append(wantEvents, "revoke persistence owner", "drain persistence", "close persistence", "shutdown tracer")
			if got := probe.eventsSnapshot(); !reflect.DeepEqual(got, wantEvents) {
				t.Fatalf("production start events = %v, want %v", got, wantEvents)
			}

			if closeErr := runtime.Close(t.Context()); closeErr != nil {
				t.Fatalf("second Runtime.Close() error = %v", closeErr)
			}
			if got := probe.eventsSnapshot(); !reflect.DeepEqual(got, wantEvents) {
				t.Fatalf("idempotent close events = %v, want unchanged %v", got, wantEvents)
			}
		})
	}
}

type productionRuntimeBuildProbe struct {
	mu                sync.Mutex
	events            []string
	acquiredResources map[string]any
	cleanedResources  map[string]any
	layer             *persistenceLayer
}

func newProductionRuntimeBuildProbe() *productionRuntimeBuildProbe {
	return &productionRuntimeBuildProbe{
		acquiredResources: make(map[string]any),
		cleanedResources:  make(map[string]any),
		layer:             productionRuntimeMatrixPersistenceLayer(),
	}
}

func (p *productionRuntimeBuildProbe) options() runtimeBuildOptions {
	return runtimeBuildOptions{
		initTracer: observability.InitTracer,
		openPersistence: func(context.Context, config.Config) (*persistenceLayer, error) {
			return p.layer, nil
		},
	}
}

func (p *productionRuntimeBuildProbe) record(event string) {
	p.mu.Lock()
	p.events = append(p.events, event)
	p.mu.Unlock()
}

func (p *productionRuntimeBuildProbe) eventsSnapshot() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.events...)
}

func (p *productionRuntimeBuildProbe) recordAcquiredResource(name string, resource any) {
	p.mu.Lock()
	p.acquiredResources[name] = resource
	p.mu.Unlock()
}

func (p *productionRuntimeBuildProbe) recordCleanedResource(name string, resource any) {
	p.mu.Lock()
	p.cleanedResources[name] = resource
	p.mu.Unlock()
}

func (p *productionRuntimeBuildProbe) assertAcquisitionResources(t *testing.T, names []string) {
	t.Helper()
	p.mu.Lock()
	acquired := make(map[string]any, len(p.acquiredResources))
	cleaned := make(map[string]any, len(p.cleanedResources))
	for name, resource := range p.acquiredResources {
		acquired[name] = resource
	}
	for name, resource := range p.cleanedResources {
		cleaned[name] = resource
	}
	p.mu.Unlock()

	for _, name := range names {
		wantType := productionRuntimeResourceTypes[name]
		if gotType := reflect.TypeOf(acquired[name]); gotType != wantType {
			t.Fatalf("acquired %s resource type = %v, want %v", name, gotType, wantType)
		}
		if !sameRuntimeResource(acquired[name], cleaned[name]) {
			t.Fatalf("cleanup %s resource = %T, want the acquired %T instance", name, cleaned[name], acquired[name])
		}
	}
	if resource, ok := acquired[runtimeBuildHTTPClients]; ok {
		owner := resource.(*runtimeHTTPClientOwner)
		owner.mu.Lock()
		stopped := owner.stopped
		owner.mu.Unlock()
		if !stopped {
			t.Fatal("outbound HTTP client owner was not stopped by its production acquisition cleanup")
		}
	}
}

func sameRuntimeResource(left, right any) bool {
	if left == nil || right == nil || reflect.TypeOf(left) != reflect.TypeOf(right) {
		return left == nil && right == nil
	}
	leftValue := reflect.ValueOf(left)
	rightValue := reflect.ValueOf(right)
	switch leftValue.Kind() {
	case reflect.Func, reflect.Pointer:
		return leftValue.Pointer() == rightValue.Pointer()
	default:
		return false
	}
}

type productionRuntimeStartProbe struct {
	mu                  sync.Mutex
	events              []string
	running             map[string]bool
	stopCalls           map[string]int
	cleanupWithoutOwner []string
	ownerActive         bool
	actualBackgrounds   map[string]runtimeManagedBackground
	failName            string
	failErr             error
	layer               *persistenceLayer
	persistence         *runtimeMatrixPersistenceProbe
	ownerMarker         *runtimeMatrixOwnerMarker
}

type runtimeMatrixOwnerContextKey struct{}

type runtimeMatrixOwnerMarker struct {
	value int
}

func newProductionRuntimeStartProbe(failName string, failErr error) *productionRuntimeStartProbe {
	probe := &productionRuntimeStartProbe{
		running:           make(map[string]bool),
		stopCalls:         make(map[string]int),
		actualBackgrounds: make(map[string]runtimeManagedBackground),
		failName:          failName,
		failErr:           failErr,
		layer:             productionRuntimeMatrixPersistenceLayer(),
		ownerMarker:       &runtimeMatrixOwnerMarker{value: 7},
	}
	probe.persistence = &runtimeMatrixPersistenceProbe{probe: probe}
	probe.layer.acquisitionGate = probe.persistence
	probe.layer.poolShutdown = probe.persistence
	return probe
}

func (p *productionRuntimeStartProbe) options() runtimeBuildOptions {
	return runtimeBuildOptions{
		initTracer: func(ctx context.Context, options observability.InitTracerOptions) (observability.TracerShutdown, error) {
			shutdown, err := observability.InitTracer(ctx, options)
			if err != nil {
				return nil, err
			}
			return func(shutdownCtx context.Context) error {
				p.record("shutdown tracer")
				return shutdown(shutdownCtx)
			}, nil
		},
		openPersistence: func(context.Context, config.Config) (*persistenceLayer, error) {
			return p.layer, nil
		},
		wrapBackground: func(name string, background runtimeManagedBackground) runtimeManagedBackground {
			p.observeBackground(name, background)
			return &runtimeMatrixBackgroundProbe{
				name:       name,
				actual:     background,
				owner:      p,
				startError: p.startError(name),
			}
		},
	}
}

func (p *productionRuntimeStartProbe) observeBackground(name string, background runtimeManagedBackground) {
	p.mu.Lock()
	p.actualBackgrounds[name] = background
	p.mu.Unlock()
}

func (p *productionRuntimeStartProbe) assertBackgroundTypes(t *testing.T, names []string) {
	t.Helper()
	p.mu.Lock()
	backgrounds := make(map[string]runtimeManagedBackground, len(p.actualBackgrounds))
	for name, background := range p.actualBackgrounds {
		backgrounds[name] = background
	}
	p.mu.Unlock()
	for _, name := range names {
		wantType := productionRuntimeResourceTypes[name]
		if gotType := reflect.TypeOf(backgrounds[name]); gotType != wantType {
			t.Fatalf("start inventory %s resource type = %v, want %v", name, gotType, wantType)
		}
	}
}

func (p *productionRuntimeStartProbe) startError(name string) error {
	if name == p.failName {
		return p.failErr
	}
	return nil
}

func (p *productionRuntimeStartProbe) record(event string) {
	p.mu.Lock()
	p.events = append(p.events, event)
	p.mu.Unlock()
}

func (p *productionRuntimeStartProbe) setRunning(name string, running bool) {
	p.mu.Lock()
	if running {
		p.running[name] = true
	} else {
		delete(p.running, name)
	}
	p.mu.Unlock()
}

func (p *productionRuntimeStartProbe) eventsSnapshot() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.events...)
}

func (p *productionRuntimeStartProbe) runningSnapshot() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	running := make([]string, 0, len(p.running))
	for name := range p.running {
		running = append(running, name)
	}
	return running
}

func (p *productionRuntimeStartProbe) observeStop(name string) {
	p.mu.Lock()
	p.stopCalls[name]++
	if !p.ownerActive {
		p.cleanupWithoutOwner = append(p.cleanupWithoutOwner, name)
	}
	p.mu.Unlock()
}

func (p *productionRuntimeStartProbe) stopCallsFor(name string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.stopCalls[name]
}

func (p *productionRuntimeStartProbe) cleanupWithoutOwnerSnapshot() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.cleanupWithoutOwner...)
}

func (p *productionRuntimeStartProbe) admitOwner() {
	p.mu.Lock()
	p.ownerActive = true
	p.events = append(p.events, "admit persistence owner")
	p.mu.Unlock()
}

func (p *productionRuntimeStartProbe) revokeOwner() {
	p.mu.Lock()
	p.ownerActive = false
	p.events = append(p.events, "revoke persistence owner")
	p.mu.Unlock()
}

type runtimeMatrixPersistenceProbe struct {
	probe *productionRuntimeStartProbe
}

func (p *runtimeMatrixPersistenceProbe) AdmitOwner(ctx context.Context) (context.Context, *database.AcquisitionOwner, error) {
	p.probe.admitOwner()
	return context.WithValue(ctx, runtimeMatrixOwnerContextKey{}, p.probe.ownerMarker), nil, nil
}

func (p *runtimeMatrixPersistenceProbe) CloseAdmission() {
	p.probe.record("close persistence admission")
}

func (p *runtimeMatrixPersistenceProbe) Drain(context.Context, *pgxpool.Pool) error {
	p.probe.revokeOwner()
	p.probe.record("drain persistence")
	return nil
}

func (*runtimeMatrixPersistenceProbe) BeginShutdown() {}

func (p *runtimeMatrixPersistenceProbe) Close(context.Context, *pgxpool.Pool) error {
	p.probe.record("close persistence")
	return nil
}

type runtimeMatrixBackgroundProbe struct {
	mu         sync.Mutex
	name       string
	actual     runtimeManagedBackground
	owner      *productionRuntimeStartProbe
	startError error
	cancel     context.CancelFunc
	done       chan struct{}
	started    bool
	closed     bool
}

func (p *runtimeMatrixBackgroundProbe) Start(ctx context.Context) error {
	p.owner.record("start " + p.name)
	if got := ctx.Value(runtimeMatrixOwnerContextKey{}); got != p.owner.ownerMarker {
		return fmt.Errorf("start %s: persistence owner marker = %p, want %p", p.name, got, p.owner.ownerMarker)
	}
	if p.startError != nil {
		return p.startError
	}

	p.mu.Lock()
	loopCtx, cancel := context.WithCancel(ctx)
	p.cancel = cancel
	p.done = make(chan struct{})
	p.started = true
	done := p.done
	p.mu.Unlock()
	p.owner.setRunning(p.name, true)
	go func() {
		<-loopCtx.Done()
		p.owner.setRunning(p.name, false)
		close(done)
	}()
	return nil
}

func (p *runtimeMatrixBackgroundProbe) Stop(ctx context.Context) error {
	p.owner.observeStop(p.name)
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil
	}
	p.closed = true
	started := p.started
	cancel := p.cancel
	done := p.done
	p.mu.Unlock()

	if started {
		p.owner.record("stop " + p.name)
		cancel()
		select {
		case <-done:
		case <-ctx.Done():
			return ctx.Err()
		}
	} else {
		p.owner.record("close " + p.name)
	}
	return p.actual.Stop(ctx)
}

func productionRuntimeMatrixConfig() config.Config {
	return config.Config{
		AppEnv:                             "test",
		SessionSigningKey:                  "rf7-production-matrix-session-key",
		MetricsAuthToken:                   "rf7-metrics-token",
		TagCacheTTLMS:                      int((5 * time.Minute).Milliseconds()),
		TreeCacheTTLMS:                     int((5 * time.Minute).Milliseconds()),
		IdempotencyEnabled:                 true,
		IdempotencyTTLMS:                   int(time.Hour.Milliseconds()),
		SiteMigrationEnabled:               true,
		SiteMigrationBatch:                 10,
		SiteMigrationIntervalMS:            int(time.Minute.Milliseconds()),
		TranslationJobsRollout:             "strict-v2",
		TranslationSourceRollout:           config.TranslationSourceRolloutStrict,
		TranslationReconcileIntervalMS:     int(time.Minute.Milliseconds()),
		TranslationReconcileBatch:          100,
		TranslationReconcileRoundTimeoutMS: int((30 * time.Second).Milliseconds()),
		TranslationReconcileMissingAfterMS: int((6 * time.Hour).Milliseconds()),
		ParseReconcileIntervalMS:           int(time.Minute.Milliseconds()),
		ParseReconcileBatch:                100,
		ParseReconcileRoundTimeoutMS:       int((30 * time.Second).Milliseconds()),
		ParseReconcileMissingAfterMS:       int((6 * time.Hour).Milliseconds()),
		RiverTerminalRetentionMS:           int((7 * 24 * time.Hour).Milliseconds()),
		RiverMaxRecoveryDowntimeMS:         int((24 * time.Hour).Milliseconds()),
		DB: config.DBConfig{
			MaxConns:         1,
			ParseConcurrency: 1,
		},
		RateLimit: config.RateLimitConfig{RPS: 10, Burst: 10},
	}
}

func productionRuntimeMatrixPersistenceLayer() *persistenceLayer {
	pool := &pgxpool.Pool{}
	metrics := observability.NewMetrics()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	return &persistenceLayer{
		pool:                pool,
		acquisitionGate:     &persistenceAcquisitionGateProbe{},
		poolShutdown:        &persistencePoolShutdownProbe{},
		links:               repository.NewPGXLinkRepository(pool),
		jobs:                repository.NewPGXJobRepository(pool),
		tags:                repository.NewPGXTagRepository(pool),
		tree:                repository.NewPGXTreeRepository(pool),
		translations:        repository.NewPGXTranslationRepository(pool),
		concepts:            repository.NewPGXConceptRepository(pool),
		conceptProposals:    repository.NewPGXConceptProposalRepository(pool),
		idempotency:         repository.NewPGXIdempotencyRepository(pool),
		feeds:               repository.NewPGXFeedRepository(pool, pool),
		sites:               repository.NewPGXSiteRepository(pool),
		classificationRules: repository.NewPGXClassificationRuleRepository(pool),
		libraryReviews:      repository.NewPGXLibraryReviewRepository(pool),
		tagCache:            service.NewTagCache(5*time.Minute, nil),
		domainCache:         service.NewDomainSummaryCache(5*time.Minute, nil),
		readRevisions:       service.NewReadRevisionService(repository.NewPGXReadRevisionRepository(pool)),
		metrics:             metrics,
		logger:              logger,
	}
}
