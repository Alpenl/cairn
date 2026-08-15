package repotest

import (
	"context"
	"fmt"
	"sync"

	"github.com/google/uuid"

	"webtag/internal/model"
	"webtag/internal/repository"
)

// LinkStoreCall is a single observed method invocation. Method names match
// the LinkStore interface; Args are the call's input arguments in order
// (excluding the context). Tests can ask "how many times was Method X
// called" through CountCalls, or pull the raw log via Calls() when they
// need ordering across methods.
type LinkStoreCall struct {
	Method string
	Args   []any
}

type ParseStateCall struct {
	LinkID       uuid.UUID
	JobID        uuid.UUID
	ErrorMessage string
}

type CompleteParseCall struct {
	Analysis repository.UpdateLinkAnalysisParams
	JobID    uuid.UUID
}

// CompleteReadingParseCall records one terminal reading write. Unlike
// CompleteParseCall it carries the classification params, because that is
// exactly what separates the reading transaction from the legacy one.
type CompleteReadingParseCall struct {
	Params repository.CompleteReadingParseParams
	JobID  uuid.UUID
}

// CompleteSiteParseCall records one terminal site write.
type CompleteSiteParseCall struct {
	Params repository.CompleteSiteParseParams
	JobID  uuid.UUID
}

// ObservableLinkStore is a fakeable LinkStore that records every call
// into a structured log and routes behavior through optional per-method
// hooks. It replaces ad-hoc spy fakes with a single shared primitive:
//
//   - Each method records the call into both a typed slice
//     (CreateCalls / GetByIDCalls / etc., kept on the struct for
//     ergonomic typed assertions) and the generic call log (Calls()).
//   - Each method then dispatches: if the matching *Func hook is set,
//     it is invoked; otherwise the embedded BaseLinkStore panics with a
//     "not implemented" message so tests fail loudly on an unintended
//     method call instead of returning a misleading zero value.
//     两个终态 completer（CompleteReadingParse / CompleteSiteParse）是刻意的
//     例外：解析必然终结于其一，强制每个测试重述 hook 只是噪音，故未设 hook
//     时默认成功。但它们会先复刻真实仓储的前置校验再返回成功——fake 可以比
//     生产省事，不可以比生产宽松。
//   - Maps (ByID / ByURL / BySourceKey) provide the common "configure a
//     lookup table once" shortcut for tests that do not need a custom
//     hook. They are consulted when the corresponding *Func is nil.
//
// The struct is mu-protected because Batch (and the parse pipeline)
// drive the store concurrently. Hook closures must be set before the
// fake is handed to the production code under test.
type ObservableLinkStore struct {
	BaseLinkStore

	mu sync.Mutex

	calls []LinkStoreCall

	// Lookup tables for the read methods. Tests that do not need a
	// custom hook can populate these directly. A nil map collapses to
	// "no hit, return (nil, nil)".
	ByID          map[uuid.UUID]*model.Link
	ByURL         map[string]*model.Link
	BySourceKey   map[string]*model.Link
	ListDoneRows  []model.Link
	ListDoneTotal int
	ListDoneErr   error

	// Typed per-method call records. Tests inspect these for ergonomic
	// typed-argument assertions (e.g. linkStore.CreateCalls[0].URL).
	CreateCalls         []repository.CreateLinkParams
	GetByIDCalls        []uuid.UUID
	GetByURLCalls       []string
	GetBySourceKeyCalls []string
	ListDoneCalls       []repository.ListLinksFilter
	UpdateStateCalls    []repository.UpdateLinkStateParams
	UpdateAnalysisCalls []repository.UpdateLinkAnalysisParams
	MarkProcessingCalls []ParseStateCall
	MarkFailedCalls     []ParseStateCall
	CompleteParseCalls  []CompleteParseCall
	DeleteCalls         []uuid.UUID

	// Terminal completion records. Production wires one repository object as
	// Links + ReadingCompleter + SiteCompleter, so this fake implements all
	// three surfaces too — a test can never accidentally exercise a different
	// terminal path than production does.
	CompleteReadingParseCalls []CompleteReadingParseCall
	CompleteSiteParseCalls    []CompleteSiteParseCall

	// CreateResult is the convenience "always return this on Create"
	// shortcut for tests that do not care about params. Consulted only
	// when CreateFunc is nil. Mirrors the legacy pattern shared across
	// the spy fakes this type replaces.
	CreateResult *model.Link

	// Optional per-method behavior hooks. nil means: use the lookup map
	// or CreateResult shortcut (for read / create methods that have one)
	// or fall through to BaseLinkStore (which panics).
	CreateFunc              func(context.Context, repository.CreateLinkParams) (*model.Link, error)
	GetByIDFunc             func(context.Context, uuid.UUID) (*model.Link, error)
	GetByURLFunc            func(context.Context, string) (*model.Link, error)
	GetBySourceKeyFunc      func(context.Context, string) (*model.Link, error)
	ListDoneFunc            func(context.Context, repository.ListLinksFilter) ([]model.Link, int, error)
	UpdateStateFunc         func(context.Context, repository.UpdateLinkStateParams) error
	UpdateAnalysisFunc      func(context.Context, repository.UpdateLinkAnalysisParams) error
	MarkParseProcessingFunc func(context.Context, uuid.UUID, uuid.UUID) error
	MarkParseFailedFunc     func(context.Context, uuid.UUID, uuid.UUID, string) error
	CompleteParseFunc       func(context.Context, repository.UpdateLinkAnalysisParams, uuid.UUID) error
	DeleteFunc              func(context.Context, uuid.UUID) error

	// CompleteReadingParseFunc / CompleteSiteParseFunc default to "record and
	// succeed" rather than panicking: every parse run ends in one of them, so
	// requiring each test to restate the hook would be pure noise. Tests that
	// care assert on the *Calls slices or override the hook.
	CompleteReadingParseFunc func(context.Context, repository.CompleteReadingParseParams, uuid.UUID) (repository.CompleteReadingParseResult, error)
	CompleteSiteParseFunc    func(context.Context, repository.CompleteSiteParseParams, uuid.UUID) (repository.SiteAggregateResult, error)
}

