package dbintegration

import (
	"encoding/json"
	"errors"
	"sync"
	"testing"

	"github.com/google/uuid"

	"webtag/internal/model"
	"webtag/internal/repository"
	"webtag/internal/service"
)

// TestReaderInboxOwnershipAndConfirmationContract is the real-PostgreSQL
// acceptance matrix for the fields that deliberately have independent owners.
// It also proves category readback/migration, canonical confirmation retries,
// and a late proposal result after a user metadata write.
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
	category, err := repo.CreateCategory(ctx, "research")
	if err != nil {
		t.Fatalf("CreateCategory: %v", err)
	}
	if err := repo.SetCategoryMembership(ctx, category.ID, "inbox", created.ID.String(), true); err != nil {
		t.Fatalf("SetCategoryMembership(inbox): %v", err)
	}

	before, err := repo.GetInbox(ctx, created.ID)
	if err != nil {
		t.Fatalf("GetInbox before proposal: %v", err)
	}
	if before.Note != "private note" || before.Summary != nil || len(before.SuggestedTags) != 0 || len(before.CategoryIDs) != 1 || before.CategoryIDs[0] != category.ID || string(before.ProposalSignals) != `{}` || before.ProposalStatus != "pending" {
		t.Fatalf("initial independent ownership/readback = %#v", before)
	}
	if _, err := repo.PatchInbox(ctx, modelReaderInboxPatchContract(created.ID, stringPtrContract("stale"), nil, nil, nil, before.MetadataRevision+1)); !errors.Is(err, repository.ErrRevisionConflict) {
		t.Fatalf("PatchInbox stale revision error = %v, want ErrRevisionConflict", err)
	}

	job, inserted, err := repo.BeginInboxResummarizeJob(ctx, created.ID, before.MetadataRevision)
	if err != nil || !inserted {
		t.Fatalf("BeginInboxResummarizeJob error=%v inserted=%v", err, inserted)
	}
	if _, err := repo.ClaimInboxJob(ctx, job.ID); err != nil {
		t.Fatalf("ClaimInboxJob: %v", err)
	}
	updatedTitle, updatedBody, updatedNote := "User title", "user body", "user note"
	updated, err := repo.PatchInbox(ctx, modelReaderInboxPatchContract(created.ID, &updatedTitle, &updatedBody, &updatedNote, []string{"user-final"}, before.MetadataRevision))
	if err != nil {
		t.Fatalf("PatchInbox user-owned fields: %v", err)
	}
	if err := repo.CompleteInboxJob(ctx, job.ID, "AI summary", []string{"ai-suggested"}); err != nil {
		t.Fatalf("CompleteInboxJob after user edit: %v", err)
	}
	// A retry after a successful worker completion is idempotent.
	if err := repo.CompleteInboxJob(ctx, job.ID, "ignored retry", []string{"ignored"}); err != nil {
		t.Fatalf("CompleteInboxJob retry: %v", err)
	}
	after, err := repo.GetInbox(ctx, created.ID)
	if err != nil {
		t.Fatalf("GetInbox after proposal: %v", err)
	}
	if after.Title == nil || *after.Title != updatedTitle || after.Body != updatedBody || after.Note != updatedNote || after.MetadataRevision != updated.MetadataRevision || len(after.Tags) != 1 || after.Tags[0] != "user-final" {
		t.Fatalf("late AI overwrote user partition: %#v", after)
	}
	if after.Summary == nil || *after.Summary != "AI summary" || len(after.SuggestedTags) != 1 || after.SuggestedTags[0] != "ai-suggested" || after.ProposalStatus != "completed" || string(after.ProposalSignals) != `{}` {
		t.Fatalf("proposal partition/readback = %#v", after)
	}

	// Confirmation moves categories to the canonical link in the same
	// transaction and remains safe when two clients race to confirm.
	var wg sync.WaitGroup
	confirmIDs := make(chan uuid.UUID, 2)
	errs := make(chan error, 2)
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			id, confirmErr := repo.ConfirmInbox(ctx, created.ID)
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
	var categoryCount int
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM reader_categorizables WHERE category_id=$1 AND host_kind='link' AND host_id=$2`, category.ID, confirmed[0].String()).Scan(&categoryCount); err != nil {
		t.Fatalf("read migrated category: %v", err)
	}
	if categoryCount != 1 {
		t.Fatalf("migrated category count = %d, want 1", categoryCount)
	}

	blank, err := repo.CreateInbox(ctx, modelReaderInboxContract("https://inbox-contract.example/blank", stringPtrContract("  "), "body", "note", nil))
	if err != nil {
		t.Fatalf("CreateInbox blank: %v", err)
	}
	reader := service.NewReaderVNextService(repo, nil)
	if _, err := reader.ConfirmInbox(ctx, blank.ID.String(), blank.MetadataRevision); err == nil {
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
	if err := repo.SetCategoryMembership(ctx, category.ID, "inbox", rollback.ID.String(), true); err != nil {
		t.Fatalf("SetCategoryMembership rollback fixture: %v", err)
	}
	if _, err := pool.Exec(t.Context(), `CREATE OR REPLACE FUNCTION reader_inbox_contract_abort_category_move() RETURNS trigger AS $$ BEGIN RAISE EXCEPTION 'injected category migration failure'; END; $$ LANGUAGE plpgsql`); err != nil {
		t.Fatalf("create rollback trigger function: %v", err)
	}
	if _, err := pool.Exec(t.Context(), `CREATE TRIGGER trg_reader_inbox_contract_abort_category_move BEFORE UPDATE ON reader_categorizables FOR EACH ROW EXECUTE FUNCTION reader_inbox_contract_abort_category_move()`); err != nil {
		t.Fatalf("create rollback trigger: %v", err)
	}
	if _, err := repo.ConfirmInbox(ctx, rollback.ID); err == nil {
		t.Fatal("ConfirmInbox with injected category migration failure succeeded")
	}
	if _, err := pool.Exec(t.Context(), `DROP TRIGGER trg_reader_inbox_contract_abort_category_move ON reader_categorizables`); err != nil {
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

func modelReaderInboxContract(url string, title *string, body, note string, tags []string) model.ReaderInbox {
	return model.ReaderInbox{URL: url, IdentityKey: url, SourceKind: "browser_capture", Title: title, Body: body, Note: note, Tags: tags, ProposalSignals: json.RawMessage(`{}`), ProposalStatus: "pending"}
}

func modelReaderInboxPatchContract(id uuid.UUID, title, body, note *string, tags []string, revision int64) model.ReaderInboxPatch {
	return model.ReaderInboxPatch{ID: id, Title: title, Body: body, Note: note, Tags: tags, ExpectedRevision: revision}
}

func stringPtrContract(value string) *string { return &value }
