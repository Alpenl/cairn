package migrate

import (
	"os"
	"strings"
	"testing"
)

func TestIntegrityRepairMigrationPinsUpgradeContracts(t *testing.T) {
	t.Parallel()

	step := migrationStepByID(t, integrityRepairMigrationID)
	if step.Manual || step.NonTransactional {
		t.Fatalf("integrity repair flags = Manual:%v NonTransactional:%v, want automatic transactional", step.Manual, step.NonTransactional)
	}
	joined := strings.ToLower(strings.Join(step.SQL, "\n"))
	for _, want := range []string{
		"add column if not exists owner_token text",
		"alter column owner_token set not null",
		"add column if not exists generation bigint",
		"alter column generation set not null",
		"chk_idempotency_keys_generation",
		"add column if not exists feed_managed boolean",
		"alter column feed_managed set not null",
		"save.created_link",
		"link.source_kind = 'subscription'",
		"link.feed_managed is null",
		"attempt.status in ('pending', 'processing')",
		"error_msg = 'link_deleted'",
		"drop constraint if exists concept_merge_proposal_loser_id_fkey",
		"drop constraint if exists concept_merge_proposal_winner_id_fkey",
		"chk_merge_proposal_decision_audit",
		"add column if not exists user_deleted boolean",
		"alter column user_deleted set not null",
		"set user_deleted=true",
		"set target='{}'::jsonb, payload='{}'::jsonb",
		"update public.reader_thought_supersession_events",
		"create trigger trg_reader_scrub_user_deleted_thought_event",
		"create trigger trg_reader_protect_user_deleted_thought_tombstone",
		"'type','user_deleted'",
		"old.user_deleted",
		"review.kind='migration_suggestion' and review.status='pending'",
		"review.payload @> jsonb_build_object('content_revision',link.content_revision)",
		"link.predicted_library_kind='site'",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("integrity repair missing %q", want)
		}
	}
	if strings.Contains(joined, "attempt.status not in") {
		t.Fatal("parse repair must name only non-terminal statuses")
	}
}

func TestFreshSchemaPinsSharedIntegrityContracts(t *testing.T) {
	t.Parallel()

	lowered := strings.ToLower(singleInstallSchemaSQL)
	for _, want := range []string{
		"owner_token text default (gen_random_uuid())::text not null",
		"generation bigint default 1 not null",
		"constraint chk_idempotency_keys_generation check ((generation >= 1))",
		"feed_managed boolean default false not null",
		"user_deleted boolean default false not null",
		"constraint chk_reader_thoughts_user_deleted_content",
		"create function public.reader_enforce_user_deleted_thought() returns trigger",
		"create trigger trg_reader_scrub_user_deleted_thought_op",
		"constraint chk_merge_proposal_decision_audit",
		"btrim(decided_by) <> ''::text",
	} {
		if !strings.Contains(lowered, want) {
			t.Errorf("fresh schema missing %q", want)
		}
	}
	for _, forbidden := range []string{
		"concept_merge_proposal_loser_id_fkey",
		"concept_merge_proposal_winner_id_fkey",
	} {
		if strings.Contains(lowered, forbidden) {
			t.Errorf("fresh schema retains lifecycle-owned proposal FK %q", forbidden)
		}
	}

	raw, err := os.ReadFile("schema.sql")
	if err != nil {
		t.Fatalf("read schema.sql: %v", err)
	}
	snapshot := strings.ToLower(string(raw))
	for _, want := range []string{
		"owner_token text",
		"feed_managed boolean",
		"user_deleted boolean",
		"chk_merge_proposal_decision_audit",
		"btrim(decided_by) <> ''::text",
	} {
		if !strings.Contains(snapshot, want) {
			t.Errorf("schema.sql missing %q", want)
		}
	}
	for _, forbidden := range []string{"concept_merge_proposal_loser_id_fkey", "concept_merge_proposal_winner_id_fkey"} {
		if strings.Contains(snapshot, forbidden) {
			t.Errorf("schema.sql retains lifecycle-owned proposal FK %q", forbidden)
		}
	}
}

