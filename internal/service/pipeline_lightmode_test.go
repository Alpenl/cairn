package service

import (
	"context"
	"testing"

	"webtag/internal/fetcher"
	"webtag/internal/model"
)

// dualFetcher tracks which path (Fetch or FetchLight) was invoked so
// tests can assert pipeline depth resolution without standing up a
// real ParsePipeline run. It implements both ContentFetcher and
// LightContentFetcher.
type dualFetcher struct {
	deepCalls  int
	lightCalls int
}

func (d *dualFetcher) Fetch(_ context.Context, url string) (fetcher.Content, error) {
	d.deepCalls++
	return fetcher.Content{URL: url, Body: "deep body", FetcherType: "deep"}, nil
}

func (d *dualFetcher) FetchLight(_ context.Context, url string) (fetcher.Content, error) {
	d.lightCalls++
	return fetcher.Content{URL: url, Body: "light body", FetcherType: "light"}, nil
}

// deepOnlyFetcher implements only ContentFetcher — exercises the
// "PreferLight requested but fetcher doesn't support it" fallback.
type deepOnlyFetcher struct {
	deepCalls int
}

func (d *deepOnlyFetcher) Fetch(_ context.Context, url string) (fetcher.Content, error) {
	d.deepCalls++
	return fetcher.Content{URL: url, Body: "deep only", FetcherType: "deep"}, nil
}

// pipelineWithFetcher 直接构造结构体而非走 NewParsePipeline：这些用例只驱动
// acquireContent，与仓储、终态 completer 无关。走构造函数会被迫装配一堆本用例
// 不关心的依赖，反而模糊了测试意图。
func pipelineWithFetcher(f ContentFetcher, preferLight bool) *ParsePipeline {
	return &ParsePipeline{
		fetcher:     f,
		preferLight: preferLight,
	}
}

func TestAcquireContentPreferLightTrueUsesLightFetch(t *testing.T) {
	f := &dualFetcher{}
	p := pipelineWithFetcher(f, true)
	link := &model.Link{URL: "https://example.com/"}

	content, tookLight, err := p.acquireContent(context.Background(), parseInputForTest(link))
	if err != nil {
		t.Fatalf("acquireContent: %v", err)
	}
	if content.FetcherType != "light" {
		t.Errorf("FetcherType = %q, want light", content.FetcherType)
	}
	if !tookLight {
		t.Errorf("tookLight = false, want true on the light path")
	}
	if f.lightCalls != 1 || f.deepCalls != 0 {
		t.Errorf("lightCalls=%d deepCalls=%d, want 1/0", f.lightCalls, f.deepCalls)
	}
}

func TestAcquireContentPreferLightFalseUsesDeepFetch(t *testing.T) {
	f := &dualFetcher{}
	p := pipelineWithFetcher(f, false)
	link := &model.Link{URL: "https://example.com/"}

	content, tookLight, _ := p.acquireContent(context.Background(), parseInputForTest(link))
	if content.FetcherType != "deep" {
		t.Errorf("FetcherType = %q, want deep", content.FetcherType)
	}
	if tookLight {
		t.Errorf("tookLight = true, want false on the deep path")
	}
	if f.deepCalls != 1 || f.lightCalls != 0 {
		t.Errorf("deepCalls=%d lightCalls=%d, want 1/0", f.deepCalls, f.lightCalls)
	}
}

func TestAcquireContentLightUnsupportedFallsBackToDeep(t *testing.T) {
	f := &deepOnlyFetcher{}
	p := pipelineWithFetcher(f, true) // requested light but fetcher is deep-only
	link := &model.Link{URL: "https://example.com/"}

	content, tookLight, _ := p.acquireContent(context.Background(), parseInputForTest(link))
	if content.FetcherType != "deep" {
		t.Errorf("FetcherType = %q, want deep (fallback)", content.FetcherType)
	}
	if tookLight {
		t.Errorf("tookLight = true, want false when light is unsupported and we fell back to deep")
	}
	if f.deepCalls != 1 {
		t.Errorf("deepCalls = %d, want 1", f.deepCalls)
	}
}

func TestAcquireContentPerLinkDeepOverridesPreferLight(t *testing.T) {
	f := &dualFetcher{}
	p := pipelineWithFetcher(f, true) // global says light
	link := &model.Link{
		URL: "https://example.com/",
		SourceMetadata: map[string]any{
			"parse_depth": "deep", // per-link override forces deep
		},
	}

	content, _, _ := p.acquireContent(context.Background(), parseInputForTest(link))
	if content.FetcherType != "deep" {
		t.Errorf("FetcherType = %q, want deep (per-link override)", content.FetcherType)
	}
	if f.deepCalls != 1 || f.lightCalls != 0 {
		t.Errorf("deepCalls=%d lightCalls=%d, want 1/0", f.deepCalls, f.lightCalls)
	}
}

func TestAcquireContentPerLinkLightOverridesGlobalDeep(t *testing.T) {
	f := &dualFetcher{}
	p := pipelineWithFetcher(f, false) // global says deep
	link := &model.Link{
		URL: "https://example.com/",
		SourceMetadata: map[string]any{
			"parse_depth": "light",
		},
	}

	content, _, _ := p.acquireContent(context.Background(), parseInputForTest(link))
	if content.FetcherType != "light" {
		t.Errorf("FetcherType = %q, want light (per-link override)", content.FetcherType)
	}
}

func TestAcquireContentPerLinkUnknownValueFallsThroughToGlobal(t *testing.T) {
	f := &dualFetcher{}
	p := pipelineWithFetcher(f, true) // global says light

	link := &model.Link{
		URL: "https://example.com/",
		SourceMetadata: map[string]any{
			"parse_depth": "potato", // garbage, ignored
		},
	}
	content, _, _ := p.acquireContent(context.Background(), parseInputForTest(link))
	if content.FetcherType != "light" {
		t.Errorf("garbage parse_depth should fall through to PreferLight; got %q", content.FetcherType)
	}
}

func TestAcquireContentPerLinkCaseInsensitive(t *testing.T) {
	f := &dualFetcher{}
	p := pipelineWithFetcher(f, false)

	link := &model.Link{
		URL: "https://example.com/",
		SourceMetadata: map[string]any{
			"parse_depth": "  LIGHT  ", // mixed case + whitespace
		},
	}
	content, _, _ := p.acquireContent(context.Background(), parseInputForTest(link))
	if content.FetcherType != "light" {
		t.Errorf("LIGHT (whitespace + uppercase) should map to light; got %q", content.FetcherType)
	}
}

// TestAcquireContentNonStringParseDepthIgnored guards against a future
// caller passing parse_depth as a bool / int / nil — we should not
// panic on type assertion failure, just fall through to the global.
func TestAcquireContentNonStringParseDepthIgnored(t *testing.T) {
	f := &dualFetcher{}
	p := pipelineWithFetcher(f, false)

	for _, badVal := range []any{true, 1, nil, []string{"light"}} {
		link := &model.Link{
			URL:            "https://example.com/",
			SourceMetadata: map[string]any{"parse_depth": badVal},
		}
		content, _, err := p.acquireContent(context.Background(), parseInputForTest(link))
		if err != nil {
			t.Errorf("parse_depth=%v (%T) caused error: %v", badVal, badVal, err)
		}
		if content.FetcherType != "deep" {
			t.Errorf("parse_depth=%v (%T) should fall through to PreferLight=false (deep); got %q",
				badVal, badVal, content.FetcherType)
		}
	}
}
