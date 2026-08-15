package service

import (
	"context"

	"webtag/internal/representation"
)

// ReadRevisionStore reads one authoritative component-aware version base.
type ReadRevisionStore interface {
	Current(context.Context, representation.ComponentSet) (representation.VersionBase, error)
}

// ReadRevisionService 只用 singleflight 合并同时进行的安装版本读取，不跨批次
// 复用结果。条件请求必须观察当次权威版本，否则写入后的旧版本可能错误命中 304。
type ReadRevisionService struct {
	store ReadRevisionStore
	cache *SnapshotCache[representation.VersionBase]
}

func NewReadRevisionService(store ReadRevisionStore) *ReadRevisionService {
	return &ReadRevisionService{
		store: store,
		// 零 TTL 禁止已完成结果复用，同时保留按组件集分键的 singleflight。
		cache: NewSnapshotCache(0, nil, cloneVersionBase),
	}
}

// Current returns the selected representation base. Conditional callers fail
// open on errors; identity callers fail closed before serving private data.
func (s *ReadRevisionService) Current(ctx context.Context, components representation.ComponentSet) (representation.VersionBase, error) {
	if s == nil || s.store == nil {
		return representation.VersionBase{}, nil
	}
	return s.cache.Get(ctx, components.Key(), func(loadCtx context.Context) (representation.VersionBase, error) {
		return s.store.Current(loadCtx, components)
	})
}

func cloneVersionBase(base representation.VersionBase) representation.VersionBase {
	base.Components = append([]representation.Component(nil), base.Components...)
	return base
}
