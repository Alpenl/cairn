// translation_stall_test.go 覆盖 upsertPendingTranslationSQL 的 ON CONFLICT
// 谓词——「在途行超过停滞阈值后可重新排期」这一分支。
//
// 为什么必须在真实 PG 上测：该分支写在 SQL 的 WHERE 里，判定的是
// `updated_at < NOW() - $12::interval`。service 层的 fake 只能复刻它的形状，
// 复刻本身就可能与生产漂移——上一版修复正是只改了 service 层、SQL 谓词没跟上，
// 而当时的 fake 无条件返回 scheduled=true，把这个分叉完全掩盖了。
package dbintegration

import (
	"crypto/sha256"
	"fmt"
	"testing"
	"time"

	"webtag/internal/model"
	"webtag/internal/repository"
)

// 生产由文本派生哈希（internal/service/translation.go 的 sha256.Sum256），
// 测试照做——塞一个长度合法的十六进制魔数只能满足
// chk_link_translations_source_hash 的 64 字符约束，却掩盖了「哈希必须与
// 文本对应」这个真正的契约。
const sourceText = "hello"

// TestSchedulePending_StalledInFlightRowIsRescheduled 是 HIGH-1 的持久层回归。
//
// 卡死场景：River 判 discard 后 RecordDiscard 失败，行永久停在 processing。
// 若 ON CONFLICT 谓词不含停滞分支，upsert 影响 0 行，调用方落回
// findTranslationByIdentity 拿到同一条卡死行、scheduled=false、不入队——用户
// 无论重试多少次都看到「翻译中」。
func TestSchedulePending_StalledInFlightRowIsRescheduled(t *testing.T) {
	pool := StartPostgres(t)
	linkRepo := repository.NewPGXLinkRepository(pool)
	translations := repository.NewPGXTranslationRepository(pool)
	queue := newRiverQueue(t, pool, newRecordingProcessor(pool))
	ctx := t.Context()

	link, _, err := linkRepo.SubmitNew(ctx, repository.CreateLinkParams{
		URL:        "https://example.com/stalled-translation",
		SourceKind: "url",
		SourceKey:  "src-stalled-tr",
		Status:     model.LinkStatusDone,
	})
	if err != nil {
		t.Fatalf("SubmitNew: %v", err)
	}

	params := repository.UpsertTranslationParams{
		LinkID:                  link.ID,
		Scope:                   model.TranslationScopeSelection,
		BlockKey:                "summary",
		StartOffset:             0,
		EndOffset:               5,
		SourceText:              sourceText,
		SourceFormat:            model.TranslationFormatPlain,
		TargetLanguage:          model.TranslationTargetChinese,
		SourceHash:              fmt.Sprintf("%x", sha256.Sum256([]byte(sourceText))),
		AllowExistingReschedule: true,
		StallAfter:              time.Hour,
	}

	// 首次排期：全新插入。
	first, scheduled, err := translations.SchedulePending(ctx, params, queue.EnqueueTranslationTx)
	if err != nil {
		t.Fatalf("首次 SchedulePending: %v", err)
	}
	if !scheduled || first == nil {
		t.Fatalf("首次排期未生效 scheduled=%v", scheduled)
	}
	if first.AttemptGeneration != 1 || first.CurrentRiverJobID == nil {
		t.Fatalf("首次 attempt identity = generation %d current %v", first.AttemptGeneration, first.CurrentRiverJobID)
	}

	// 模拟卡死：置为 processing 且时间戳退回到阈值之外。
	if _, err := pool.Exec(ctx,
		`UPDATE link_translations SET status = 'processing', updated_at = NOW() - INTERVAL '3 hours'
		 WHERE id = $1`, first.ID); err != nil {
		t.Fatalf("模拟卡死状态: %v", err)
	}

	// 再次排期：停滞分支应放行。
	again, scheduled, err := translations.SchedulePending(ctx, params, queue.EnqueueTranslationTx)
	if err != nil {
		t.Fatalf("重新 SchedulePending: %v", err)
	}
	if !scheduled {
		t.Fatal("停滞行未被重新排期——ON CONFLICT 谓词缺少停滞分支，用户会永远看到「翻译中」")
	}
	if again.Status != model.TranslationStatusPending {
		t.Fatalf("status = %q, want pending", again.Status)
	}
	if again.AttemptGeneration != first.AttemptGeneration+1 || again.CurrentRiverJobID == nil ||
		*again.CurrentRiverJobID == *first.CurrentRiverJobID {
		t.Fatalf("重新排期 attempt identity = generation %d current %v; first=%+v",
			again.AttemptGeneration, again.CurrentRiverJobID, first)
	}

	var firstJobState string
	if err := pool.QueryRow(ctx,
		`SELECT state FROM river_job WHERE id = $1`,
		*first.CurrentRiverJobID,
	).Scan(&firstJobState); err != nil {
		t.Fatalf("读取旧 River attempt: %v", err)
	}
	if firstJobState != "cancelled" {
		t.Fatalf("旧 River attempt state = %q, want cancelled", firstJobState)
	}

	var activeCount int
	var activeJobID int64
	if err := pool.QueryRow(ctx,
		`SELECT COUNT(*), COALESCE(MIN(id), 0)
		 FROM river_job
		 WHERE kind IN ('translate_link_content', 'translate_link_v2')
		   AND args->>'translation_id' = $1
		   AND state IN ('available', 'pending', 'retryable', 'running', 'scheduled')`,
		again.ID.String(),
	).Scan(&activeCount, &activeJobID); err != nil {
		t.Fatalf("读取 active River attempts: %v", err)
	}
	if activeCount != 1 || activeJobID != *again.CurrentRiverJobID {
		t.Fatalf("active River attempts = count %d id %d, want count 1 id %d",
			activeCount, activeJobID, *again.CurrentRiverJobID)
	}
}

