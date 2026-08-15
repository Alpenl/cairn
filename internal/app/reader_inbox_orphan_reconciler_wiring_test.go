package app

import (
	"context"
	"strings"
	"testing"
	"time"

	"webtag/internal/app/durablework"
	"webtag/internal/config"
	"webtag/internal/worker"
)

func TestBuildRuntimeWiresOneInboxCommandToRequestsAndOrphanReconciler(t *testing.T) {
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
	var captured worker.ReaderInboxOrphanReconcilerOptions
	options.newReaderInboxOrphanReconciler = func(
		opts worker.ReaderInboxOrphanReconcilerOptions,
	) (*worker.ReaderInboxOrphanReconciler, error) {
		captured = opts
		return worker.NewReaderInboxOrphanReconciler(opts)
	}
	var requestCommands *durablework.InboxCommands
	options.buildLinkFeature = func(input linkFeatureOptions) (linkFeature, error) {
		requestCommands = input.shared.inboxCommands
		return buildLinkFeature(input)
	}

	runtime, err := buildRuntimeWithOptions(t.Context(), productionRuntimeMatrixConfig(), options)
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
	if layer == nil || requestCommands == nil {
		t.Fatal("production persistence or request Inbox commands were not captured")
	}
	if captured.Repairer != requestCommands {
		t.Fatalf("orphan repairer = %T %p, want request commands %p", captured.Repairer, captured.Repairer, requestCommands)
	}
	if captured.Logger != layer.logger || captured.Metrics != layer.metrics {
		t.Fatalf("observability wiring = logger %p metrics %p, want layer logger %p metrics %p",
			captured.Logger, captured.Metrics, layer.logger, layer.metrics)
	}
}

func TestBuildRuntimeRejectsReaderInboxReconcilerInstanceSubstitution(t *testing.T) {
	t.Parallel()
	probe := newProductionRuntimeBuildProbe()
	options := probe.options()
	options.acquisitions.beforeStartTransfer = func(resources *runtimeStartResources) {
		resources.readerInboxReconciler = &worker.ReaderInboxOrphanReconciler{}
	}

	runtime, err := buildRuntimeWithOptions(t.Context(), productionRuntimeMatrixConfig(), options)
	if runtime != nil {
		_ = runtime.Close(t.Context())
		t.Fatalf("buildRuntimeWithOptions() runtime = %+v, want nil", runtime)
	}
	if err == nil || !strings.Contains(err.Error(), runtimeBuildReaderInboxReconciler) ||
		!strings.Contains(err.Error(), "different instance") {
		t.Fatalf("buildRuntimeWithOptions() error = %v, want reader Inbox reconciler handoff mismatch", err)
	}
}