// Calls returns a snapshot of every method invocation observed so far.
// The slice is a copy so callers can iterate without holding the mutex.
func (o *ObservableLinkStore) Calls() []LinkStoreCall {
	o.mu.Lock()
	defer o.mu.Unlock()
	out := make([]LinkStoreCall, len(o.calls))
	copy(out, o.calls)
	return out
}

// CountCalls returns the number of times the named method was invoked.
func (o *ObservableLinkStore) CountCalls(method string) int {
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

func (o *ObservableLinkStore) record(method string, args ...any) {
	o.calls = append(o.calls, LinkStoreCall{Method: method, Args: args})
}

// Create 记录调用并依次回退到 CreateFunc / CreateResult / BaseLinkStore（panic）。
func (o *ObservableLinkStore) Create(ctx context.Context, params repository.CreateLinkParams) (*model.Link, error) {
	o.mu.Lock()
	o.record("Create", params)
	o.CreateCalls = append(o.CreateCalls, params)
	hook := o.CreateFunc
	result := o.CreateResult
	o.mu.Unlock()
	if hook != nil {
		return hook(ctx, params)
	}
	if result != nil {
		return result, nil
	}
	return o.BaseLinkStore.Create(ctx, params)
}

// GetByID 记录调用；优先调用 GetByIDFunc，否则查 ByID 表，未命中返回 (nil, nil)。
func (o *ObservableLinkStore) GetByID(ctx context.Context, id uuid.UUID) (*model.Link, error) {
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

// GetByURL 记录调用；优先调用 GetByURLFunc，否则查 ByURL 表，未命中返回 (nil, nil)。
func (o *ObservableLinkStore) GetByURL(ctx context.Context, url string) (*model.Link, error) {
	o.mu.Lock()
	o.record("GetByURL", url)
	o.GetByURLCalls = append(o.GetByURLCalls, url)
	hook := o.GetByURLFunc
	byURL := o.ByURL
	o.mu.Unlock()
	if hook != nil {
		return hook(ctx, url)
	}
	if byURL != nil {
		return byURL[url], nil
	}
	return nil, nil
}

// GetBySourceKey 记录调用；优先调用 GetBySourceKeyFunc，否则查 BySourceKey 表，未命中返回 (nil, nil)。
func (o *ObservableLinkStore) GetBySourceKey(ctx context.Context, key string) (*model.Link, error) {
	o.mu.Lock()
	o.record("GetBySourceKey", key)
	o.GetBySourceKeyCalls = append(o.GetBySourceKeyCalls, key)
	hook := o.GetBySourceKeyFunc
	bySK := o.BySourceKey
	o.mu.Unlock()
	if hook != nil {
		return hook(ctx, key)
	}
	if bySK != nil {
		return bySK[key], nil
	}
	return nil, nil
}

// GetBySourceKeyOrURL emulates the merged ingest lookup. Tests that
// only configure BySourceKey or ByURL inherit the natural fall-through:
// source_key matches first; url matches as a secondary lookup. That
// matches the SQL OR semantics in PGXLinkRepository.
//
// Test fidelity: this fake records two separate Get* calls in the
// counter slices when source_key misses and url matches, where the
// production single-SQL OR records one. Tests should not assert
// GetBySourceKeyCalls / GetByURLCalls counts to verify "merged round
// trip" — assert on the function's return value instead, or on the
// CallLog by method name.
func (o *ObservableLinkStore) GetBySourceKeyOrURL(ctx context.Context, key, url string) (*model.Link, error) {
	if link, err := o.GetBySourceKey(ctx, key); err != nil || link != nil {
		return link, err
	}
	if url == "" {
		return nil, nil
	}
	return o.GetByURL(ctx, url)
}

func (o *ObservableLinkStore) GetDetailByID(ctx context.Context, id uuid.UUID) (*repository.LinkDetailProjection, error) {
	link, err := o.GetByID(ctx, id)
	if link == nil || err != nil {
		return nil, err
	}
	projection := detailProjection(link)
	return &projection, nil
}

func (o *ObservableLinkStore) GetDetailByURL(ctx context.Context, rawURL string) (*repository.LinkDetailProjection, error) {
	link, err := o.GetByURL(ctx, rawURL)
	if link == nil && err == nil {
		link, err = o.GetBySourceKey(ctx, rawURL)
	}
	if link == nil || err != nil {
		return nil, err
	}
	projection := detailProjection(link)
	return &projection, nil
}

func (o *ObservableLinkStore) GetParseInputByID(ctx context.Context, id uuid.UUID) (*repository.LinkParseInput, error) {
	link, err := o.GetByID(ctx, id)
	if link == nil || err != nil {
		return nil, err
	}
	projection := parseInputProjection(link)
	return &projection, nil
}

func (o *ObservableLinkStore) GetParseInputBySourceKeyOrURL(ctx context.Context, sourceKey, rawURL string) (*repository.LinkParseInput, error) {
	link, err := o.GetBySourceKeyOrURL(ctx, sourceKey, rawURL)
	if link == nil || err != nil {
		return nil, err
	}
	projection := parseInputProjection(link)
	return &projection, nil
}

func (o *ObservableLinkStore) GetLifecycleByID(ctx context.Context, id uuid.UUID) (*repository.LinkLifecycleProjection, error) {
	link, err := o.GetByID(ctx, id)
	if link == nil || err != nil {
		return nil, err
	}
	projection := lifecycleProjection(link)
	return &projection, nil
}

func (o *ObservableLinkStore) GetSubmitLookupByID(ctx context.Context, id uuid.UUID) (*repository.LinkSubmitLookup, error) {
	link, err := o.GetByID(ctx, id)
	if link == nil || err != nil {
		return nil, err
	}
	projection := submitLookupProjection(link)
	return &projection, nil
}

func (o *ObservableLinkStore) GetSubmitLookupByURL(ctx context.Context, rawURL string) (*repository.LinkSubmitLookup, error) {
	link, err := o.GetByURL(ctx, rawURL)
	if link == nil && err == nil {
		link, err = o.GetBySourceKey(ctx, rawURL)
	}
	if link == nil || err != nil {
		return nil, err
	}
	projection := submitLookupProjection(link)
	return &projection, nil
}

func detailProjection(link *model.Link) repository.LinkDetailProjection {
	return repository.LinkDetailProjection{
		ID: link.ID, URL: link.URL, Title: link.Title, Summary: link.Summary, Tags: link.Tags,
		FetcherType: link.FetcherType, IsLowConfidence: link.IsLowConfidence,
		LowConfidenceReason: link.LowConfidenceReason, Status: link.Status, ErrorMsg: link.ErrorMsg,
		Description: link.Description, Domain: link.Domain, ContentType: link.ContentType,
		LibraryKind: link.LibraryKind, LibraryKindSource: link.LibraryKindSource,
		LibraryKindLocked: link.LibraryKindLocked, PredictedLibraryKind: link.PredictedLibraryKind,
		ClassificationConfidence:  link.ClassificationConfidence,
		ClassificationReason:      link.ClassificationReason,
		ClassificationExplanation: link.ClassificationExplanation,
		ClassifierVersion:         link.ClassifierVersion, ContentRevision: link.ContentRevision,
		ContentSource: link.ContentSource, HasContent: link.HasContent,
		ContentCJKChars: link.ContentCJKChars, ContentWords: link.ContentWords,
		PathDepth: link.PathDepth, ParentPath: link.ParentPath, ParentID: link.ParentID,
		CreatedAt: link.CreatedAt, UpdatedAt: link.UpdatedAt,
	}
}

func parseInputProjection(link *model.Link) repository.LinkParseInput {
	return repository.LinkParseInput{
		ID: link.ID, URL: link.URL, SourceKind: link.SourceKind, SourceKey: link.SourceKey,
		InputTitle: link.InputTitle, InputText: link.InputText, InputHTML: link.InputHTML,
		InputImages: link.InputImages, SourceMetadata: link.SourceMetadata,
		Description: link.Description, Status: link.Status,
		RequestedLibraryKind:       link.RequestedLibraryKind,
		RequestedLibraryKindSource: link.RequestedLibraryKindSource,
		LibraryKind:                link.LibraryKind,
		LibraryKindLocked:          link.LibraryKindLocked, ContentRevision: link.ContentRevision,
		UpdatedAt: link.UpdatedAt,
	}
}

func lifecycleProjection(link *model.Link) repository.LinkLifecycleProjection {
	return repository.LinkLifecycleProjection{
		ID: link.ID, URL: link.URL, Status: link.Status, LibraryKind: link.LibraryKind,
		LibraryKindSource: link.LibraryKindSource, LibraryKindLocked: link.LibraryKindLocked,
		ClassificationReason: link.ClassificationReason, ContentRevision: link.ContentRevision,
		HasContent: link.HasContent,
	}
}

func submitLookupProjection(link *model.Link) repository.LinkSubmitLookup {
	return repository.LinkSubmitLookup{
		ID: link.ID, URL: link.URL, SourceKey: link.SourceKey, Status: link.Status,
		LibraryKind: link.LibraryKind,
	}
}

// ListDone 记录调用；优先调用 ListDoneFunc，否则返回预设的 ListDoneRows / Total / Err。
func (o *ObservableLinkStore) ListDone(ctx context.Context, filter repository.ListLinksFilter) ([]model.Link, int, error) {
	o.mu.Lock()
	o.record("ListDone", filter)
	o.ListDoneCalls = append(o.ListDoneCalls, filter)
	hook := o.ListDoneFunc
	rows := append([]model.Link(nil), o.ListDoneRows...)
	total := o.ListDoneTotal
	err := o.ListDoneErr
	o.mu.Unlock()
	if hook != nil {
		return hook(ctx, filter)
	}
	return rows, total, err
}

// UpdateState 记录调用并回退到 UpdateStateFunc，未设置时通过 BaseLinkStore panic（防止意外写入悄然成功）。
func (o *ObservableLinkStore) UpdateState(ctx context.Context, params repository.UpdateLinkStateParams) error {
	o.mu.Lock()
	o.record("UpdateState", params)
	o.UpdateStateCalls = append(o.UpdateStateCalls, params)
	hook := o.UpdateStateFunc
	o.mu.Unlock()
	if hook != nil {
		return hook(ctx, params)
	}
	// No hook configured -> fall through to BaseLinkStore which panics
	// with "not implemented in fake". The previous "return nil" silently
	// signalled success, which let production bugs (an unintended
	// UpdateState call from a code path the test forgot to exercise)
	// pass under test. Tests that legitimately exercise this method
	// must either set UpdateStateFunc explicitly or assert via
	// UpdateStateCalls after configuring the hook.
	return o.BaseLinkStore.UpdateState(ctx, params)
}

// UpdateAnalysis 记录调用并回退到 UpdateAnalysisFunc，未设置时通过 BaseLinkStore panic。
func (o *ObservableLinkStore) UpdateAnalysis(ctx context.Context, params repository.UpdateLinkAnalysisParams) error {
	o.mu.Lock()
	o.record("UpdateAnalysis", params)
	o.UpdateAnalysisCalls = append(o.UpdateAnalysisCalls, params)
	hook := o.UpdateAnalysisFunc
	o.mu.Unlock()
	if hook != nil {
		return hook(ctx, params)
	}
	return o.BaseLinkStore.UpdateAnalysis(ctx, params)
}

func (o *ObservableLinkStore) MarkParseProcessing(ctx context.Context, linkID, jobID uuid.UUID) error {
	o.mu.Lock()
	o.record("MarkParseProcessing", linkID, jobID)
	o.MarkProcessingCalls = append(o.MarkProcessingCalls, ParseStateCall{LinkID: linkID, JobID: jobID})
	hook := o.MarkParseProcessingFunc
	o.mu.Unlock()
	if hook != nil {
		return hook(ctx, linkID, jobID)
	}
	return o.BaseLinkStore.MarkParseProcessing(ctx, linkID, jobID)
}

func (o *ObservableLinkStore) MarkParseFailed(ctx context.Context, linkID, jobID uuid.UUID, message string) error {
	o.mu.Lock()
	o.record("MarkParseFailed", linkID, jobID, message)
	o.MarkFailedCalls = append(o.MarkFailedCalls, ParseStateCall{LinkID: linkID, JobID: jobID, ErrorMessage: message})
	hook := o.MarkParseFailedFunc
	o.mu.Unlock()
	if hook != nil {
		return hook(ctx, linkID, jobID, message)
	}
	return o.BaseLinkStore.MarkParseFailed(ctx, linkID, jobID, message)
}

func (o *ObservableLinkStore) CompleteParse(ctx context.Context, params repository.UpdateLinkAnalysisParams, jobID uuid.UUID) error {
	o.mu.Lock()
	o.record("CompleteParse", params, jobID)
	o.UpdateAnalysisCalls = append(o.UpdateAnalysisCalls, params)
	o.CompleteParseCalls = append(o.CompleteParseCalls, CompleteParseCall{Analysis: params, JobID: jobID})
	hook := o.CompleteParseFunc
	o.mu.Unlock()
	if hook != nil {
		return hook(ctx, params, jobID)
	}
	return o.BaseLinkStore.CompleteParse(ctx, params, jobID)
}

// CompleteReadingParse 记录终态 reading 写入。未设置 hook 时默认成功——
// 解析流程必然终结于此，强制每个测试重述该 hook 只会制造噪音。
//
// 但「默认成功」不等于「无条件成功」：这里复刻 PGXLinkRepository.CompleteReadingParse
// 的前置校验（Kind 必须是 reading、分类与分析必须指向同一 link）。fake 一旦比生产
// 宽松，生产会拒绝的参数在测试里就悄悄通过——曾有一次归属为空字符串的回归正是
// 这样对整套测试隐形的。
func (o *ObservableLinkStore) CompleteReadingParse(ctx context.Context, params repository.CompleteReadingParseParams, jobID uuid.UUID) (repository.CompleteReadingParseResult, error) {
	// 校验先于记录：生产在守卫失败时整个事务回滚，不写任何行。若先记录再校验，
	// 断言 len(CompleteReadingParseCalls)==1 的测试对「参数非法、生产会拒绝」
	// 的场景仍会通过——守卫挡住了返回值，却没挡住观测记录。
	if err := ValidateReadingParse(params); err != nil {
		return repository.CompleteReadingParseResult{}, err
	}

	o.mu.Lock()
	o.record("CompleteReadingParse", params, jobID)
	o.UpdateAnalysisCalls = append(o.UpdateAnalysisCalls, params.Analysis)
	o.CompleteReadingParseCalls = append(o.CompleteReadingParseCalls, CompleteReadingParseCall{Params: params, JobID: jobID})
	hook := o.CompleteReadingParseFunc
	storedMetadataRevision := int64(0)
	if link := o.ByID[params.Analysis.ID]; link != nil {
		storedMetadataRevision = link.MetadataRevision
	}
	o.mu.Unlock()
	if hook != nil {
		return hook(ctx, params, jobID)
	}
	revision := params.Analysis.ExpectedMetadataRevision
	if revision <= 0 {
		// A pre-rollout job is still permitted to complete its lifecycle, but
		// it owns no user-facing metadata. Mirror the parser fence rather than
		// letting a zero fixture accidentally exercise embeddings or concepts.
		return repository.CompleteReadingParseResult{MetadataRevision: storedMetadataRevision, MetadataApplied: false}, nil
	}
	return repository.CompleteReadingParseResult{MetadataRevision: revision, MetadataApplied: true}, nil
}

// ValidateReadingParse 拒绝生产 CompleteReadingParse 事务内会拒绝的全部参数，
// 而不只是入口那一条：
//
//	parse_state_repository.go   Kind 必须是 reading、分析与分类同一 link
//	link_repo_library.go        updateLibraryClassificationOn 的 Source 白名单
//
// 第二条容易漏——它在被调用的下游函数里，不在 CompleteReadingParse 的函数体内。
// 漏掉的后果与 H-2 当初要防的完全一致，只是换到了姊妹字段上：若
// finalClassificationSource 将来多一个返回空 Source 的分支，service 层整套测试
// 会保持全绿，而生产解析会以 invalid source 失败。
func ValidateReadingParse(params repository.CompleteReadingParseParams) error {
	if params.Classification.Kind != model.LibraryKindReading || params.Analysis.ID != params.Classification.ID {
		return fmt.Errorf("repotest: complete reading parse: final reading classification and matching link id are required (kind=%q analysis=%s classification=%s)",
			params.Classification.Kind, params.Analysis.ID, params.Classification.ID)
	}
	// 直接调用生产实现，不复刻。
	return repository.ValidateLibraryKindSource(params.Classification.Source)
}

// CompleteSiteParse 记录终态 site 写入。未设置 hook 时返回零值聚合结果并成功，
// 但先复刻 PGXLinkRepository.CompleteSiteParse 的两条前置校验——理由同
// CompleteReadingParse：fake 可以比生产省事，不可以比生产宽松。
func (o *ObservableLinkStore) CompleteSiteParse(ctx context.Context, params repository.CompleteSiteParseParams, jobID uuid.UUID) (repository.SiteAggregateResult, error) {
	// 校验先于记录，理由同 CompleteReadingParse。
	if err := ValidateSiteParse(params); err != nil {
		return repository.SiteAggregateResult{}, err
	}

	o.mu.Lock()
	o.record("CompleteSiteParse", params, jobID)
	o.UpdateAnalysisCalls = append(o.UpdateAnalysisCalls, params.Analysis)
	o.CompleteSiteParseCalls = append(o.CompleteSiteParseCalls, CompleteSiteParseCall{Params: params, JobID: jobID})
	hook := o.CompleteSiteParseFunc
	o.mu.Unlock()
	if hook != nil {
		return hook(ctx, params, jobID)
	}
	return repository.SiteAggregateResult{}, nil
}

// ValidateSiteParse 拒绝生产 CompleteSiteParse 会拒绝的全部参数：
//
//	site_repository.go                     Kind 必须是 site、Source 非空、link id 一致
//	repository.ValidateAggregateSiteParams aggregateSiteOn 的五项聚合参数校验
//	repository.ValidateLibraryKindSource   library_kind_source 白名单
//
// 后两条**直接调用生产实现**而非复刻。复刻必然漂移：本函数曾只抄了
// name/entry_name 两条，漏掉 LinkID / IdentityKey / NormalizedURL——而 fake 一旦
// 比生产宽松，生产会拒绝的参数在测试里就静默通过。
//
// Source 白名单的出处值得一提：site 路径**不**经 updateLibraryClassificationOn
// （那是 reading 路径专有），它用 completeSiteLinkSQL 内联写入，Go 层没有对应
// 守卫，只由 DB 的 chk_links_library_kind_source 约束——fake 在此代 DB 拒绝。
func ValidateSiteParse(params repository.CompleteSiteParseParams) error {
	if params.Classification.Kind != model.LibraryKindSite || params.Classification.Source == "" {
		return fmt.Errorf("repotest: complete site parse: final site classification is required (kind=%q source=%q)",
			params.Classification.Kind, params.Classification.Source)
	}
	if params.Analysis.ID != params.Site.LinkID || params.Analysis.ID != params.Classification.ID {
		return fmt.Errorf("repotest: complete site parse: link ids must match (analysis=%s site=%s classification=%s)",
			params.Analysis.ID, params.Site.LinkID, params.Classification.ID)
	}
	if err := repository.ValidateAggregateSiteParams(params.Site); err != nil {
		return fmt.Errorf("repotest: %w", err)
	}
	return repository.ValidateLibraryKindSource(params.Classification.Source)
}

// Delete 记录调用并回退到 DeleteFunc，未设置时通过 BaseLinkStore panic。
func (o *ObservableLinkStore) Delete(ctx context.Context, id uuid.UUID) error {
	o.mu.Lock()
	o.record("Delete", id)
	o.DeleteCalls = append(o.DeleteCalls, id)
	hook := o.DeleteFunc
	o.mu.Unlock()
	if hook != nil {
		return hook(ctx, id)
	}
	return o.BaseLinkStore.Delete(ctx, id)
}

// Compile-time assertions: ObservableLinkStore satisfies LinkStore plus both
// terminal completer surfaces, mirroring production where a single repository
// object is wired as Links + ReadingCompleter + SiteCompleter.
var (
	_ repository.LinkStore             = (*ObservableLinkStore)(nil)
	_ repository.ReadingParseCompleter = (*ObservableLinkStore)(nil)
	_ repository.SiteParseCompleter    = (*ObservableLinkStore)(nil)
)
