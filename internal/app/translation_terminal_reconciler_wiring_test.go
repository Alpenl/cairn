package app

import (
	"context"
	"strings"
	"testing"
	"time"

	"webtag/internal/config"
	"webtag/internal/worker"
)

func TestBuildRuntimeRejectsTranslationReconcilerInstanceSubstitution(t *testing.T) {
	t.Parallel()

	probe := newProductionRuntimeBuildProbe()
	options := probe.options()
	options.acquisitions.beforeStartTransfer = func(resources *runtimeStartResources) {
		resources.translationReconciler = &worker.TranslationTerminalReconciler{}
	}

	runtime, err := buildRuntimeWithOptions(t.Context(), productionRuntimeMatrixConfig(), options)
	if runtime != nil {
		_ = runtime.Close(t.Context())
		t.Fatalf("buildRuntimeWithOptions() runtime = %+v, want nil", runtime)
	}
	if err == nil || !strings.Contains(err.Error(), runtimeBuildTranslationReconciler) ||
		!strings.Contains(err.Error(), "different instance") {
		t.Fatalf(
			"buildRuntimeWithOptions() error = %v, want translation reconciler handoff mismatch",
			err,
		)
	}
}

func TestBuildRuntimeWiresTranslationReconcilerConstructorOptions(t *testing.T) {
	t.Parallel()

	probe := newProductionRuntimeBuildProbe()
	options := probe.options()
	originalOpenPersistence := options.openPersistence
	var layer *persistenceLayer
	options.openPersistence = func(ctx context.Context, cfg config.Config) (*persistenceLayer, error) {
		var err error
		layer, err = originalOpenPersistence(ctx, cfg)
		return layer, err
	}
	var captured worker.TranslationTerminalReconcilerOptions
	options.newTranslationTerminalReconciler = func(
		opts worker.TranslationTerminalReconcilerOptions,
	) (*worker.TranslationTerminalReconciler, error) {
		captured = opts
		return worker.NewTranslationTerminalReconciler(opts)
	}
	cfg := productionRuntimeMatrixConfig()
	cfg.ExtensionAPIToken = "rf6b-constructor-options-token"
	cfg.TranslationReconcileIntervalMS = 12_001
	cfg.TranslationReconcileBatch = 317
	cfg.TranslationReconcileRoundTimeoutMS = 4_003
	cfg.TranslationReconcileMissingAfterMS = 19_007

	runtime, err := buildRuntimeWithOptions(t.Context(), cfg, options)
	if err != nil {
		t.Fatalf("buildRuntimeWithOptions() error = %v", err)
	}
	t.Cleanup(func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := runtime.Close(closeCtx); err != nil {
			t.Errorf("Runtime.Close() error = %v", err)
		}
	})
	if layer == nil {
		t.Fatal("persistence layer was not captured")
	}
	if captured.Pool != layer.pool || captured.Projector != layer.translations ||
		captured.LegacyAttempts != layer.translations {
		t.Fatalf("persistence wiring = pool %p projector %T resolver %T, want production layer identities",
			captured.Pool, captured.Projector, captured.LegacyAttempts)
	}
	if captured.Interval != 12_001*time.Millisecond || captured.BatchSize != 317 ||
		captured.RoundTimeout != 4_003*time.Millisecond || captured.MissingAfter != 19_007*time.Millisecond {
		t.Fatalf("reconcile knobs = interval %s batch %d timeout %s missing %s",
			captured.Interval, captured.BatchSize, captured.RoundTimeout, captured.MissingAfter)
	}
	if captured.Logger != layer.logger || captured.Metrics != layer.metrics {
		t.Fatalf("observability wiring = logger %p metrics %p, want layer logger %p metrics %p",
			captured.Logger, captured.Metrics, layer.logger, layer.metrics)
	}
}
