// Package repotest provides Base*Store types whose every method panics with
// "not implemented in fake". Tests embed the appropriate Base* into their
// own fake struct and override only the methods their scenario actually
// touches; missing overrides crash loudly instead of silently returning the
// zero value.
//
// This eliminates the boilerplate of repeating ten empty stubs per fake when
// a test only cares about two of them — see git history before this package
// existed for a sense of the pre-cleanup pattern.
//
// The package is consumed only from _test.go files; it imports the
// production repository interface package solely to ensure compile-time
// satisfaction via `var _ repository.LinkStore = (*BaseLinkStore)(nil)`.
package repotest

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	"webtag/internal/model"
	"webtag/internal/repository"
)

// notImplemented panics with a method-tagged message so a test failure
// pinpoints the missing override without a stack-trace dive.
func notImplemented(name string) error {
	panic(fmt.Sprintf("repotest: %s not implemented in this fake; embed a Base store and override the method", name))
}

// BaseLinkStore satisfies repository.LinkStore by panicking on every call.
type BaseLinkStore struct{}

func (BaseLinkStore) GetDetailByID(context.Context, uuid.UUID) (*repository.LinkDetailProjection, error) {
	return nil, notImplemented("LinkStore.GetDetailByID")
}

func (BaseLinkStore) GetDetailByURL(context.Context, string) (*repository.LinkDetailProjection, error) {
	return nil, notImplemented("LinkStore.GetDetailByURL")
}

func (BaseLinkStore) GetParseInputByID(context.Context, uuid.UUID) (*repository.LinkParseInput, error) {
	return nil, notImplemented("LinkStore.GetParseInputByID")
}

func (BaseLinkStore) GetParseInputBySourceKeyOrURL(context.Context, string, string) (*repository.LinkParseInput, error) {
	return nil, notImplemented("LinkStore.GetParseInputBySourceKeyOrURL")
}

func (BaseLinkStore) GetLifecycleByID(context.Context, uuid.UUID) (*repository.LinkLifecycleProjection, error) {
	return nil, notImplemented("LinkStore.GetLifecycleByID")
}

func (BaseLinkStore) GetSubmitLookupByID(context.Context, uuid.UUID) (*repository.LinkSubmitLookup, error) {
	return nil, notImplemented("LinkStore.GetSubmitLookupByID")
}

func (BaseLinkStore) GetSubmitLookupByURL(context.Context, string) (*repository.LinkSubmitLookup, error) {
	return nil, notImplemented("LinkStore.GetSubmitLookupByURL")
}

// Create 默认 panic；测试需自行嵌入并覆盖。
func (BaseLinkStore) Create(context.Context, repository.CreateLinkParams) (*model.Link, error) {
	return nil, notImplemented("LinkStore.Create")
}

// GetByID 默认 panic；测试需自行嵌入并覆盖。
func (BaseLinkStore) GetByID(context.Context, uuid.UUID) (*model.Link, error) {
	return nil, notImplemented("LinkStore.GetByID")
}

// GetByURL 默认 panic；测试需自行嵌入并覆盖。
func (BaseLinkStore) GetByURL(context.Context, string) (*model.Link, error) {
	return nil, notImplemented("LinkStore.GetByURL")
}

// GetBySourceKey 默认 panic；测试需自行嵌入并覆盖。
func (BaseLinkStore) GetBySourceKey(context.Context, string) (*model.Link, error) {
	return nil, notImplemented("LinkStore.GetBySourceKey")
}

// GetBySourceKeyOrURL 默认 panic；测试需自行嵌入并覆盖。
func (BaseLinkStore) GetBySourceKeyOrURL(context.Context, string, string) (*model.Link, error) {
	return nil, notImplemented("LinkStore.GetBySourceKeyOrURL")
}

// ListDone 默认 panic；测试需自行嵌入并覆盖。
func (BaseLinkStore) ListDone(context.Context, repository.ListLinksFilter) ([]model.Link, int, error) {
	return nil, 0, notImplemented("LinkStore.ListDone")
}

