package repository

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"webtag/internal/model"
)

// 「保存原文」的读写刻意独立于 scanLink / UpdateAnalysis：content 是按需保存的大
// 文本，只在详情页单条读取，不进列表/通用扫描路径，避免给每次 list 都拖上整篇正文。

// UpdateContentIfCurrent writes canonical content only while the link is still
// the same completed revision the caller read and no other Save has won. A
// false result is an expected optimistic-concurrency miss, not a repository
// error; the caller can reuse a concurrent winner or return a retryable 409.
// Saving derived content deliberately does not bump links.updated_at: that
// timestamp identifies the parsed source revision used by CAS checks.
//
// It DOES bump content_revision. The two tokens track different things and
// both are load-bearing:
//
//   - updated_at   — which parsed source revision this content was derived from
//   - content_revision — which generation of saved content is on the row
//
// Until this bump existed, content_revision moved only on the conversion and
// historical-migration paths (which null out content), so replacing a saved
// original left it untouched. That made the column's name a lie and broke the
// two things that read it as "the content you saw is still the content
// that's there":
//
//   - Reader caches saved originals under (linkId, content_revision) and, since
//     PF3/PF4, serves a cache hit without revalidating. A constant revision
//     meant a client that had cached the old text never learned it changed —
//     permanently, and across a refresh, since the entry is persisted to
//     IndexedDB. Only the writing client invalidated its own copy.
//   - Site conversion uses ExpectedContentRevision as its CAS token, and the
//     preview it is issued from reports SavedOriginal / Destructive — i.e. what
//     the conversion would delete. Saving an original between preview and
//     execute silently invalidated that report. Now it conflicts, which is the
//     correct answer.
//
// RF5A extends the same rule to every saved-body writer: requeue and
// site-complete clears, conversion, and historical migration all advance the
// generation in the SQL/transaction that changes content, content_document,
// content_format, or their existence. has_content remains the authoritative
// existence signal for Reader rendering, but it is not a substitute for this
// cache/CAS identity.
//
// 返回值里的 revision 是自增后的新代次，通过 RETURNING 与写入同一条语句取回：
// 调用方要把它交给客户端（见 dto.LinkContentResponse.ContentRevision），而事后
// 补一次 SELECT 既多一个往返，也有被下一次写入抢先的窗口。CAS 未命中时
// RETURNING 不产出行，revision 为 0 且 stored 为 false。
func (r *PGXLinkRepository) UpdateContentIfCurrent(
	ctx context.Context,
	id uuid.UUID,
	expectedUpdatedAt time.Time,
	content model.SavedContent,
) (int64, bool, error) {
	// 两项阅读计数与正文写在**同一条语句**里：分开写就有窗口让它们与正文
	// 不一致，而不一致的症状是折叠状态下的阅读时长与展开后对不上。
	var revision int64
	err := r.db.QueryRow(ctx,
		`UPDATE links
		 SET content = $1, content_document = $2, content_format = $3,
		     content_cjk_chars = $4, content_words = $5,
		     content_source = 'fetched',
		     content_revision = content_revision + 1
		 WHERE id = $6
		   AND updated_at = $7
		   AND status = 'done'
		   AND deleted_at IS NULL
		   AND content IS NULL
		 RETURNING content_revision`,
		content.Text, content.Document, content.Format, content.CJKChars, content.Words,
		id, expectedUpdatedAt).Scan(&revision)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("update link content if current: %w", err)
	}
	return revision, true, nil
}

// ReplaceContentIfCurrentWithRevision is the production PUT CAS. The parsed
// source timestamp and the saved-content generation are both observed before
// the network fetch starts; requiring both at write time prevents a slow PUT
// from overwriting a user PATCH that completed during that fetch.
func (r *PGXLinkRepository) ReplaceContentIfCurrentWithRevision(
	ctx context.Context,
	id uuid.UUID,
	expectedUpdatedAt time.Time,
	expectedContentRevision int64,
	content model.SavedContent,
) (int64, bool, error) {
	var revision int64
	err := r.db.QueryRow(ctx,
		`UPDATE links
		 SET content = $1, content_document = $2, content_format = $3,
		     content_cjk_chars = $4, content_words = $5,
		     content_source = 'fetched',
		     content_revision = content_revision + 1
		 WHERE id = $6
		   AND updated_at = $7
		   AND content_revision = $8
		   AND status = 'done'
		   AND deleted_at IS NULL
		 RETURNING content_revision`,
		content.Text, content.Document, content.Format, content.CJKChars, content.Words,
		id, expectedUpdatedAt, expectedContentRevision).Scan(&revision)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("replace link content if current revision: %w", err)
	}
	return revision, true, nil
}

// EditContentIfRevision replaces a saved snapshot authored by the user. The
// revision predicate is the complete optimistic-concurrency boundary: a
// stale request cannot change the body, counts, source marker, or revision.
func (r *PGXLinkRepository) EditContentIfRevision(
	ctx context.Context,
	id uuid.UUID,
	expectedRevision int64,
	content model.SavedContent,
) (int64, bool, error) {
	var revision int64
	err := r.db.QueryRow(ctx,
		`UPDATE links
		 SET content = $1, content_document = $2, content_format = $3,
		     content_cjk_chars = $4, content_words = $5,
		     content_source = 'user',
		     content_revision = content_revision + 1
		 WHERE id = $6
		   AND status = 'done'
		   AND deleted_at IS NULL
		   AND library_kind IS DISTINCT FROM 'site'
		   AND (content IS NOT NULL OR content_document IS NOT NULL)
		   AND content_revision = $7
		 RETURNING content_revision`,
		content.Text, content.Document, content.Format,
		content.CJKChars, content.Words, id, expectedRevision).Scan(&revision)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("edit link content if revision: %w", err)
	}
	return revision, true, nil
}

// GetContent 读取某条 link 已保存的原文；未保存返回 (nil, nil)，link 不存在也
// 返回 (nil, nil)（调用方已通过 GetByID 校验存在性）。
func (r *PGXLinkRepository) GetContent(ctx context.Context, id uuid.UUID) (*model.SavedContent, error) {
	var (
		content  pgtype.Text
		document pgtype.Text
		format   string
		source   string
		revision int64
	)
	err := r.db.QueryRow(ctx,
		`SELECT content, content_document, content_format, content_source, content_revision FROM links WHERE id = $1 AND deleted_at IS NULL`,
		id).Scan(&content, &document, &format, &source, &revision)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get link content: %w", err)
	}
	if !content.Valid {
		return nil, nil
	}
	var documentPtr *string
	if document.Valid {
		documentPtr = &document.String
	}
	return &model.SavedContent{
		Text:     content.String,
		Document: documentPtr,
		Format:   model.ContentFormat(format),
		Source:   contentSourceFromString(source),
		Revision: revision,
	}, nil
}
