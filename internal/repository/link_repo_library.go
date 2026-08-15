package repository

import (
	"context"
	"errors"
	"fmt"

	"webtag/internal/database"
	"webtag/internal/model"

	"github.com/jackc/pgx/v5"
)

// updateLibraryClassificationSQL 的 WHERE 带锁定守卫：已被用户锁定的链接，
// 只接受非自动来源（user / migration）的改写，自动分类一律让路。
//
// 这是持久层兜底，不是唯一防线——decideFinalLibrary 已在决策层让路。两层都要，
// 因为它们防的是不同的东西：决策层保证解析路径算出正确结论，这里保证任何绕过
// 决策层的写入（未来的新调用方、批处理脚本）也无法抹掉用户裁决。
//
// 谓词写成 `$3 <> 'auto'` 而非 `library_kind_locked = false`：后者会把用户自己
// 的改判也挡在门外——用户有权改变主意，锁只对系统生效。
const updateLibraryClassificationSQL = `UPDATE links
SET library_kind = $2, library_kind_source = $3, library_kind_locked = $4,
	requested_library_kind = CASE WHEN $3 = 'user' THEN $2 ELSE requested_library_kind END,
	requested_library_kind_source = CASE WHEN $3 = 'user' THEN 'user' ELSE requested_library_kind_source END,
	predicted_library_kind = $5, classification_confidence = $6,
    classification_reason = $7, classification_explanation = $8,
    classifier_version = $9, updated_at = NOW()
WHERE id = $1
	AND deleted_at IS NULL
  AND (library_kind_locked = false OR $3 <> 'auto')`

const updateLibraryClassificationForCompletionSQL = updateLibraryClassificationSQL + `
  AND requested_library_kind = $10 AND requested_library_kind_source = $11`

func (r *PGXLinkRepository) UpdateLibraryClassification(ctx context.Context, params UpdateLibraryClassificationParams) error {
	return updateLibraryClassificationOn(ctx, r.db, params)
}

func updateLibraryClassificationOn(ctx context.Context, db database.Querier, params UpdateLibraryClassificationParams) error {
	if params.Kind != model.LibraryKindReading && params.Kind != model.LibraryKindSite {
		return fmt.Errorf("update library classification: invalid kind %q", params.Kind)
	}
	if err := ValidateLibraryKindSource(params.Source); err != nil {
		return err
	}
	tag, err := db.Exec(ctx, updateLibraryClassificationSQL, params.ID, params.Kind, params.Source, params.Locked, params.PredictedKind, params.Confidence, params.Reason, params.Explanation, params.ClassifierVersion)
	if err != nil {
		return fmt.Errorf("update library classification: %w", err)
	}
	if tag.RowsAffected() == 0 {
		// 零行有两种原因，必须分清：行不存在，或行存在但被用户锁定而本次是
		// 自动分类。后者不是错误状态——它正是锁该起的作用——但如果一律报
		// ErrNotFound，排查时会往「数据丢了」的方向找。
		var locked bool
		scanErr := db.QueryRow(ctx, selectLibraryKindLockedSQL, params.ID).Scan(&locked)
		switch {
		case errors.Is(scanErr, pgx.ErrNoRows):
			// 行确实不存在。
			return ErrNotFound
		case scanErr != nil:
			// 连接断开 / 超时 / 权限等真实故障。整段逻辑的立意就是「别把排查
			// 引向数据丢了」，把 DB 故障也吞成 ErrNotFound 正好犯同一个错。
			return fmt.Errorf("update library classification: probe lock state: %w", scanErr)
		}
		if locked {
			return fmt.Errorf("%w: link %s is user-locked; automatic classification cannot overwrite it", ErrLibraryKindLocked, params.ID)
		}
		return ErrNotFound
	}
	return nil
}

func updateLibraryClassificationForCompletionOn(
	ctx context.Context,
	db database.Querier,
	params UpdateLibraryClassificationParams,
	expectedKind model.RequestedLibraryKind,
	expectedSource model.RequestedLibraryKindSource,
) error {
	if params.Kind != model.LibraryKindReading && params.Kind != model.LibraryKindSite {
		return fmt.Errorf("update library classification: invalid kind %q", params.Kind)
	}
	if err := ValidateLibraryKindSource(params.Source); err != nil {
		return err
	}
	expectedKind, expectedSource = normalizeRequestedLibraryIntent(expectedKind, expectedSource)
	tag, err := db.Exec(ctx, updateLibraryClassificationForCompletionSQL, params.ID, params.Kind, params.Source, params.Locked, params.PredictedKind, params.Confidence, params.Reason, params.Explanation, params.ClassifierVersion, expectedKind, expectedSource)
	if err != nil {
		return fmt.Errorf("update library classification for completion: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return classifyLibraryCompletionMiss(ctx, db, params, expectedKind, expectedSource)
	}
	return nil
}

const selectLibraryIntentForCompletionSQL = `SELECT requested_library_kind, requested_library_kind_source, library_kind_locked FROM links WHERE id = $1 AND deleted_at IS NULL`

func classifyLibraryCompletionMiss(
	ctx context.Context,
	db database.Querier,
	params UpdateLibraryClassificationParams,
	expectedKind model.RequestedLibraryKind,
	expectedSource model.RequestedLibraryKindSource,
) error {
	var currentKind model.RequestedLibraryKind
	var currentSource model.RequestedLibraryKindSource
	var locked bool
	err := db.QueryRow(ctx, selectLibraryIntentForCompletionSQL, params.ID).Scan(&currentKind, &currentSource, &locked)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("update library classification for completion: probe intent: %w", err)
	}
	if currentKind != expectedKind || currentSource != expectedSource {
		return fmt.Errorf("%w: expected %s/%s, found %s/%s", ErrLibraryIntentChanged, expectedKind, expectedSource, currentKind, currentSource)
	}
	if locked && params.Source == model.LibraryKindSourceAuto {
		return ErrLibraryKindLocked
	}
	return ErrNotFound
}

const selectLibraryKindLockedSQL = `SELECT library_kind_locked FROM links WHERE id = $1 AND deleted_at IS NULL`

var _ LibraryClassificationWriter = (*PGXLinkRepository)(nil)

// ValidateLibraryKindSource 是 library_kind_source 的白名单，对应 DB 约束
// chk_links_library_kind_source。导出供 repotest 的 fake 复用。
//
// 两条终态路径的强制点不同：reading 侧经本文件的 updateLibraryClassificationOn
// 在 Go 层拒绝，site 侧走 completeSiteLinkSQL 内联写入、只由 DB 约束拒绝。取值
// 集合相同，故共用同一个校验。
func ValidateLibraryKindSource(source model.LibraryKindSource) error {
	switch source {
	case model.LibraryKindSourceAuto, model.LibraryKindSourceUser, model.LibraryKindSourceMigration:
		return nil
	default:
		return fmt.Errorf("update library classification: invalid source %q", source)
	}
}
