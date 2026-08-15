package app

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"webtag/internal/config"
	"webtag/internal/service"
)

// 这个文件补的是「PF7 装配层无人守卫」这个洞——与 PF1 补
// router_middleware_gzip_test.go 是同一类问题的第三次复发：
// internal/service 的缓存测试全部自己手工装配缓存实例，于是下面这些改动
// 可以在 Go 全量测试全绿的情况下发生：
//   · 删掉 deps_services.go 里的 .WithDomainCache(layer.domainCache)
//     —— 生产环境 /api/tree?view=domains 的缓存完全消失，PF7 的头号指标退回 10→10
//   · 把 TagInvalidator / TagCacheInvalidator 改回 layer.tagCache
//     —— 写入不再失效域名聚合
//   · 不给 routerOpts.AggregateInvalidator 赋值
//     —— 所有 HTTP 写路径都不再失效任何聚合
// 本文件断言的是**装配产物本身**，不是它调用的那些函数。

func testConfig() config.Config {
	return config.Config{
		TagCacheTTLMS:  300000,
		TreeCacheTTLMS: 300000,
		GzipEnabled:    true,
		GzipMinLength:  1024,
		AppEnv:         "dev",
	}
}

// countingInvalidatorProbe 用来确认扇出真的打到了两份缓存。
type countingInvalidatorProbe struct{ calls int }

func (p *countingInvalidatorProbe) Invalidate(context.Context) { p.calls++ }

// TestPersistenceLayerBuildsBothAggregateCaches 锁定 persistenceLayer 同时
// 持有标签缓存与域名缓存。域名缓存漏建时 aggregateCacheInvalidator 只会扇出
// 一个成员，本用例当场变红。
func TestPersistenceLayerBuildsBothAggregateCaches(t *testing.T) {
	t.Parallel()

	layer := &persistenceLayer{
		tagCache:    service.NewTagCache(durationMS(300000), nil),
		domainCache: service.NewDomainSummaryCache(durationMS(300000), nil),
	}

	invalidator := layer.aggregateCacheInvalidator()
	if len(invalidator) != 2 {
		t.Fatalf("聚合失效器成员数 = %d, want 2（标签 + 域名）", len(invalidator))
	}
	for i, member := range invalidator {
		if member == nil {
			t.Fatalf("聚合失效器第 %d 个成员为 nil", i)
		}
	}
}

// TestAggregateCacheInvalidatorSkipsNilDomainCache 锁定 TREE_CACHE_TTL_MS
// 使缓存禁用（NewDomainSummaryCache 返回 nil）时，扇出退化为只有标签缓存
// 且不 panic。
func TestAggregateCacheInvalidatorSkipsNilDomainCache(t *testing.T) {
	t.Parallel()

	layer := &persistenceLayer{
		tagCache:    service.NewTagCache(durationMS(300000), nil),
		domainCache: service.NewDomainSummaryCache(0, nil), // nil
	}

	invalidator := layer.aggregateCacheInvalidator()
	if len(invalidator) != 1 {
		t.Fatalf("域名缓存禁用时失效器成员数 = %d, want 1", len(invalidator))
	}
	invalidator.Invalidate(context.Background()) // 不得 panic
}

// TestRuntimeRouterOptionsCarryAggregateInvalidator 锁定生产装配确实把聚合
// 失效器接进了 RouterOptions。
//
// 断言的是 routerOptionsWithRuntimeDeps 的**返回值**，而赋值行就在那个函数
// 体内——删掉 `opts.AggregateInvalidator = layer.aggregateCacheInvalidator()`
// 这一行本用例即变红。不这样做的话，删掉它会让所有 HTTP 写路径（站点转换 /
// 合并 / 拆分 / 删除、资料与标签编辑、审核队列操作……）静默停止失效聚合缓存，
// 而没有任何 service 层测试会有反应。
func TestRuntimeRouterOptionsCarryAggregateInvalidator(t *testing.T) {
	t.Parallel()

	layer := &persistenceLayer{
		tagCache:    service.NewTagCache(durationMS(300000), nil),
		domainCache: service.NewDomainSummaryCache(durationMS(300000), nil),
	}

	opts := routerOptionsWithRuntimeDeps(testConfig(), layer, nil)

	if opts.AggregateInvalidator == nil {
		t.Fatal("RouterOptions.AggregateInvalidator 为 nil：HTTP 写路径将不再失效聚合缓存")
	}

	// 扇出必须真的打到两份缓存，而不是一个空的 MultiCacheInvalidator。
	fanout, ok := opts.AggregateInvalidator.(service.MultiCacheInvalidator)
	if !ok {
		t.Fatalf("AggregateInvalidator 类型 = %T, want service.MultiCacheInvalidator", opts.AggregateInvalidator)
	}
	if len(fanout) != 2 {
		t.Fatalf("扇出成员数 = %d, want 2（标签 + 域名）", len(fanout))
	}
}

