package concept

// Benchmark 覆盖 concept Resolver 的三条主要热路径：
//   1. 缓存命中——纯内存查 sync.Map，对应在线服务里 RAG/LLM 之类高频 surface tag
//      被同进程反复解析的最常态。
//   2. 缓存未命中 + 本地 fallback——既不在 cache 也不在 store，未配置 embedder，
//      落到 createLocalConcept，对应"新出现的项目名"冷路径。
//   3. ResolveAndAttachBatch——10 / 100 个 surface tag 的批量 attach，验证最近
//      一轮 perf 改造（4N → 4 round-trip）的实际收益。
//
// 所有 benchmark 不依赖外部网络、不依赖 Postgres，全部用 in-memory stubStore，
// 与 resolver_test.go 的 fixture 复用同一套结构。

import (
	"context"
	"fmt"
	"testing"

	"github.com/google/uuid"
)

// BenchmarkResolverResolve_CacheHit 衡量"alias 已缓存"的纯内存查找路径。
// 这是最被频繁触发的路径——只要进程里见过同一个 surface tag，每次解析就走
// 这条 fast path，不发 SELECT、不调 Wikidata。
func BenchmarkResolverResolve_CacheHit(b *testing.B) {
	store := newStubStore()
	conceptID := uuid.New()
	// 预热：让 alias 同时存在于 store 和 cache。第一次 Resolve 把 cache 填好。
	store.aliasByName["rag"] = conceptID
	cache := NewAliasCache()
	r := NewResolver(ResolverOptions{Store: store, Cache: cache})

	ctx := context.Background()
	if _, err := r.Resolve(ctx, "RAG"); err != nil {
		b.Fatalf("warm-up Resolve: %v", err)
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		id, err := r.Resolve(ctx, "RAG")
		if err != nil {
			b.Fatalf("Resolve: %v", err)
		}
		if id != conceptID {
			b.Fatalf("id = %v, want %v", id, conceptID)
		}
	}
}

// BenchmarkResolverResolve_CacheMiss_LocalOnly 衡量"既不在 cache 也不在 store、
// 未配置 embedder"时的冷路径。每次跑都会创建一个新的本地 concept——这是 perf
// 改动里最容易回退的路径，因为它走 CreateConcept + UpsertAlias 两次 store 写入。
//
// 用 ctr 在每次迭代里造唯一 surface tag，避免缓存/store dedupe 把后续 N-1 次跑
// 退化成 cache hit。
func BenchmarkResolverResolve_CacheMiss_LocalOnly(b *testing.B) {
	store := newStubStore()
	// 不配置 embedder：Resolve 跳过向量闸直接落到 createLocalConcept。
	cache := NewAliasCache()
	r := NewResolver(ResolverOptions{Store: store, Cache: cache})

	ctx := context.Background()

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		// 每次都用不同 surface tag 强制走 createLocalConcept 分支。
		// 如果用同一个 tag，第二次开始就 cache 命中了，benchmark 失真。
		tag := fmt.Sprintf("unseen-%d", i)
		if _, err := r.Resolve(ctx, tag); err != nil {
			b.Fatalf("Resolve: %v", err)
		}
	}
}

// BenchmarkResolverResolveAndAttachBatch 衡量批量解析 + attach 的整体吞吐。
// 用 sub-bench 跑两个规模：10 个 tag（典型一条 link 的 tag 数）和 100 个 tag
// （极端 / 批量重处理场景），观察 batch 路径相对于 N 的线性度。
//
// 走 cache 全命中 + 单次 AttachLinkConceptsBatch + IncrementUseCounts +
// RecalculateDisplayNames 的最佳分支——这正是 perf 改造的目标路径。
func BenchmarkResolverResolveAndAttachBatch(b *testing.B) {
	cases := []struct {
		name string
		n    int
	}{
		{"N=10", 10},
		{"N=100", 100},
	}

	for _, c := range cases {
		c := c
		b.Run(c.name, func(b *testing.B) {
			store := newStubStore()
			cache := NewAliasCache()
			// 预生成 N 个 surface tag 和对应 concept_id，全部塞进 store 的
			// aliasByName，让批量解析全部走 alias hit 分支。
			tags := make([]string, c.n)
			for i := 0; i < c.n; i++ {
				tag := fmt.Sprintf("tag-%03d", i)
				id := uuid.New()
				tags[i] = tag
				store.aliasByName[tag] = id
			}

			r := NewResolver(ResolverOptions{Store: store, Cache: cache})
			ctx := context.Background()

			// 预热 cache：第一轮 batch 会把所有 alias 拉进 cache，后续 b.N 次
			// 全部走 cache hit。
			if _, err := r.ResolveAndAttachBatch(ctx, uuid.New(), 1, tags); err != nil {
				b.Fatalf("warm-up batch: %v", err)
			}

			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				linkID := uuid.New()
				if _, err := r.ResolveAndAttachBatch(ctx, linkID, 1, tags); err != nil {
					b.Fatalf("ResolveAndAttachBatch: %v", err)
				}
			}
		})
	}
}
