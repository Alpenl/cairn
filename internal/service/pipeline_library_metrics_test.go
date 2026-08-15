package service

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"webtag/internal/model"
	"webtag/internal/observability"
	"webtag/internal/repository"
)

func TestPipelineLibraryMetricsUseBoundedClassificationAndAggregationLabels(t *testing.T) {
	metrics := observability.NewMetrics()
	pipeline := &ParsePipeline{metrics: metrics}
	pipeline.recordLibraryClassification(model.RequestedLibraryKindAuto, model.LibraryKindReading, model.LibraryKindSourceAuto, .91)
	pipeline.recordSiteAggregation(repository.SiteAggregateResult{CreatedSite: true, CreatedEntry: true}, "v1:github:owner/repo")
	pipeline.recordSiteAggregation(repository.SiteAggregateResult{}, "v1:host:example.com")

	recorder := httptest.NewRecorder()
	metrics.Handler().ServeHTTP(recorder, httptest.NewRequest("GET", "/metrics", nil))
	body := recorder.Body.String()
	for _, want := range []string{
		`webtag_library_classification_total{confidence_band="high",final="reading",requested="auto",source="auto",version="library-v1"} 1`,
		`webtag_site_aggregation_total{adapter="github",result="created"} 1`,
		`webtag_site_aggregation_total{adapter="host",result="existing"} 1`,
		`webtag_site_entry_total{operation="created"} 1`,
		`webtag_site_entry_total{operation="recollected"} 1`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("metrics output missing %q:\n%s", want, body)
		}
	}
}

func TestClassificationConfidenceBandIsBounded(t *testing.T) {
	for _, tt := range []struct {
		value float32
		want  string
	}{{0, "low"}, {.5, "medium"}, {.79, "medium"}, {.8, "high"}, {1, "high"}} {
		if got := classificationConfidenceBand(tt.value); got != tt.want {
			t.Fatalf("classificationConfidenceBand(%v)=%q, want %q", tt.value, got, tt.want)
		}
	}
}

