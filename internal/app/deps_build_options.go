package app

import (
	"context"

	"webtag/internal/config"
	"webtag/internal/observability"
	"webtag/internal/worker"
)

type runtimeTracerInitializer func(
	context.Context,
	observability.InitTracerOptions,
) (observability.TracerShutdown, error)

type persistenceLayerOpener func(context.Context, config.Config) (*persistenceLayer, error)

type runtimeBackgroundWrapper func(string, runtimeManagedBackground) runtimeManagedBackground
type runtimeHTTPClientOwnerFactory func() *runtimeHTTPClientOwner
type parseTerminalReconcilerConstructor func(
	worker.ParseTerminalReconcilerOptions,
) (*worker.ParseTerminalReconciler, error)
type readerInboxOrphanReconcilerConstructor func(
	worker.ReaderInboxOrphanReconcilerOptions,
) (*worker.ReaderInboxOrphanReconciler, error)
type translationTerminalReconcilerConstructor func(
	worker.TranslationTerminalReconcilerOptions,
) (*worker.TranslationTerminalReconciler, error)
type linkFeatureConstructor func(linkFeatureOptions) (linkFeature, error)
type siteFeatureConstructor func(siteFeatureOptions) (siteFeature, error)
type feedFeatureConstructor func(feedFeatureOptions) (feedFeature, error)

// runtimeBuildOptions keeps production BuildRuntime on one executable wiring
// path while allowing ownership tests to replace external boundaries and wrap
// the final background inventory. Its zero value selects every production
// implementation.
type runtimeBuildOptions struct {
	initTracer                       runtimeTracerInitializer
	openPersistence                  persistenceLayerOpener
	newHTTPClientOwner               runtimeHTTPClientOwnerFactory
	newReaderInboxOrphanReconciler   readerInboxOrphanReconcilerConstructor
	newParseTerminalReconciler       parseTerminalReconcilerConstructor
	newTranslationTerminalReconciler translationTerminalReconcilerConstructor
	buildLinkFeature                 linkFeatureConstructor
	buildSiteFeature                 siteFeatureConstructor
	buildFeedFeature                 feedFeatureConstructor
	acquisitions                     runtimeBuildAcquisitionHooks
	wrapBackground                   runtimeBackgroundWrapper
}

func (o runtimeBuildOptions) namedBackground(
	name string,
	background runtimeManagedBackground,
) namedRuntimeBackground {
	if background != nil && o.wrapBackground != nil {
		background = o.wrapBackground(name, background)
	}
	return namedRuntimeBackground{name: name, background: background}
}

func (o runtimeBuildOptions) withDefaults() runtimeBuildOptions {
	if o.initTracer == nil {
		o.initTracer = observability.InitTracer
	}
	if o.openPersistence == nil {
		o.openPersistence = openPersistenceLayer
	}
	if o.newHTTPClientOwner == nil {
		o.newHTTPClientOwner = newRuntimeHTTPClientOwner
	}
	if o.newParseTerminalReconciler == nil {
		o.newParseTerminalReconciler = worker.NewParseTerminalReconciler
	}
	if o.newReaderInboxOrphanReconciler == nil {
		o.newReaderInboxOrphanReconciler = worker.NewReaderInboxOrphanReconciler
	}
	if o.newTranslationTerminalReconciler == nil {
		o.newTranslationTerminalReconciler = worker.NewTranslationTerminalReconciler
	}
	if o.buildLinkFeature == nil {
		o.buildLinkFeature = buildLinkFeature
	}
	if o.buildSiteFeature == nil {
		o.buildSiteFeature = buildSiteFeature
	}
	if o.buildFeedFeature == nil {
		o.buildFeedFeature = buildFeedFeature
	}
	return o
}
