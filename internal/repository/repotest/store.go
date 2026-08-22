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
// The package is consumed only from _test.go files.
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

// BaseLinkStore provides panic-by-default implementations for link repository
// test doubles.
type BaseLinkStore struct{}

func (BaseLinkStore) GetDetailByID(context.Context, uuid.UUID) (*repository.LinkDetailProjection, error) {
	return nil, notImplemented("BaseLinkStore.GetDetailByID")
}

func (BaseLinkStore) GetDetailByURL(context.Context, string) (*repository.LinkDetailProjection, error) {
	return nil, notImplemented("BaseLinkStore.GetDetailByURL")
}

func (BaseLinkStore) GetParseInputByID(context.Context, uuid.UUID) (*repository.LinkParseInput, error) {
	return nil, notImplemented("BaseLinkStore.GetParseInputByID")
}

func (BaseLinkStore) GetParseInputBySourceKeyOrURL(context.Context, string, string) (*repository.LinkParseInput, error) {
	return nil, notImplemented("BaseLinkStore.GetParseInputBySourceKeyOrURL")
}

func (BaseLinkStore) GetLifecycleByID(context.Context, uuid.UUID) (*repository.LinkLifecycleProjection, error) {
	return nil, notImplemented("BaseLinkStore.GetLifecycleByID")
}

func (BaseLinkStore) GetSubmitLookupByID(context.Context, uuid.UUID) (*repository.LinkSubmitLookup, error) {
	return nil, notImplemented("BaseLinkStore.GetSubmitLookupByID")
}

func (BaseLinkStore) GetSubmitLookupByURL(context.Context, string) (*repository.LinkSubmitLookup, error) {
	return nil, notImplemented("BaseLinkStore.GetSubmitLookupByURL")
}

// Create 默认 panic；测试需自行嵌入并覆盖。
func (BaseLinkStore) Create(context.Context, repository.CreateLinkParams) (*model.Link, error) {
	return nil, notImplemented("BaseLinkStore.Create")
}

// GetByID 默认 panic；测试需自行嵌入并覆盖。
func (BaseLinkStore) GetByID(context.Context, uuid.UUID) (*model.Link, error) {
	return nil, notImplemented("BaseLinkStore.GetByID")
}

// GetByURL 默认 panic；测试需自行嵌入并覆盖。
func (BaseLinkStore) GetByURL(context.Context, string) (*model.Link, error) {
	return nil, notImplemented("BaseLinkStore.GetByURL")
}

// GetBySourceKey 默认 panic；测试需自行嵌入并覆盖。
func (BaseLinkStore) GetBySourceKey(context.Context, string) (*model.Link, error) {
	return nil, notImplemented("BaseLinkStore.GetBySourceKey")
}

// GetBySourceKeyOrURL 默认 panic；测试需自行嵌入并覆盖。
func (BaseLinkStore) GetBySourceKeyOrURL(context.Context, string, string) (*model.Link, error) {
	return nil, notImplemented("BaseLinkStore.GetBySourceKeyOrURL")
}

// ListDone 默认 panic；测试需自行嵌入并覆盖。
func (BaseLinkStore) ListDone(context.Context, repository.ListLinksFilter) ([]model.Link, int, error) {
	return nil, 0, notImplemented("BaseLinkStore.ListDone")
}

// UpdateState 默认 panic；测试需自行嵌入并覆盖。
func (BaseLinkStore) UpdateState(context.Context, repository.UpdateLinkStateParams) error {
	return notImplemented("BaseLinkStore.UpdateState")
}

// UpdateAnalysis 默认 panic；测试需自行嵌入并覆盖。
func (BaseLinkStore) UpdateAnalysis(context.Context, repository.UpdateLinkAnalysisParams) error {
	return notImplemented("BaseLinkStore.UpdateAnalysis")
}

func (BaseLinkStore) MarkParseProcessing(context.Context, model.ParseAttempt) error {
	return notImplemented("ParseStateStore.MarkParseProcessing")
}

func (BaseLinkStore) MarkParseFailed(context.Context, model.ParseAttempt, string) error {
	return notImplemented("ParseStateStore.MarkParseFailed")
}

// Delete 默认 panic；测试需自行嵌入并覆盖。
func (BaseLinkStore) Delete(context.Context, uuid.UUID) error {
	return notImplemented("BaseLinkStore.Delete")
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
	_ repository.TreeStore = (*BaseTreeStore)(nil)
)
