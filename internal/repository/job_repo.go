package repository

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"webtag/internal/database"
	"webtag/internal/model"
)

const jobSelectColumns = "id, link_id, status, error_msg, created_at, updated_at, expected_metadata_revision"

// parseJobsPerLinkRetention caps the parse_jobs history kept per link.
// Wave 14 L-4: every Refresh and re-submit appends a row, so a link that
// gets retried daily would accumulate 365 rows/year with no upper bound
// — only the latest is read by latestJobByLinkSQL. Pruning happens
// in-line on the next insert (pay-as-you-go GC) so there is no separate
// cron, no scheduler dependency, and no chance of an idle link silently
// missing its window. 20 covers any realistic UI-driven debugging
// session (the user can see the last 20 attempts) while keeping the
// per-link footprint bounded at O(1).
const parseJobsPerLinkRetention = 20

const (
	// insertJobSQL inserts the new pending job and prunes older history
	// for the same link beyond parseJobsPerLinkRetention rows in a single
	// CTE. PostgreSQL evaluates all CTE branches against the same
	// snapshot, so the DELETE cannot see the just-inserted row — we
	// keep the top (cap-1) existing rows + the new row = cap total
	// after the statement commits. Modifying CTEs (DELETE without
	// RETURNING) execute even when unreferenced from the final SELECT,
	// which is the contract we lean on here.
	insertJobSQL = `WITH inserted AS (
	    INSERT INTO parse_jobs (link_id, status, created_at, updated_at, expected_metadata_revision)
	    VALUES ($1, 'pending', NOW(), NOW(), (SELECT metadata_revision FROM links WHERE id = $1))
    RETURNING ` + jobSelectColumns + `
), ranked AS (
    SELECT id, ROW_NUMBER() OVER (ORDER BY created_at DESC, id DESC) AS rn
    FROM parse_jobs
    WHERE link_id = $1
), pruned AS (
    DELETE FROM parse_jobs
    WHERE link_id = $1 AND id IN (SELECT id FROM ranked WHERE rn >= $2)
)
SELECT ` + jobSelectColumns + ` FROM inserted`
	updateJobStateSQL  = "UPDATE parse_jobs SET status = $2, error_msg = $3, updated_at = NOW() WHERE id = $1"
	getJobByIDSQL      = "SELECT " + jobSelectColumns + " FROM parse_jobs WHERE id = $1"
	listJobsByIDsSQL   = "SELECT " + jobSelectColumns + " FROM parse_jobs WHERE id = ANY($1::uuid[]) ORDER BY created_at DESC, id DESC"
	latestJobByLinkSQL = "SELECT " + jobSelectColumns + " FROM parse_jobs WHERE link_id = $1 ORDER BY created_at DESC LIMIT 1"
)

// PGXJobRepository 是 JobStore 的 PG 实现，包装 parse_jobs 表的 CRUD 与启动恢复操作。
type PGXJobRepository struct {
	db database.Querier
}

// NewPGXJobRepository 用给定的 Querier 构造 PGXJobRepository。
func NewPGXJobRepository(db database.Querier) *PGXJobRepository {
	return &PGXJobRepository{db: db}
}

// Create 为指定 link 插入一条 pending 的 parse_jobs 行；
// 底层 CTE 会同时裁剪超过 parseJobsPerLinkRetention 的历史行，保证每个 link 的任务历史 O(1)。
func (r *PGXJobRepository) Create(ctx context.Context, linkID uuid.UUID) (*model.ParseJob, error) {
	row := r.db.QueryRow(ctx, insertJobSQL, linkID, parseJobsPerLinkRetention)
	job, err := scanJob(row)
	if err != nil {
		return nil, fmt.Errorf("insert job: %w", err)
	}
	return &job, nil
}

// GetByID 按主键查找 parse_job，未命中返回 (nil, nil)。
func (r *PGXJobRepository) GetByID(ctx context.Context, id uuid.UUID) (*model.ParseJob, error) {
	row := r.db.QueryRow(ctx, getJobByIDSQL, id)
	job, err := scanJob(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get job by id: %w", err)
	}
	return &job, nil
}

// ListByIDs returns all matching parse_jobs for the supplied ids ordered by
// recency so clients can process one batch response deterministically.
func (r *PGXJobRepository) ListByIDs(ctx context.Context, ids []uuid.UUID) ([]model.ParseJob, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	rows, err := r.db.Query(ctx, listJobsByIDsSQL, ids)
	if err != nil {
		return nil, fmt.Errorf("list jobs by ids: %w", err)
	}
	defer rows.Close()

	jobs, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (model.ParseJob, error) {
		return scanJob(row)
	})
	if err != nil {
		return nil, fmt.Errorf("collect jobs by ids: %w", err)
	}
	return jobs, nil
}

// GetLatestByLinkID 返回某 link 最近一次的 parse_job（按 created_at 倒序取首条），未命中返回 (nil, nil)。
func (r *PGXJobRepository) GetLatestByLinkID(ctx context.Context, linkID uuid.UUID) (*model.ParseJob, error) {
	row := r.db.QueryRow(ctx, latestJobByLinkSQL, linkID)
	job, err := scanJob(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get latest job by link id: %w", err)
	}
	return &job, nil
}

// UpdateState 切换 parse_jobs.status / error_msg，命中 0 行时返回 ErrNotFound。
func (r *PGXJobRepository) UpdateState(ctx context.Context, params UpdateJobStateParams) error {
	tag, err := r.db.Exec(ctx, updateJobStateSQL, params.ID, params.Status, params.ErrorMsg)
	if err != nil {
		return fmt.Errorf("update job state: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func scanJob(row rowScanner) (model.ParseJob, error) {
	var job model.ParseJob
	var errorMsg pgtype.Text
	err := row.Scan(
		&job.ID,
		&job.LinkID,
		&job.Status,
		&errorMsg,
		&job.CreatedAt,
		&job.UpdatedAt,
		&job.ExpectedMetadataRevision,
	)
	if err != nil {
		return job, err
	}

	job.ErrorMsg = textPointer(errorMsg)
	return job, nil
}
