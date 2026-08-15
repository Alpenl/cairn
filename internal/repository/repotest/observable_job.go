package repotest

import (
	"context"
	"sync"

	"github.com/google/uuid"

	"webtag/internal/model"
	"webtag/internal/repository"
)

// JobStoreCall mirrors LinkStoreCall for the JobStore surface.
type JobStoreCall struct {
	Method string
	Args   []any
}

// ObservableJobStore is the JobStore counterpart to ObservableLinkStore.
// Same shape: per-method typed call slices, optional behavior hooks, a
// generic call log, mu-protected for concurrent test scenarios.
type ObservableJobStore struct {
	BaseJobStore

	mu sync.Mutex

	calls []JobStoreCall

	ByID           map[uuid.UUID]*model.ParseJob
	LatestByLinkID map[uuid.UUID]*model.ParseJob

	CreateCalls          []uuid.UUID
	GetByIDCalls         []uuid.UUID
	GetLatestByLinkCalls []uuid.UUID
	UpdateStateCalls     []repository.UpdateJobStateParams

	// CreateResult, like ObservableLinkStore.CreateResult, is the
	// "always return this on Create" shortcut for tests that don't need
	// custom CreateFunc behavior.
	CreateResult *model.ParseJob

	CreateFunc            func(context.Context, uuid.UUID) (*model.ParseJob, error)
	GetByIDFunc           func(context.Context, uuid.UUID) (*model.ParseJob, error)
	GetLatestByLinkIDFunc func(context.Context, uuid.UUID) (*model.ParseJob, error)
	UpdateStateFunc       func(context.Context, repository.UpdateJobStateParams) error
}

// Calls 返回到目前为止观察到的全部方法调用快照（拷贝后释放锁，调用方可安全遍历）。
func (o *ObservableJobStore) Calls() []JobStoreCall {
	o.mu.Lock()
	defer o.mu.Unlock()
	out := make([]JobStoreCall, len(o.calls))
	copy(out, o.calls)
	return out
}

// CountCalls 返回指定方法名被调用过的次数。
func (o *ObservableJobStore) CountCalls(method string) int {
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

func (o *ObservableJobStore) record(method string, args ...any) {
	o.calls = append(o.calls, JobStoreCall{Method: method, Args: args})
}

// Create 记录调用并依次回退到 CreateFunc / CreateResult / BaseJobStore（panic）。
func (o *ObservableJobStore) Create(ctx context.Context, linkID uuid.UUID) (*model.ParseJob, error) {
	o.mu.Lock()
	o.record("Create", linkID)
	o.CreateCalls = append(o.CreateCalls, linkID)
	hook := o.CreateFunc
	result := o.CreateResult
	o.mu.Unlock()
	if hook != nil {
		return hook(ctx, linkID)
	}
	if result != nil {
		return result, nil
	}
	return o.BaseJobStore.Create(ctx, linkID)
}

// GetByID 记录调用；优先调用 GetByIDFunc，否则查 ByID 表，未命中返回 (nil, nil)。
func (o *ObservableJobStore) GetByID(ctx context.Context, id uuid.UUID) (*model.ParseJob, error) {
	o.mu.Lock()
	o.record("GetByID", id)
	o.GetByIDCalls = append(o.GetByIDCalls, id)
	hook := o.GetByIDFunc
	byID := o.ByID
	o.mu.Unlock()
	if hook != nil {
		return hook(ctx, id)
	}
	if byID != nil {
		return byID[id], nil
	}
	return nil, nil
}

// GetLatestByLinkID 记录调用；优先调用 GetLatestByLinkIDFunc，否则查 LatestByLinkID 表。
func (o *ObservableJobStore) GetLatestByLinkID(ctx context.Context, linkID uuid.UUID) (*model.ParseJob, error) {
	o.mu.Lock()
	o.record("GetLatestByLinkID", linkID)
	o.GetLatestByLinkCalls = append(o.GetLatestByLinkCalls, linkID)
	hook := o.GetLatestByLinkIDFunc
	latest := o.LatestByLinkID
	o.mu.Unlock()
	if hook != nil {
		return hook(ctx, linkID)
	}
	if latest != nil {
		return latest[linkID], nil
	}
	return nil, nil
}

// UpdateState 记录调用并回退到 UpdateStateFunc，未设置时 panic（防止意外写入悄然成功）。
func (o *ObservableJobStore) UpdateState(ctx context.Context, params repository.UpdateJobStateParams) error {
	o.mu.Lock()
	o.record("UpdateState", params)
	o.UpdateStateCalls = append(o.UpdateStateCalls, params)
	hook := o.UpdateStateFunc
	o.mu.Unlock()
	if hook != nil {
		return hook(ctx, params)
	}
	// Symmetry with ObservableLinkStore: no hook -> panic via Base*Store
	// so unintended writes fail loudly instead of silently succeeding.
	return o.BaseJobStore.UpdateState(ctx, params)
}

var _ repository.JobStore = (*ObservableJobStore)(nil)
