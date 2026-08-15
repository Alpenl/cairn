package concept

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

type stubConceptLookup struct {
	concepts map[uuid.UUID]*Concept
	err      error
}

func (s *stubConceptLookup) GetConcept(_ context.Context, id uuid.UUID) (*Concept, error) {
	if s.err != nil {
		return nil, s.err
	}
	return s.concepts[id], nil
}

type stubMergeApprover struct {
	calls []struct {
		id uuid.UUID
		by string
	}
	err error
}

func (s *stubMergeApprover) MergeConceptsByProposal(_ context.Context, id uuid.UUID, by string) error {
	s.calls = append(s.calls, struct {
		id uuid.UUID
		by string
	}{id, by})
	return s.err
}

// stubProposalStore is an in-memory MergeProposalStore for the admin
// service tests. The v3 interface dropped FindCandidatePairs (pg_trgm
// scan retired) and added CountPendingProposals.
type stubProposalStore struct {
	mu          sync.Mutex
	createErr   error
	createCalls []CreateMergeProposalParams
	listResult  []MergeProposal
	listErr     error
	countResult int
	countErr    error
	getResult   *MergeProposal
	markErr     error
	markCalls   []struct {
		id         uuid.UUID
		status, by string
	}
}

func (s *stubProposalStore) CreateProposal(_ context.Context, p CreateMergeProposalParams) (uuid.UUID, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.createCalls = append(s.createCalls, p)
	if s.createErr != nil {
		return uuid.Nil, s.createErr
	}
	return uuid.New(), nil
}

func (s *stubProposalStore) ListPendingProposals(context.Context, int, int) ([]MergeProposal, error) {
	return s.listResult, s.listErr
}

func (s *stubProposalStore) CountPendingProposals(context.Context) (int, error) {
	return s.countResult, s.countErr
}

func (s *stubProposalStore) GetProposal(context.Context, uuid.UUID) (*MergeProposal, error) {
	return s.getResult, nil
}

func (s *stubProposalStore) MarkProposalDecided(_ context.Context, id uuid.UUID, status, by string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.markCalls = append(s.markCalls, struct {
		id         uuid.UUID
		status, by string
	}{id, status, by})
	return s.markErr
}

func TestMergeAdminListHydratesWinnerAndLoserNames(t *testing.T) {
	t.Parallel()

	winnerID := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	loserID := uuid.MustParse("22222222-2222-2222-2222-222222222222")
	now := time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC)

	proposals := &stubProposalStore{
		listResult: []MergeProposal{{
			ID:        uuid.MustParse("33333333-3333-3333-3333-333333333333"),
			WinnerID:  winnerID,
			LoserID:   loserID,
			Score:     0.82,
			LLMReason: "same concept",
			Status:    MergeProposalPending,
			CreatedAt: now,
		}},
	}
	lookup := &stubConceptLookup{concepts: map[uuid.UUID]*Concept{
		winnerID: {ID: winnerID, PrimaryName: "RAG", UseCount: 17},
		loserID:  {ID: loserID, PrimaryName: "rag", UseCount: 3},
	}}
	svc := NewMergeAdminService(MergeAdminServiceOptions{
		Proposals: proposals,
		Concepts:  lookup,
	})

	got, err := svc.ListPendingViews(context.Background(), 50, 0)
	if err != nil {
		t.Fatalf("ListPendingViews err = %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("len = %d, want 1", len(got))
	}
	v := got[0]
	if v.WinnerName != "RAG" || v.LoserName != "rag" {
		t.Fatalf("name hydration failed: %+v", v)
	}
	if v.WinnerUseCount != 17 || v.LoserUseCount != 3 {
		t.Fatalf("use_count hydration failed: %+v", v)
	}
	if v.CreatedAtUnix != now.Unix() {
		t.Fatalf("CreatedAtUnix = %d, want %d", v.CreatedAtUnix, now.Unix())
	}
}

func TestMergeAdminListSurvivesMissingConcept(t *testing.T) {
	// Race: judge wrote proposal, another approval already absorbed the
	// concept before list runs. The hydration should leave the name
	// empty (so the admin sees the row and rejects it) rather than 500.
	t.Parallel()

	winnerID := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	loserID := uuid.MustParse("22222222-2222-2222-2222-222222222222")
	proposals := &stubProposalStore{
		listResult: []MergeProposal{{
			ID: uuid.New(), WinnerID: winnerID, LoserID: loserID,
		}},
	}
	lookup := &stubConceptLookup{concepts: map[uuid.UUID]*Concept{
		// Only winner exists; loser was already merged away.
		winnerID: {ID: winnerID, PrimaryName: "RAG"},
	}}
	svc := NewMergeAdminService(MergeAdminServiceOptions{
		Proposals: proposals, Concepts: lookup,
	})

	got, err := svc.ListPendingViews(context.Background(), 50, 0)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if got[0].WinnerName != "RAG" || got[0].LoserName != "" {
		t.Fatalf("expected loser name empty when concept gone; got %+v", got[0])
	}
}

