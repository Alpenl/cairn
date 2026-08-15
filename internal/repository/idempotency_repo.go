package repository

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"webtag/internal/database"
)

// IdempotencyRecord mirrors one owned processing generation plus its replay
// response. OwnerToken/Generation form the CAS identity for Store/Delete.
type IdempotencyRecord struct {
	OwnerToken  string
	Generation  int64
	Status      int
	Body        []byte
	ContentType string
	InFlight    bool
	ExpiresAt   time.Time
}

const (
	// acquireIdempotencyKeySQL atomically inserts a new claim or replaces an
	// expired completed response while incrementing generation. An in-flight
	// row is never replaced: after expiry it is durable evidence that the
	// business result may have committed. A conflict produces no RETURNING
	// row, so the caller loads it for bounded wait/replay or fail-closed.
	//
	// 抢占语义靠主键唯一约束 + conditional DO UPDATE 的原子性保证：并发的
	// 多个副本同时申请同一 live key，PG 保证至多一个 owner 执行业务处理。
	acquireIdempotencyKeySQL = `INSERT INTO idempotency_keys
			(key, owner_token, generation, status, body, content_type, in_flight, expires_at)
		VALUES ($1, $2, 1, 0, NULL, '', true, $4)
		ON CONFLICT (key) DO UPDATE SET
			owner_token = EXCLUDED.owner_token,
			generation = idempotency_keys.generation + 1,
			status = 0,
			body = NULL,
			content_type = '',
			in_flight = true,
			expires_at = EXCLUDED.expires_at
			WHERE idempotency_keys.expires_at <= $3
				AND idempotency_keys.in_flight = false
		RETURNING owner_token, generation, status, body, content_type, in_flight, expires_at`

	// getIdempotencyRecordSQL 读既存幂等键行（抢占未命中后用它取回放数据）。
	getIdempotencyRecordSQL = `SELECT owner_token, generation, status, body, content_type, in_flight, expires_at
		FROM idempotency_keys WHERE key = $1`

	// storeIdempotencyResponseSQL 在 handler 跑完后回填录制的响应并解除
	// in_flight，让后续同 key 请求能回放。只更新自己抢占到的行（WHERE key）。
	storeIdempotencyResponseSQL = `UPDATE idempotency_keys
			SET status = $4, body = $5, content_type = $6, in_flight = false, expires_at = $7
			WHERE key = $1 AND owner_token = $2 AND generation = $3 AND in_flight = true`

	// deleteIdempotencyKeySQL 删除一个键。仅用于应用合同明确保证未提交副作用
	// 的瞬态 409/425/429；5xx 必须保留 in-flight 证据。若删除结果无法确认，
	// 后续请求仍会 fail closed。
	deleteIdempotencyKeySQL = `DELETE FROM idempotency_keys
		WHERE key = $1 AND owner_token = $2 AND generation = $3 AND in_flight = true`

	// purgeExpiredIdempotencyKeysSQL 只删除已完成且过期的响应。过期的
	// in-flight 行是结果未知的持久证据，不得清理或重新执行。
	purgeExpiredIdempotencyKeysSQL = `DELETE FROM idempotency_keys
			WHERE expires_at <= $1 AND in_flight = false`
)

// ErrIdempotencyClaimLost means a delayed owner tried to finalize a claim that
// has already been released or superseded by a newer generation.
var ErrIdempotencyClaimLost = errors.New("idempotency claim ownership lost")

// PGXIdempotencyRepository 是 idempotency_keys 表的 PG 仓储实现（参数化查询，
// 防注入）。把幂等缓存从进程内 LRU 下沉到 PG，多副本就绪。
type PGXIdempotencyRepository struct {
	db database.Querier
}

// NewPGXIdempotencyRepository 包装一个 Querier（生产传 *pgxpool.Pool）。
func NewPGXIdempotencyRepository(db database.Querier) *PGXIdempotencyRepository {
	return &PGXIdempotencyRepository{db: db}
}