// TestEnrichmentSkippedMetric 锁住 fail-soft 补充步骤的可观测契约。
//
// 这两步在链接已写成 done 之后运行，失败只打 WARN。仓库里没有任何对账 worker，
// 因此一次 embedding 故障会在语义检索里留下永久空洞，而此前没有任何信号能报告
// 它。这个计数器是那个空洞的唯一出口——尤其 reason=failed，它直接等于「有多少
// 链接永久缺了这项数据」。
func TestEnrichmentSkippedMetric(t *testing.T) {
	t.Parallel()

	// 标签值写成字面量而非引用生产常量。got 与 want 同源时，改名（如
	// failed → error）不会让任何测试变红，而告警规则 key 在这些字符串上——
	// 静默改名 = 静默失效。这里钉死的是对外契约，不是内部标识符。
	const (
		metricEmbedding = "content_embedding"
		metricConcept   = "concept_attach"

		reasonNotWired   = "not_wired"
		reasonDisabled   = "disabled"
		reasonEmptyInput = "empty_input"
		reasonFailed     = "failed"
	)

	// 生产常量必须与上面的对外契约一致——否则指标吐出的 label 会与告警规则脱节。
	for _, pair := range []struct{ got, want string }{
		{enrichmentContentEmbedding, metricEmbedding},
		{enrichmentConceptAttach, metricConcept},
		{enrichmentNotWired, reasonNotWired},
		{enrichmentDisabled, reasonDisabled},
		{enrichmentEmptyInput, reasonEmptyInput},
		{enrichmentFailed, reasonFailed},
	} {
		if pair.got != pair.want {
			t.Fatalf("指标 label 常量 = %q, want %q——改名会让既有告警静默失效", pair.got, pair.want)
		}
	}

	t.Run("embedder 未装配记 not_wired", func(t *testing.T) {
		t.Parallel()
		metrics := observability.NewMetrics()
		f := collectionFinalizer{metrics: metrics}
		f.writeContentEmbedding(context.Background(), uuid.New(), uuid.New(), "标题", "摘要", "正文")

		assertEnrichmentCount(t, metrics, metricEmbedding, reasonNotWired, 1)
		assertEnrichmentCount(t, metrics, metricEmbedding, reasonFailed, 0)
	})

	t.Run("embedder 关闭记 disabled", func(t *testing.T) {
		t.Parallel()
		metrics := observability.NewMetrics()
		f := collectionFinalizer{
			metrics:             metrics,
			contentEmbedder:     &recordingEmbedder{enabled: false},
			linkEmbeddingWriter: &recordingLinkEmbeddingWriter{},
		}
		f.writeContentEmbedding(context.Background(), uuid.New(), uuid.New(), "标题", "摘要", "正文")

		assertEnrichmentCount(t, metrics, metricEmbedding, reasonDisabled, 1)
	})

	t.Run("embedding 调用失败记 failed", func(t *testing.T) {
		t.Parallel()
		metrics := observability.NewMetrics()
		f := collectionFinalizer{
			metrics:             metrics,
			contentEmbedder:     &recordingEmbedder{enabled: true, err: errors.New("embedding backend down")},
			linkEmbeddingWriter: &recordingLinkEmbeddingWriter{},
		}
		f.writeContentEmbedding(context.Background(), uuid.New(), uuid.New(), "标题", "摘要", "正文")

		assertEnrichmentCount(t, metrics, metricEmbedding, reasonFailed, 1)
	})

	t.Run("embedding 成功不记账", func(t *testing.T) {
		t.Parallel()
		metrics := observability.NewMetrics()
		f := collectionFinalizer{
			metrics:             metrics,
			contentEmbedder:     &recordingEmbedder{enabled: true, vec: []float32{0.5, 0.5}},
			linkEmbeddingWriter: &recordingLinkEmbeddingWriter{},
		}
		f.writeContentEmbedding(context.Background(), uuid.New(), uuid.New(), "标题", "摘要", "正文")

		for _, reason := range []string{reasonNotWired, reasonDisabled, reasonEmptyInput, reasonFailed} {
			assertEnrichmentCount(t, metrics, metricEmbedding, reason, 0)
		}
	})

	t.Run("concept attacher 未装配记 not_wired", func(t *testing.T) {
		t.Parallel()
		metrics := observability.NewMetrics()
		f := collectionFinalizer{metrics: metrics}
		f.attachConcepts(context.Background(), uuid.New(), 1, []string{"Go"})

		assertEnrichmentCount(t, metrics, metricConcept, reasonNotWired, 1)
	})

	t.Run("无标签记 empty_input", func(t *testing.T) {
		t.Parallel()
		metrics := observability.NewMetrics()
		f := collectionFinalizer{metrics: metrics, conceptAttacher: &recordingConceptAttacher{}}
		f.attachConcepts(context.Background(), uuid.New(), 1, nil)

		assertEnrichmentCount(t, metrics, metricConcept, reasonEmptyInput, 1)
	})

	t.Run("concept attach 失败记 failed", func(t *testing.T) {
		t.Parallel()
		metrics := observability.NewMetrics()
		f := collectionFinalizer{
			metrics:         metrics,
			conceptAttacher: &recordingConceptAttacher{err: errors.New("concept store down")},
		}
		f.attachConcepts(context.Background(), uuid.New(), 1, []string{"Go"})

		assertEnrichmentCount(t, metrics, metricConcept, reasonFailed, 1)
	})
}

func assertEnrichmentCount(t *testing.T, metrics *observability.Metrics, kind, reason string, want float64) {
	t.Helper()
	got := testutil.ToFloat64(metrics.ParseEnrichmentSkippedTotal.WithLabelValues(kind, reason))
	if got != want {
		t.Errorf("enrichment_skipped_total{kind=%q,reason=%q} = %v, want %v", kind, reason, got, want)
	}
}
