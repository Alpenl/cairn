package app

import (
	"context"
	"testing"
	"time"

	"webtag/internal/config"
	"webtag/internal/worker"
)

func TestBuildRuntimeWiresParseReconcilerConstructorOptions(t *testing.T) {
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
	var captured worker.ParseTerminalReconcilerOptions
	options.newParseTerminalReconciler = func(
		opts worker.ParseTerminalReconcilerOptions,
	) (*worker.ParseTerminalReconciler, error) {
		captured = opts
		return worker.NewParseTerminalReconciler(opts)
	}
	cfg := productionRuntimeMatrixConfig()
	cfg.ExtensionAPIToken = "rf6c-constructor-options-token"
	cfg.ParseReconcileIntervalMS = 12_013
	cfg.ParseReconcileBatch = 311
	cfg.ParseReconcileRoundTimeoutMS = 4_009
	cfg.ParseReconcileMissingAfterMS = 19_009

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
	if captured.Pool != layer.pool || captured.Projector == nil {
		t.Fatalf("persistence wiring = pool %p projector %T, want production layer identities",
			captured.Pool, captured.Projector)
	}
	if captured.Interval != 12_013*time.Millisecond || captured.BatchSize != 311 ||
		captured.RoundTimeout != 4_009*time.Millisecond || captured.MissingAfter != 19_009*time.Millisecond {
		t.Fatalf("reconcile knobs = interval %s batch %d timeout %s missing %s",
			captured.Interval, captured.BatchSize, captured.RoundTimeout, captured.MissingAfter)
	}
	if captured.Logger != layer.logger {
		t.Fatalf("logger wiring = %p, want layer logger %p", captured.Logger, layer.logger)
	}
}