func TestConceptMergeAuditRepairMigrationPinsDurableOwnership(t *testing.T) {
	t.Parallel()

	step := migrationStepByID(t, conceptMergeAuditRepairMigrationID)
	if step.Manual || step.NonTransactional {
		t.Fatalf("concept audit repair flags = Manual:%v NonTransactional:%v, want automatic transactional", step.Manual, step.NonTransactional)
	}
	joined := strings.ToLower(strings.Join(step.SQL, "\n"))
	for _, want := range []string{
		"drop constraint if exists concept_merge_proposal_loser_id_fkey",
		"drop constraint if exists concept_merge_proposal_winner_id_fkey",
		"drop constraint if exists chk_merge_proposal_decision_audit",
		"btrim(decided_by) = ''",
		"btrim(decided_by) <> ''",
		"decided_at = coalesce(decided_at, created_at)",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("concept audit repair missing %q", want)
		}
	}
	if strings.Contains(joined, "set winner_id = null") || strings.Contains(joined, "set loser_id = null") {
		t.Fatal("concept audit repair must preserve original winner/loser UUID snapshots")
	}
}

func TestHistoricalRepairMigrationReplaysRollingUpgradeCleanup(t *testing.T) {
	t.Parallel()

	step := migrationStepByID(t, historicalRepairMigrationID)
	if step.Manual || step.NonTransactional {
		t.Fatalf("historical repair flags = Manual:%v NonTransactional:%v, want automatic transactional", step.Manual, step.NonTransactional)
	}
	joined := strings.ToLower(strings.Join(step.SQL, "\n"))
	for _, want := range []string{
		"link.status='done'",
		"link.deleted_at is null",
		"link.library_kind='reading'",
		"link.library_kind_source='migration'",
		"not link.library_kind_locked",
		"link.classifier_version='historical-migration-v1'",
		"link.predicted_library_kind='site'",
		"translation.link_id=link.id",
		"review.payload @> jsonb_build_object('content_revision',link.content_revision)",
		"set classifier_version=null",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("historical repair missing %q", want)
		}
	}
}

func TestLifecycleRepairMigrationReplaysDeletedAndFeedOwnershipCleanup(t *testing.T) {
	t.Parallel()

	step := migrationStepByID(t, lifecycleRepairMigrationID)
	if step.Manual || step.NonTransactional {
		t.Fatalf("lifecycle repair flags = Manual:%v NonTransactional:%v, want automatic transactional", step.Manual, step.NonTransactional)
	}
	if got := strings.ToLower(strings.TrimSpace(step.SQL[0])); got != "select public.lock_library_feed_revisions()" {
		t.Fatalf("lifecycle repair first statement = %q, want representation revision prelock", got)
	}
	joined := strings.ToLower(strings.Join(step.SQL, "\n"))
	for _, want := range []string{
		"select public.lock_library_feed_revisions()",
		"create table if not exists public.feed_lifecycle_repair_audit",
		"feed_lifecycle_repair_audit_pkey",
		"repaired_feed_managed_orphan",
		"ambiguous_subscription_orphan",
		"save.created_link",
		"link.feed_managed is null",
		"not exists (",
		"attempt.status in ('pending','processing')",
		"error_msg='link_deleted'",
		"from public.river_job as job",
		"'public.river_control'",
		"cancel_attempted_at",
		"job.kind='parse_link'",
		"job.kind in ('translate_link_content','translate_link_v2')",
		"update public.link_translations as attempt",
		"current_river_job_id=null",
		"for update",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("lifecycle repair missing %q", want)
		}
	}
	if strings.Contains(joined, "attempt.status not in") {
		t.Fatal("lifecycle parse repair must preserve terminal history")
	}
	// 「feed_managed 且无 save」无法证明是孤儿：保留策略裁掉一条已读的已保存
	// feed item 后，级联删除 save 行会让合法保存的 Link 落到完全相同的状态。
	// 该修复只允许记录审计，不允许软删，否则会静默销毁用户保存的正文。
	if strings.Contains(joined, "set deleted_at=current_timestamp") {
		t.Fatal("lifecycle repair must not soft-delete unproven feed_managed orphans")
	}
	if strings.Contains(joined, "'action','soft_delete'") {
		t.Fatal("lifecycle repair must not classify feed_managed orphans as soft-deleted")
	}
}

func migrationStepByID(t *testing.T, id string) Step {
	t.Helper()
	for _, step := range Steps() {
		if step.ID == id {
			return step
		}
	}
	t.Fatalf("migration %q not found", id)
	return Step{}
}
