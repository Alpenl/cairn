package service

import (
	"strings"

	"webtag/internal/model"
	"webtag/internal/observability"
	"webtag/internal/repository"
)

func (p *ParsePipeline) recordLibraryClassification(requested model.RequestedLibraryKind, final model.LibraryKind, source model.LibraryKindSource, confidence float32) {
	if p == nil {
		return
	}
	recordLibraryClassificationMetric(p.metrics, requested, final, source, confidence)
}

func (f collectionFinalizer) recordLibraryClassification(requested model.RequestedLibraryKind, final model.LibraryKind, source model.LibraryKindSource, confidence float32) {
	recordLibraryClassificationMetric(f.metrics, requested, final, source, confidence)
}

func recordLibraryClassificationMetric(metrics *observability.Metrics, requested model.RequestedLibraryKind, final model.LibraryKind, source model.LibraryKindSource, confidence float32) {
	if metrics == nil || metrics.LibraryClassificationTotal == nil {
		return
	}
	metrics.LibraryClassificationTotal.WithLabelValues(string(requested), string(final), string(source), classificationConfidenceBand(confidence), libraryClassifierVersion).Inc()
}

func classificationConfidenceBand(value float32) string {
	switch {
	case value >= .8:
		return "high"
	case value >= .5:
		return "medium"
	default:
		return "low"
	}
}

func (p *ParsePipeline) recordSiteAggregation(result repository.SiteAggregateResult, identityKey string) {
	if p == nil {
		return
	}
	recordSiteAggregationMetric(p.metrics, result, identityKey)
}

func (f collectionFinalizer) recordSiteAggregation(result repository.SiteAggregateResult, identityKey string) {
	recordSiteAggregationMetric(f.metrics, result, identityKey)
}

func recordSiteAggregationMetric(metrics *observability.Metrics, result repository.SiteAggregateResult, identityKey string) {
	if metrics == nil {
		return
	}
	adapter := "host"
	parts := strings.SplitN(identityKey, ":", 3)
	if len(parts) == 3 && parts[1] != "" {
		adapter = parts[1]
	}
	if metrics.SiteAggregationTotal != nil {
		outcome := "existing"
		if result.CreatedSite {
			outcome = "created"
		}
		metrics.SiteAggregationTotal.WithLabelValues(outcome, adapter).Inc()
	}
	if metrics.SiteEntryTotal != nil {
		op := "recollected"
		if result.CreatedEntry {
			op = "created"
		}
		metrics.SiteEntryTotal.WithLabelValues(op).Inc()
	}
}