// UpdateState 默认 panic；测试需自行嵌入并覆盖。
func (BaseLinkStore) UpdateState(context.Context, repository.UpdateLinkStateParams) error {
	return notImplemented("LinkStore.UpdateState")
}

// UpdateAnalysis 默认 panic；测试需自行嵌入并覆盖。
func (BaseLinkStore) UpdateAnalysis(context.Context, repository.UpdateLinkAnalysisParams) error {
	return notImplemented("LinkStore.UpdateAnalysis")
}

func (BaseLinkStore) MarkParseProcessing(context.Context, uuid.UUID, uuid.UUID) error {
	return notImplemented("ParseStateStore.MarkParseProcessing")
}

func (BaseLinkStore) MarkParseFailed(context.Context, uuid.UUID, uuid.UUID, string) error {
	return notImplemented("ParseStateStore.MarkParseFailed")
}

func (BaseLinkStore) CompleteParse(context.Context, repository.UpdateLinkAnalysisParams, uuid.UUID) error {
	return notImplemented("ParseStateStore.CompleteParse")
}

// Delete 默认 panic；测试需自行嵌入并覆盖。
func (BaseLinkStore) Delete(context.Context, uuid.UUID) error {
	return notImplemented("LinkStore.Delete")
}

// BaseJobStore satisfies repository.JobStore by panicking on every call.
type BaseJobStore struct{}

// Create 默认 panic；测试需自行嵌入并覆盖。
func (BaseJobStore) Create(context.Context, uuid.UUID) (*model.ParseJob, error) {
	return nil, notImplemented("JobStore.Create")
}

// GetByID 默认 panic；测试需自行嵌入并覆盖。
func (BaseJobStore) GetByID(context.Context, uuid.UUID) (*model.ParseJob, error) {
	return nil, notImplemented("JobStore.GetByID")
}

// ListByIDs 默认 panic；测试需自行嵌入并覆盖。
func (BaseJobStore) ListByIDs(context.Context, []uuid.UUID) ([]model.ParseJob, error) {
	return nil, notImplemented("JobStore.ListByIDs")
}

// GetLatestByLinkID 默认 panic；测试需自行嵌入并覆盖。
func (BaseJobStore) GetLatestByLinkID(context.Context, uuid.UUID) (*model.ParseJob, error) {
	return nil, notImplemented("JobStore.GetLatestByLinkID")
}

// UpdateState 默认 panic；测试需自行嵌入并覆盖。
func (BaseJobStore) UpdateState(context.Context, repository.UpdateJobStateParams) error {
	return notImplemented("JobStore.UpdateState")
}

// BaseTreeStore satisfies repository.TreeStore by panicking on every call.
type BaseTreeStore struct{}

// LookupByURLs 默认 panic；测试需自行嵌入并覆盖。
func (BaseTreeStore) LookupByURLs(context.Context, []string) (map[string]*model.Link, error) {
	return nil, notImplemented("TreeStore.LookupByURLs")
}

// ListVisible 默认 panic；测试需自行嵌入并覆盖。
func (BaseTreeStore) ListVisible(context.Context, *string) ([]model.Link, error) {
	return nil, notImplemented("TreeStore.ListVisible")
}

// ListDomains 默认 panic；测试需自行嵌入并覆盖。
func (BaseTreeStore) ListDomains(context.Context) (repository.DomainTreeSummarySet, error) {
	return repository.DomainTreeSummarySet{}, notImplemented("TreeStore.ListDomains")
}

// ListDomainsScoped 默认 panic；测试需自行嵌入并覆盖。
func (BaseTreeStore) ListDomainsScoped(context.Context, model.LibraryKind) (repository.DomainTreeSummarySet, error) {
	return repository.DomainTreeSummarySet{}, notImplemented("TreeStore.ListDomainsScoped")
}

// Compile-time interface satisfaction so a future interface change here will
// fail the build instead of letting fakes silently drift.
var (
	_ repository.LinkStore = (*BaseLinkStore)(nil)
	_ repository.JobStore  = (*BaseJobStore)(nil)
	_ repository.TreeStore = (*BaseTreeStore)(nil)
)
