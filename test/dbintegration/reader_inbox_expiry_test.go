package dbintegration

import (
	"bytes"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"

	"webtag/internal/model"
	"webtag/internal/repository"
)

func TestRestoreExpiredLiveInboxRenewsOnlyExpiryState(t *testing.T) {
	pool := StartPostgres(t)
	ctx := t.Context()
	repo := repository.NewPGXReaderVNextRepository(pool)
	host := seedReaderLifecycleHost(t, pool, model.ReaderHostInbox, "expiry-restore")
	category, err := repo.CreateCategory(ctx, "expiry category")
	if err != nil {
		t.Fatalf("CreateCategory: %v", err)
	}
	if err := repo.SetCategoryMembership(ctx, category.ID, "inbox", host.id.String(), true); err != nil {
		t.Fatalf("SetCategoryMembership: %v", err)
	}
	thought := seedReaderLifecycleThought(t, repo, ctx, host, "expiry-restore", "anchor")

	deadline := time.Now().UTC().Add(-time.Hour)
	materializedAt := deadline.Add(10 * time.Minute)
	leaseID := uuid.New()
	proposalSignals := json.RawMessage(`{"source":"expiry-test"}`)
	if _, err := pool.Exec(t.Context(), `
		UPDATE reader_inbox
		SET note='preserved private note',
			suggested_tags=ARRAY['suggested'],
			proposal_signals=$2::jsonb,
			proposal_status='completed',
			expires_at=$3,
			expired_at=$4,
			expiry_lease_id=$5,
			expiry_lease_until=$6
		WHERE id=$1`, host.id, []byte(proposalSignals), deadline, materializedAt, leaseID, materializedAt.Add(time.Hour)); err != nil {
		t.Fatalf("seed expired Inbox row: %v", err)
	}

	before, err := repo.GetInbox(ctx, host.id)
	if err != nil {
		t.Fatalf("GetInbox before restore: %v", err)
	}
	beforeThought, err := repo.GetThought(ctx, thought.id)
	if err != nil {
		t.Fatalf("GetThought before restore: %v", err)
	}
	var beforeOps int
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM reader_thought_ops WHERE annotation_id=$1`, thought.id).Scan(&beforeOps); err != nil {
		t.Fatalf("count thought ops before restore: %v", err)
	}

	started := time.Now().UTC()
	if err := repo.RestoreInbox(ctx, host.id); err != nil {
		t.Fatalf("RestoreInbox: %v", err)
	}
	finished := time.Now().UTC()
	after, err := repo.GetInbox(ctx, host.id)
	if err != nil {
		t.Fatalf("GetInbox after restore: %v", err)
	}
	if after.Status != "pending" || after.Expired || after.ExpiredAt != nil || after.ExpiresAt == nil || after.DeletedAt != nil {
		t.Fatalf("restored Inbox lifecycle = %#v", after)
	}
	if after.ExpiresAt.Before(started.Add(30*24*time.Hour-time.Minute)) || after.ExpiresAt.After(finished.Add(30*24*time.Hour+time.Minute)) {
		t.Fatalf("renewed expires_at = %s, want now + 30 days", after.ExpiresAt)
	}
	if after.Title == nil || before.Title == nil || *after.Title != *before.Title || after.Body != before.Body || after.Note != before.Note || after.Summary == nil || before.Summary == nil || *after.Summary != *before.Summary || !bytes.Equal(after.ProposalSignals, before.ProposalSignals) || after.ProposalStatus != before.ProposalStatus || len(after.SuggestedTags) != 1 || after.SuggestedTags[0] != "suggested" {
		t.Fatalf("restore changed Inbox-owned content or proposal data: before=%#v after=%#v", before, after)
	}
	if len(after.CategoryIDs) != 1 || after.CategoryIDs[0] != category.ID {
		t.Fatalf("restore categories = %#v, want %s", after.CategoryIDs, category.ID)
	}
	afterThought, err := repo.GetThought(ctx, thought.id)
	if err != nil {
		t.Fatalf("GetThought after restore: %v", err)
	}
	if afterThought.LastSequence != beforeThought.LastSequence || afterThought.Body != beforeThought.Body || !bytes.Equal(afterThought.Target, beforeThought.Target) || !bytes.Equal(afterThought.Quote, beforeThought.Quote) {
		t.Fatalf("restore changed live thought: before=%#v after=%#v", beforeThought, afterThought)
	}
	var expiredNull, leaseIDNull, leaseUntilNull bool
	if err := pool.QueryRow(t.Context(), `SELECT expired_at IS NULL,expiry_lease_id IS NULL,expiry_lease_until IS NULL FROM reader_inbox WHERE id=$1`, host.id).Scan(&expiredNull, &leaseIDNull, &leaseUntilNull); err != nil {
		t.Fatalf("read restored expiry state: %v", err)
	}
	if !expiredNull || !leaseIDNull || !leaseUntilNull {
		t.Fatalf("restored expiry state = expired_null:%v lease_id_null:%v lease_until_null:%v", expiredNull, leaseIDNull, leaseUntilNull)
	}

	firstDeadline := *after.ExpiresAt
	if err := repo.RestoreInbox(ctx, host.id); err != nil {
		t.Fatalf("idempotent RestoreInbox retry: %v", err)
	}
	afterRetry, err := repo.GetInbox(ctx, host.id)
	if err != nil {
		t.Fatalf("GetInbox after retry: %v", err)
	}
	if afterRetry.ExpiresAt == nil || !afterRetry.ExpiresAt.Equal(firstDeadline) || afterRetry.ExpiredAt != nil {
		t.Fatalf("retry changed restored expiry state: %#v", afterRetry)
	}
	var afterOps int
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM reader_thought_ops WHERE annotation_id=$1`, thought.id).Scan(&afterOps); err != nil {
		t.Fatalf("count thought ops after restore: %v", err)
	}
	if afterOps != beforeOps {
		t.Fatalf("thought operations after expiry restore = %d, want %d", afterOps, beforeOps)
	}
}