// TestMultiCacheInvalidatorFanoutReachesMembers 独立锁定扇出语义本身。
func TestMultiCacheInvalidatorFanoutReachesMembers(t *testing.T) {
	t.Parallel()

	first, second := &countingInvalidatorProbe{}, &countingInvalidatorProbe{}
	service.MultiCacheInvalidator{first, nil, second}.Invalidate(context.Background())

	if first.calls != 1 || second.calls != 1 {
		t.Fatalf("扇出未触达全部成员：first=%d second=%d, want 1/1", first.calls, second.calls)
	}
}

// TestBuildTreeReadServiceAttachesDomainCache 锁定域名摘要缓存真的被接到了
// 树读服务上。
//
// 删掉 buildTreeReadService 里的 `.WithDomainCache(layer.domainCache)`，
// 生产环境 /api/tree?view=domains 的缓存整个消失、PF7 的头号指标退回 10→10，
// 而在补这条测试之前 Go 全量测试对此毫无反应。
func TestBuildTreeReadServiceAttachesDomainCache(t *testing.T) {
	t.Parallel()

	layer := &persistenceLayer{
		domainCache: service.NewDomainSummaryCache(durationMS(300000), nil),
	}
	if got := buildTreeReadService(layer).DomainCacheInvalidator(); got == nil {
		t.Fatal("树读服务上没有域名摘要缓存：/api/tree?view=domains 将退回每次全表聚合")
	}

	// TREE_CACHE_TTL_MS 使缓存禁用时应如实为 nil（退化为直查，不是静默缓存）。
	disabled := &persistenceLayer{domainCache: service.NewDomainSummaryCache(0, nil)}
	if got := buildTreeReadService(disabled).DomainCacheInvalidator(); got != nil {
		t.Fatal("缓存禁用时 DomainCacheInvalidator 应为 nil")
	}
}

// TestPublicAPIRoutesMountAggregateInvalidation 锁定失效中间件真的被挂进了
// 公开 API 鉴权组。
//
// 这是所有 HTTP 写路径（站点转换 / 合并 / 拆分 / 删除、资料与标签编辑、
// 审核队列操作……）唯一的失效点。registerPublicAPIRoutes 里那个 if 分支被
// 删掉的话，它们会全部静默停止失效聚合缓存。
func TestPublicAPIRoutesMountAggregateInvalidation(t *testing.T) {
	t.Parallel()

	invalidator := &countingInvalidatorProbe{}
	router := NewRouterWithDependencies(smokeDeps(), nil, nil, nil, nil, RouterOptions{
		AppEnv:                  "prod",
		ExtensionAPIToken:       "extension-secret",
		ConditionalGetRevisions: sessionSecurityVersions{},
		AggregateInvalidator:    invalidator,
	})

	post := func() *http.Request {
		req := httptest.NewRequest(http.MethodPost, "/api/links", strings.NewReader(`{"url":"https://example.com"}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer extension-secret")
		return req
	}

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, post())
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body=%q", rec.Code, rec.Body.String())
	}
	if invalidator.calls != 1 {
		t.Fatalf("成功写请求后聚合失效次数 = %d, want 1（中间件没有被挂进公开 API 组）", invalidator.calls)
	}

	// 读请求不该触发失效。
	before := invalidator.calls
	getRec := httptest.NewRecorder()
	getReq := httptest.NewRequest(http.MethodGet, "/api/links", nil)
	getReq.Header.Set("Authorization", "Bearer extension-secret")
	router.ServeHTTP(getRec, getReq)
	if invalidator.calls != before {
		t.Fatalf("GET 触发了失效：calls 从 %d 变成 %d", before, invalidator.calls)
	}

	// 鉴权失败的写请求同样不该触发。
	unauthorized := httptest.NewRecorder()
	unauthReq := httptest.NewRequest(http.MethodPost, "/api/links", strings.NewReader(`{"url":"https://example.com"}`))
	unauthReq.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(unauthorized, unauthReq)
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("未鉴权写请求 status = %d, want 401", unauthorized.Code)
	}
	if invalidator.calls != before {
		t.Fatalf("401 写请求触发了失效：calls 从 %d 变成 %d", before, invalidator.calls)
	}
}