// Acquire 尝试抢占 key 的处理权。
//
// 返回值 (acquired, existing, err)：
//   - acquired==true, existing!=nil：本次 INSERT/expired completed takeover 成功，
//     existing carries the owner/generation required by Store/Delete CAS.
//   - acquired==false, existing!=nil：key 已被（本副本或其它副本）占用，
//     existing 是既存行；调用方据 existing.InFlight / ExpiresAt 决定回放
//     （已完成且未过期）还是等待（仍在飞，绝不再次执行 handler）。
//   - err!=nil：DB 故障，中间件应 fail closed 返回 503。
func (r *PGXIdempotencyRepository) Acquire(ctx context.Context, key, ownerToken string, now, expiresAt time.Time) (bool, *IdempotencyRecord, error) {
	row := r.db.QueryRow(ctx, acquireIdempotencyKeySQL, key, ownerToken, now, expiresAt)
	if claimed, err := scanIdempotencyRecord(row); err == nil {
		// INSERT 成功，RETURNING 返回了刚插入的占位行 → 抢占成功。
		return true, &claimed, nil
	} else if !errors.Is(err, pgx.ErrNoRows) {
		// 非「无行」错误 = DB 故障，冒泡让中间件 fail closed。
		return false, nil, fmt.Errorf("acquire idempotency key: %w", err)
	}

	// Live conflict 的 conditional update 未执行 → 无 RETURNING 行，回读当前代。
	existing, getErr := r.Get(ctx, key)
	if getErr != nil {
		return false, nil, getErr
	}
	// 极小概率竞态：抢占与回读之间该行被 owner Delete/TTL 清理，existing 为
	// nil。中间件将其视为结果未知并 fail closed，不进入业务 handler。
	return false, existing, nil
}

// Get 读既存幂等键行；未命中返回 (nil, nil)。
func (r *PGXIdempotencyRepository) Get(ctx context.Context, key string) (*IdempotencyRecord, error) {
	row := r.db.QueryRow(ctx, getIdempotencyRecordSQL, key)
	rec, err := scanIdempotencyRecord(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get idempotency record: %w", err)
	}
	return &rec, nil
}

// Store 回填录制的响应、解除 in_flight，并从完成时刻刷新回放 TTL。
func (r *PGXIdempotencyRepository) Store(ctx context.Context, key, ownerToken string, generation int64, status int, body []byte, contentType string, expiresAt time.Time) error {
	tag, err := r.db.Exec(ctx, storeIdempotencyResponseSQL, key, ownerToken, generation, status, body, contentType, expiresAt)
	if err != nil {
		return fmt.Errorf("store idempotency response: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return ErrIdempotencyClaimLost
	}
	return nil
}

// Delete 释放一个已知未提交业务副作用的瞬态响应 claim。
func (r *PGXIdempotencyRepository) Delete(ctx context.Context, key, ownerToken string, generation int64) error {
	tag, err := r.db.Exec(ctx, deleteIdempotencyKeySQL, key, ownerToken, generation)
	if err != nil {
		return fmt.Errorf("delete idempotency key: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return ErrIdempotencyClaimLost
	}
	return nil
}

// PurgeExpired 删除 expires_at <= now 的已完成响应，返回删除行数。过期的
// in-flight 行表示结果未知，必须保留以阻止同 key 重复执行。
func (r *PGXIdempotencyRepository) PurgeExpired(ctx context.Context, now time.Time) (int64, error) {
	tag, err := r.db.Exec(ctx, purgeExpiredIdempotencyKeysSQL, now)
	if err != nil {
		return 0, fmt.Errorf("purge expired idempotency keys: %w", err)
	}
	return tag.RowsAffected(), nil
}

// scanIdempotencyRecord scans the claim identity, replay payload, processing
// state, and expiry. A placeholder body is NULL and maps to a nil byte slice.
func scanIdempotencyRecord(row pgx.Row) (IdempotencyRecord, error) {
	var rec IdempotencyRecord
	err := row.Scan(&rec.OwnerToken, &rec.Generation, &rec.Status, &rec.Body, &rec.ContentType, &rec.InFlight, &rec.ExpiresAt)
	return rec, err
}