func TestMergeAdminGetAuditDoesNotRequireLiveConcepts(t *testing.T) {
	t.Parallel()
	id, winner, deletedLoser := uuid.New(), uuid.New(), uuid.New()
	decidedAt := time.Date(2026, 8, 14, 10, 0, 0, 0, time.UTC)
	proposals := &stubProposalStore{getResult: &MergeProposal{
		ID: id, WinnerID: winner, LoserID: deletedLoser, Score: 0.88,
		LLMReason: "same", Status: MergeProposalApproved, DecidedBy: "ops", DecidedAt: decidedAt,
	}}
	svc := NewMergeAdminService(MergeAdminServiceOptions{Proposals: proposals})

	got, err := svc.GetAudit(context.Background(), id)
	if err != nil {
		t.Fatalf("GetAudit() error = %v", err)
	}
	if got == nil || got.ID != id || got.WinnerID != winner || got.LoserID != deletedLoser || got.DecidedBy != "ops" || !got.DecidedAt.Equal(decidedAt) {
		t.Fatalf("GetAudit() = %+v", got)
	}
}

func TestMergeAdminApproveDelegatesToMerger(t *testing.T) {
	t.Parallel()
	merger := &stubMergeApprover{}
	svc := NewMergeAdminService(MergeAdminServiceOptions{
		Proposals: &stubProposalStore{},
		Merger:    merger,
	})
	id := uuid.New()
	if err := svc.Approve(context.Background(), id, "ops"); err != nil {
		t.Fatalf("Approve err = %v", err)
	}
	if len(merger.calls) != 1 || merger.calls[0].id != id || merger.calls[0].by != "ops" {
		t.Fatalf("merger.calls = %+v", merger.calls)
	}
}

func TestMergeAdminApprovePropagatesMergerError(t *testing.T) {
	t.Parallel()
	merger := &stubMergeApprover{err: errors.New("tx aborted")}
	svc := NewMergeAdminService(MergeAdminServiceOptions{
		Proposals: &stubProposalStore{},
		Merger:    merger,
	})
	if err := svc.Approve(context.Background(), uuid.New(), "ops"); err == nil {
		t.Fatal("Approve should propagate merger error")
	}
}

func TestMergeAdminApproveFlushesAliasCacheOnSuccess(t *testing.T) {
	t.Parallel()
	// The merge tx may have repointed or deleted concept_alias rows;
	// the in-memory alias cache must be flushed so subsequent Resolve
	// calls don't return the now-deleted loser concept_id.
	merger := &stubMergeApprover{}
	cache := &countingAliasInvalidator{}
	svc := NewMergeAdminService(MergeAdminServiceOptions{
		Proposals:  &stubProposalStore{},
		Merger:     merger,
		AliasCache: cache,
	})
	if err := svc.Approve(context.Background(), uuid.New(), "ops"); err != nil {
		t.Fatalf("Approve err = %v", err)
	}
	if cache.count != 1 {
		t.Errorf("InvalidateAll calls = %d; want 1 (cache flush regression)", cache.count)
	}
}

func TestMergeAdminApproveSkipsCacheFlushOnMergerError(t *testing.T) {
	t.Parallel()
	// A failed merge leaves the cache consistent with the DB (no rows
	// changed), so flushing would only churn entries for nothing.
	merger := &stubMergeApprover{err: errors.New("tx aborted")}
	cache := &countingAliasInvalidator{}
	svc := NewMergeAdminService(MergeAdminServiceOptions{
		Proposals:  &stubProposalStore{},
		Merger:     merger,
		AliasCache: cache,
	})
	if err := svc.Approve(context.Background(), uuid.New(), "ops"); err == nil {
		t.Fatal("Approve should propagate merger error")
	}
	if cache.count != 0 {
		t.Errorf("InvalidateAll calls = %d; want 0 on merger failure", cache.count)
	}
}

type countingAliasInvalidator struct {
	count int
}

func (c *countingAliasInvalidator) InvalidateAll() { c.count++ }

func TestMergeAdminRejectCallsMarkDecidedRejected(t *testing.T) {
	t.Parallel()
	proposals := &stubProposalStore{}
	svc := NewMergeAdminService(MergeAdminServiceOptions{Proposals: proposals})
	id := uuid.New()
	if err := svc.Reject(context.Background(), id, "ops"); err != nil {
		t.Fatalf("Reject err = %v", err)
	}
	if len(proposals.markCalls) != 1 ||
		proposals.markCalls[0].status != MergeProposalRejected ||
		proposals.markCalls[0].id != id ||
		proposals.markCalls[0].by != "ops" {
		t.Fatalf("markCalls = %+v", proposals.markCalls)
	}
}

func TestMergeAdminApproveErrorsWhenMergerNil(t *testing.T) {
	t.Parallel()
	svc := NewMergeAdminService(MergeAdminServiceOptions{Proposals: &stubProposalStore{}})
	if err := svc.Approve(context.Background(), uuid.New(), "ops"); err == nil {
		t.Fatal("Approve without Merger should error")
	}
}
