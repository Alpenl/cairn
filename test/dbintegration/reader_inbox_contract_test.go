package dbintegration

import (
	"errors"
	"sync"
	"testing"

	"github.com/google/uuid"

	"webtag/internal/model"
	"webtag/internal/repository"
)

// TestReaderInboxOwnershipAndConfirmationContract is the real-PostgreSQL
// acceptance matrix for the fields that deliberately have independent owners.
// It also proves canonical confirmation retries and a late proposal result
// after a user metadata write.
func TestReaderInboxOwnershipAndConfirmationContract(t *testing.T) {
	pool := StartPostgres(t)
	repo := repository.NewPGXReaderVNextRepository(pool)
	ctx := t.Context()

	title := "Draft title"
	created, err := repo.CreateInbox(ctx, modelReaderInboxContract(
		"https://inbox-contract.example/article", &title, "draft body", "private note", []string{"user-tag"},
	))
	if err != nil {
		t.Fatalf("CreateInbox: %v", err)
	}
	before, err := repo.GetInbox(ctx, created.ID)
	if err != nil {
		t.Fatalf("GetInbox before proposal: %v", err)
	}
	if before.Note != "private note" || before.Summary != nil || len(before.SuggestedTags) != 0 || before.ProposalStatus != "idle" {
		t.Fatalf("initial independent ownership/readback = %#v", before)
	}
	if _, err := repo.PatchInbox(ctx, modelReaderInboxPatchContract(created.ID, stringPtrContract("stale"), nil, nil, nil, before.MetadataRevision+1)); !errors.Is(err, repository.ErrRevisionConflict) {
		t.Fatalf("PatchInbox stale revision error = %v, want ErrRevisionConflict", err)
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin initial proposal transaction: %v", err)
	}
	if _, err := repo.StartInboxProposalTx(ctx, tx, created.ID, before.MetadataRevision); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("StartInboxProposalTx: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit initial proposal: %v", err)
	}
	if _, err := repo.ClaimInboxProposal(ctx, created.ID, before.MetadataRevision); err != nil {
		t.Fatalf("ClaimInboxProposal: %v", err)
	}
	tx, err = pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin duplicate proposal transaction: %v", err)
	}
	duplicate, err := repo.StartInboxProposalTx(ctx, tx, created.ID, before.MetadataRevision)
	if err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("duplicate StartInboxProposalTx: %v", err)
	}
	if duplicate.ProposalStatus != "running" {
		_ = tx.Rollback(ctx)
		t.Fatalf("duplicate StartInboxProposalTx status = %q, want running", duplicate.ProposalStatus)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit duplicate proposal: %v", err)
	}
	updatedTitle, updatedBody, updatedNote := "User title", "user body", "user note"
	updated, err := repo.PatchInbox(ctx, modelReaderInboxPatchContract(created.ID, &updatedTitle, &updatedBody, &updatedNote, []string{"user-final"}, before.MetadataRevision))
	if err != nil {
		t.Fatalf("PatchInbox user-owned fields: %v", err)
	}
	if err := repo.CompleteInboxProposal(ctx, created.ID, before.MetadataRevision, "stale AI summary", []string{"stale"}); !errors.Is(err, repository.ErrReaderInboxProposalNotRunnable) {
		t.Fatalf("CompleteInboxProposal after user edit error = %v, want ErrReaderInboxProposalNotRunnable", err)
	}
	staleDropped, err := repo.GetInbox(ctx, created.ID)
	if err != nil {
		t.Fatalf("GetInbox after stale proposal: %v", err)
	}
	if staleDropped.Summary != nil || len(staleDropped.SuggestedTags) != 0 || staleDropped.ProposalStatus != "idle" {
		t.Fatalf("stale proposal mutated current draft = %#v", staleDropped)
	}

	tx, err = pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin current proposal transaction: %v", err)
	}
	if _, err := repo.StartInboxProposalTx(ctx, tx, created.ID, updated.MetadataRevision); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("StartInboxProposalTx current revision: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit current proposal: %v", err)
	}
	if _, err := repo.ClaimInboxProposal(ctx, created.ID, updated.MetadataRevision); err != nil {
		t.Fatalf("ClaimInboxProposal current revision: %v", err)
	}
	if err := repo.CompleteInboxProposal(ctx, created.ID, updated.MetadataRevision, "AI summary", []string{"ai-suggested"}); err != nil {
		t.Fatalf("CompleteInboxProposal current revision: %v", err)
	}
	after, err := repo.GetInbox(ctx, created.ID)
	if err != nil {
		t.Fatalf("GetInbox after proposal: %v", err)
	}
	if after.Title == nil || *after.Title != updatedTitle || after.Body != updatedBody || after.Note != updatedNote || after.MetadataRevision != updated.MetadataRevision || len(after.Tags) != 1 || after.Tags[0] != "user-final" {
		t.Fatalf("late AI overwrote user partition: %#v", after)
	}
	if after.Summary == nil || *after.Summary != "AI summary" || len(after.SuggestedTags) != 1 || after.SuggestedTags[0] != "ai-suggested" || after.ProposalStatus != "completed" {
		t.Fatalf("proposal partition/readback = %#v", after)
	}

	// Confirmation remains safe when two clients race to confirm.
	var wg sync.WaitGroup
	confirmIDs := make(chan uuid.UUID, 2)
	errs := make(chan error, 2)
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			id, confirmErr := repo.ConfirmInbox(ctx, created.ID, nil)
			confirmIDs <- id
			errs <- confirmErr
		}()
	}
	wg.Wait()
	close(confirmIDs)
	close(errs)
	var confirmed []uuid.UUID
	for confirmErr := range errs {
		if confirmErr != nil {
			t.Fatalf("concurrent ConfirmInbox: %v", confirmErr)
		}
	}
	for id := range confirmIDs {
		confirmed = append(confirmed, id)
	}
	if len(confirmed) != 2 || confirmed[0] != confirmed[1] {
		t.Fatalf("concurrent confirmation identities = %#v", confirmed)
	}
	blank, err := repo.CreateInbox(ctx, modelReaderInboxContract("https://inbox-contract.example/blank", stringPtrContract("  "), "body", "note", nil))
	if err != nil {
		t.Fatalf("CreateInbox blank: %v", err)
	}
	reader := postgresReaderApplications(t, pool, repo).Inbox
	blankRevision := blank.MetadataRevision
	if _, err := reader.ConfirmInbox(ctx, blank.ID, &blankRevision); err == nil {
		t.Fatal("ConfirmInbox blank title succeeded")
	}
	var blankStatus string
	if err := pool.QueryRow(t.Context(), `SELECT status FROM reader_inbox WHERE id=$1`, blank.ID).Scan(&blankStatus); err != nil {
		t.Fatalf("read blank inbox state: %v", err)
	}
	if blankStatus != "pending" {
		t.Fatalf("blank-title confirmation partially wrote status %q", blankStatus)
	}

	rollbackTitle := "rollback"
	rollback, err := repo.CreateInbox(ctx, modelReaderInboxContract("https://inbox-contract.example/rollback", &rollbackTitle, "body", "note", nil))
	if err != nil {
		t.Fatalf("CreateInbox rollback fixture: %v", err)
	}
	if _, err := pool.Exec(t.Context(), `CREATE OR REPLACE FUNCTION reader_inbox_contract_abort_finalize() RETURNS trigger AS $$ BEGIN RAISE EXCEPTION 'injected confirmation finalization failure'; END; $$ LANGUAGE plpgsql`); err != nil {
		t.Fatalf("create rollback trigger function: %v", err)
	}
	if _, err := pool.Exec(t.Context(), `CREATE TRIGGER trg_reader_inbox_contract_abort_finalize BEFORE UPDATE ON reader_inbox FOR EACH ROW WHEN (NEW.status='confirmed') EXECUTE FUNCTION reader_inbox_contract_abort_finalize()`); err != nil {
		t.Fatalf("create rollback trigger: %v", err)
	}
	if _, err := repo.ConfirmInbox(ctx, rollback.ID, nil); err == nil {
		t.Fatal("ConfirmInbox with injected finalization failure succeeded")
	}
	if _, err := pool.Exec(t.Context(), `DROP TRIGGER trg_reader_inbox_contract_abort_finalize ON reader_inbox`); err != nil {
		t.Fatalf("drop rollback trigger: %v", err)
	}
	var rollbackStatus string
	var rollbackLinks int
	if err := pool.QueryRow(t.Context(), `SELECT status FROM reader_inbox WHERE id=$1`, rollback.ID).Scan(&rollbackStatus); err != nil {
		t.Fatalf("read rollback inbox: %v", err)
	}
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM links WHERE source_key=$1`, "https://inbox-contract.example/rollback").Scan(&rollbackLinks); err != nil {
		t.Fatalf("count rollback links: %v", err)
	}
	if rollbackStatus != "pending" || rollbackLinks != 0 {
		t.Fatalf("confirmation rollback status=%q links=%d, want pending/0", rollbackStatus, rollbackLinks)
	}
}

func TestReaderInboxDiscardUsesTrashTombstoneAndRollsBackAtomically(t *testing.T) {
	pool := StartPostgres(t)
	repo := repository.NewPGXReaderVNextRepository(pool)
	ctx := t.Context()

	title := "Discardable"
	discarded, err := repo.CreateInbox(ctx, modelReaderInboxContract(
		"https://inbox-contract.example/discard", &title, "body", "note", nil,
	))
	if err != nil {
		t.Fatalf("CreateInbox discard fixture: %v", err)
	}
	if err := repo.DiscardInbox(ctx, discarded.ID); err != nil {
		t.Fatalf("DiscardInbox: %v", err)
	}
	var status string
	var deleted bool
	if err := pool.QueryRow(ctx, `SELECT status,deleted_at IS NOT NULL FROM reader_inbox WHERE id=$1`, discarded.ID).Scan(&status, &deleted); err != nil {
		t.Fatalf("read discarded Inbox row: %v", err)
	}
	if status != "pending" || !deleted {
		t.Fatalf("discarded Inbox storage = status:%q deleted:%t, want pending/true", status, deleted)
	}
	if _, err := repo.GetInbox(ctx, discarded.ID); !errors.Is(err, repository.ErrNotFound) {
		t.Fatalf("GetInbox discarded row error = %v, want ErrNotFound", err)
	}
	if err := repo.RestoreInbox(ctx, discarded.ID); err != nil {
		t.Fatalf("RestoreInbox discarded row: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT status,deleted_at IS NOT NULL FROM reader_inbox WHERE id=$1`, discarded.ID).Scan(&status, &deleted); err != nil {
		t.Fatalf("read restored Inbox row: %v", err)
	}
	if status != "pending" || deleted {
		t.Fatalf("restored Inbox storage = status:%q deleted:%t, want pending/false", status, deleted)
	}

	confirmedTitle := "Confirmed"
	confirmed, err := repo.CreateInbox(ctx, modelReaderInboxContract(
		"https://inbox-contract.example/confirmed", &confirmedTitle, "body", "note", nil,
	))
	if err != nil {
		t.Fatalf("CreateInbox confirmed fixture: %v", err)
	}
	if _, err := repo.ConfirmInbox(ctx, confirmed.ID, nil); err != nil {
		t.Fatalf("ConfirmInbox fixture: %v", err)
	}
	if err := repo.DiscardInbox(ctx, confirmed.ID); !errors.Is(err, repository.ErrReaderInboxStateConflict) {
		t.Fatalf("DiscardInbox confirmed row error = %v, want ErrReaderInboxStateConflict", err)
	}

	pendingTitle := "Atomic pending"
	pending, err := repo.CreateInbox(ctx, modelReaderInboxContract(
		"https://inbox-contract.example/atomic", &pendingTitle, "body", "note", nil,
	))
	if err != nil {
		t.Fatalf("CreateInbox atomic fixture: %v", err)
	}
	if _, err := repo.BulkDiscardInbox(ctx, []uuid.UUID{pending.ID, confirmed.ID}); !errors.Is(err, repository.ErrReaderInboxStateConflict) {
		t.Fatalf("BulkDiscardInbox mixed state error = %v, want ErrReaderInboxStateConflict", err)
	}
	if err := pool.QueryRow(ctx, `SELECT deleted_at IS NOT NULL FROM reader_inbox WHERE id=$1`, pending.ID).Scan(&deleted); err != nil {
		t.Fatalf("read atomic pending fixture: %v", err)
	}
	if deleted {
		t.Fatal("BulkDiscardInbox partially tombstoned a pending row before rollback")
	}
}

func modelReaderInboxContract(url string, title *string, body, note string, tags []string) model.ReaderInbox {
	return model.ReaderInbox{URL: url, IdentityKey: url, SourceKind: "browser_capture", Title: title, Body: body, Note: note, Tags: tags, ProposalStatus: "idle"}
}

func modelReaderInboxPatchContract(id uuid.UUID, title, body, note *string, tags []string, revision int64) model.ReaderInboxPatch {
	return model.ReaderInboxPatch{ID: id, Title: title, Body: body, Note: note, Tags: tags, ExpectedRevision: revision}
}

func stringPtrContract(value string) *string { return &value }