// TestSchedulePending_FreshInFlightRowIsNotRescheduled 反向断言：仍在跑的行
// 不得被重复排期，否则并发请求会把同一条翻译反复入队。
func TestSchedulePending_FreshInFlightRowIsNotRescheduled(t *testing.T) {
	pool := StartPostgres(t)
	linkRepo := repository.NewPGXLinkRepository(pool)
	translations := repository.NewPGXTranslationRepository(pool)
	queue := newRiverQueue(t, pool, newRecordingProcessor(pool))
	ctx := t.Context()

	link, _, err := linkRepo.SubmitNew(ctx, repository.CreateLinkParams{
		URL:        "https://example.com/fresh-translation",
		SourceKind: "url",
		SourceKey:  "src-fresh-tr",
		Status:     model.LinkStatusDone,
	})
	if err != nil {
		t.Fatalf("SubmitNew: %v", err)
	}

	params := repository.UpsertTranslationParams{
		LinkID:                  link.ID,
		Scope:                   model.TranslationScopeSelection,
		BlockKey:                "summary",
		StartOffset:             0,
		EndOffset:               5,
		SourceText:              sourceText,
		SourceFormat:            model.TranslationFormatPlain,
		TargetLanguage:          model.TranslationTargetChinese,
		SourceHash:              fmt.Sprintf("%x", sha256.Sum256([]byte(sourceText))),
		AllowExistingReschedule: true,
		StallAfter:              time.Hour,
	}

	first, _, err := translations.SchedulePending(ctx, params, queue.EnqueueTranslationTx)
	if err != nil {
		t.Fatalf("首次 SchedulePending: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE link_translations SET status = 'processing', updated_at = NOW() WHERE id = $1`,
		first.ID); err != nil {
		t.Fatalf("置为在途: %v", err)
	}

	_, scheduled, err := translations.SchedulePending(ctx, params, nil)
	if err != nil {
		t.Fatalf("重复 SchedulePending: %v", err)
	}
	if scheduled {
		t.Fatal("刚开始跑的翻译被重复排期——并发请求会把同一条反复入队")
	}
	retained, err := translations.GetByID(ctx, first.ID)
	if err != nil || retained == nil || retained.AttemptGeneration != first.AttemptGeneration ||
		retained.CurrentRiverJobID == nil || *retained.CurrentRiverJobID != *first.CurrentRiverJobID {
		t.Fatalf("fresh attempt changed: retained=%+v err=%v first=%+v", retained, err, first)
	}
}
