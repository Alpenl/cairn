package service

import (
	"context"
	"time"
)

// TagCount 表示一个标签及其使用次数，是 service 层与缓存之间的统一形状。
//
// ReadingCount / SiteCount 只在带 library_kind scope 的聚合里有意义（阅读库
// 与网站库各自的计数）；无 scope 的全局聚合两者恒为 0，投影成 DTO 时也不带。
type TagCount struct {
	Tag          string
	Count        int
	ReadingCount int
	SiteCount    int
}

// TagLoader 是缓存未命中时用来从底层加载标签计数的回调签名。
type TagLoader func(context.Context) ([]TagCount, error)

// tagCacheVariantGlobal 是无 scope 标签聚合（/api/tags 不带 library_kind）
// 在缓存里的切面名。带 scope 的路径用 scope 字符串本身作为切面，共用同一个
// SnapshotCache 实例，因此一次写入会失效全部切面。
const tagCacheVariantGlobal = ""

// TagCache 是安装级标签聚合 TTL 缓存，也是 SnapshotCache 的薄封装。
// 返回的切片必须视为只读：同一份快照分发给所有并发读者。
type TagCache struct {
	inner *SnapshotCache[[]TagCount]
}

// NewTagCache 构造一个按 TTL 过期的标签计数缓存。now 用于注入
// 时钟，传 nil 默认走 time.Now，单元测试可借此精确驱动过期边界。
func NewTagCache(ttl time.Duration, now func() time.Time) *TagCache {
	return &TagCache{inner: NewSnapshotCache(ttl, now, cloneTagCounts)}
}

// Get 返回当前安装的标签计数快照；未命中时由 singleflight 合并回源，
// loader 失败而存在历史快照时按 stale-on-error 返回旧值 + 错误。
func (c *TagCache) Get(ctx context.Context, loader TagLoader) ([]TagCount, error) {
	return c.GetScoped(ctx, tagCacheVariantGlobal, loader)
}

// GetScoped 与 Get 相同，但按 scope 切面分别缓存（/api/tags?library_kind=）。
// 这条路径改造前完全绕过缓存直落 DB，而其中的 all 变体是两个聚合的
// FULL OUTER JOIN。
func (c *TagCache) GetScoped(ctx context.Context, scope string, loader TagLoader) ([]TagCount, error) {
	values, err := c.inner.Get(ctx, scope, SnapshotLoader[[]TagCount](loader))
	if err != nil {
		return values, err
	}
	if values == nil {
		// 成功但空结果统一暴露为非 nil 空切片，调用方不必区分 nil 与空。
		return []TagCount{}, nil
	}
	return values, nil
}

// Invalidate 让安装级标签缓存立即失效，下一次 Get 必然回源。
// Pipeline 写入新链接或 Delete 后调用，避免标签聚合在 TTL 内停留旧数据。
func (c *TagCache) Invalidate(ctx context.Context) {
	c.inner.Invalidate(ctx)
}

func cloneTagCounts(values []TagCount) []TagCount {
	if values == nil {
		return nil
	}

	// cap == len：调用方即便违反只读契约去 append，也只能触发一次新分配，
	// 而不会静默改写快照里 len 之后的位置。
	cloned := make([]TagCount, len(values))
	copy(cloned, values)
	return cloned
}
