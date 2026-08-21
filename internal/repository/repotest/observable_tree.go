package repotest

import (
	"context"
	"sync"

	"webtag/internal/model"
	"webtag/internal/repository"
)

// TreeStoreCall mirrors LinkStoreCall for the TreeStore
// surface. Tree tests inspect Calls() / CountCalls() the same way the
// link and job tests do.
type TreeStoreCall struct {
	Method string
	Args   []any
}

// ObservableTreeStore mirrors ObservableLinkStore
// for the TreeStore interface. Same shape: per-method typed call
// slices, optional behavior hooks, a generic call log, mu-protected
// for concurrent test scenarios. Read-side defaults pull from the
// configured Lookups map (LookupByURLs) / Visible slice (ListVisible)
// when no hook is set.
type ObservableTreeStore struct {
	BaseTreeStore

	mu sync.Mutex

	calls []TreeStoreCall

	// Lookups is consulted by LookupByURLs when no LookupByURLsFunc
	// hook is set: only URLs present in the map appear in the
	// returned result map (mirrors PGXTreeRepository.LookupByURLs).
	Lookups map[string]*model.Link
	// Visible is the rows ListVisible returns when ListVisibleFunc
	// is nil. Matched by domain when the filter pointer is non-nil.
	Visible []model.Link

	LookupByURLsCalls [][]string
	ListVisibleCalls  []*string

	LookupByURLsFunc func(context.Context, []string) (map[string]*model.Link, error)
	ListVisibleFunc  func(context.Context, *string) ([]model.Link, error)
}

// Calls 返回到目前为止观察到的全部方法调用快照（拷贝后释放锁，调用方可安全遍历）。
func (o *ObservableTreeStore) Calls() []TreeStoreCall {
	o.mu.Lock()
	defer o.mu.Unlock()
	out := make([]TreeStoreCall, len(o.calls))
	copy(out, o.calls)
	return out
}

// CountCalls 返回指定方法名被调用过的次数。
func (o *ObservableTreeStore) CountCalls(method string) int {
	o.mu.Lock()
	defer o.mu.Unlock()
	count := 0
	for _, c := range o.calls {
		if c.Method == method {
			count++
		}
	}
	return count
}

func (o *ObservableTreeStore) record(method string, args ...any) {
	o.calls = append(o.calls, TreeStoreCall{Method: method, Args: args})
}

// LookupByURLs 记录调用；优先调用 LookupByURLsFunc，否则按 Lookups 表逐项命中（未命中的 URL 不出现在结果中）。
func (o *ObservableTreeStore) LookupByURLs(ctx context.Context, urls []string) (map[string]*model.Link, error) {
	o.mu.Lock()
	o.record("LookupByURLs", urls)
	o.LookupByURLsCalls = append(o.LookupByURLsCalls, append([]string(nil), urls...))
	hook := o.LookupByURLsFunc
	lookups := o.Lookups
	o.mu.Unlock()
	if hook != nil {
		return hook(ctx, urls)
	}
	out := make(map[string]*model.Link, len(urls))
	if lookups == nil {
		return out, nil
	}
	for _, u := range urls {
		if link, ok := lookups[u]; ok && link != nil {
			out[u] = link
		}
	}
	return out, nil
}

// ListVisible 记录调用；优先调用 ListVisibleFunc，否则返回预设的 Visible 切片拷贝。
func (o *ObservableTreeStore) ListVisible(ctx context.Context, domain *string) ([]model.Link, error) {
	o.mu.Lock()
	o.record("ListVisible", domain)
	var captured *string
	if domain != nil {
		copied := *domain
		captured = &copied
	}
	o.ListVisibleCalls = append(o.ListVisibleCalls, captured)
	hook := o.ListVisibleFunc
	rows := append([]model.Link(nil), o.Visible...)
	o.mu.Unlock()
	if hook != nil {
		return hook(ctx, domain)
	}
	return rows, nil
}

var _ repository.TreeStore = (*ObservableTreeStore)(nil)
