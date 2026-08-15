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

const classificationRuleColumns = "id, host, identity_adapter, path_prefix, target_kind, enabled, revision, created_at, updated_at"

const archiveV2ClassificationRuleSQL = `SELECT jsonb_build_object(
	'id',r.id,'host',r.host,'identity_adapter',r.identity_adapter,
		'path_prefix',r.path_prefix,'target_kind',r.target_kind,'enabled',r.enabled,
		'revision',r.revision,'created_at',r.created_at,'updated_at',r.updated_at
	)::text FROM library_classification_rules r
	ORDER BY r.host, r.path_prefix NULLS FIRST, r.id`

type PGXClassificationRuleRepository struct{ db database.Querier }

func NewPGXClassificationRuleRepository(db database.Querier) *PGXClassificationRuleRepository {
	return &PGXClassificationRuleRepository{db: db}
}

func (r *PGXClassificationRuleRepository) ListClassificationRules(ctx context.Context) ([]model.LibraryClassificationRule, error) {
	rows, err := r.db.Query(ctx, "SELECT "+classificationRuleColumns+" FROM library_classification_rules ORDER BY host, path_prefix NULLS FIRST, id")
	if err != nil {
		return nil, fmt.Errorf("list classification rules: %w", err)
	}
	defer rows.Close()
	out := []model.LibraryClassificationRule{}
	for rows.Next() {
		rule, err := scanClassificationRule(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, rule)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate classification rules: %w", err)
	}
	return out, nil
}

func (r *PGXClassificationRuleRepository) StreamArchiveV2Rules(ctx context.Context, yield func([]byte) error) error {
	rows, err := r.db.Query(ctx, archiveV2ClassificationRuleSQL)
	if err != nil {
		return fmt.Errorf("stream archive classification rules: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return fmt.Errorf("scan archive classification rule: %w", err)
		}
		if err := yield([]byte(raw)); err != nil {
			return err
		}
	}
	return rows.Err()
}

func (r *PGXClassificationRuleRepository) CreateClassificationRule(ctx context.Context, p CreateClassificationRuleParams) (*model.LibraryClassificationRule, error) {
	row := r.db.QueryRow(ctx, "INSERT INTO library_classification_rules (host, identity_adapter, path_prefix, target_kind, enabled) VALUES ($1,$2,$3,$4,$5) RETURNING "+classificationRuleColumns, p.Host, p.IdentityAdapter, p.PathPrefix, p.TargetKind, p.Enabled)
	rule, err := scanClassificationRule(row)
	if err != nil {
		return nil, fmt.Errorf("create classification rule: %w", err)
	}
	return &rule, nil
}

func (r *PGXClassificationRuleRepository) UpdateClassificationRule(ctx context.Context, p UpdateClassificationRuleParams) (*model.LibraryClassificationRule, error) {
	row := r.db.QueryRow(ctx, "UPDATE library_classification_rules SET host=COALESCE($1,host), identity_adapter=CASE WHEN $2 THEN $3 ELSE identity_adapter END, path_prefix=CASE WHEN $4 THEN $5 ELSE path_prefix END, target_kind=COALESCE($6,target_kind), enabled=COALESCE($7,enabled), revision=revision+1, updated_at=NOW() WHERE id=$8 AND revision=$9 RETURNING "+classificationRuleColumns, p.Host, p.IdentityAdapter != nil, optionalStringValue(p.IdentityAdapter), p.PathPrefix != nil, optionalStringValue(p.PathPrefix), p.TargetKind, p.Enabled, p.ID, p.Revision)
	rule, err := scanClassificationRule(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrRevisionConflict
	}
	if err != nil {
		return nil, fmt.Errorf("update classification rule: %w", err)
	}
	return &rule, nil
}

func (r *PGXClassificationRuleRepository) DeleteClassificationRule(ctx context.Context, id uuid.UUID, revision int64) (bool, error) {
	tag, err := r.db.Exec(ctx, "DELETE FROM library_classification_rules WHERE id=$1 AND revision=$2", id, revision)
	if err != nil {
		return false, fmt.Errorf("delete classification rule: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

func scanClassificationRule(row interface{ Scan(...any) error }) (model.LibraryClassificationRule, error) {
	var rule model.LibraryClassificationRule
	var adapter, prefix pgtype.Text
	err := row.Scan(&rule.ID, &rule.Host, &adapter, &prefix, &rule.TargetKind, &rule.Enabled, &rule.Revision, &rule.CreatedAt, &rule.UpdatedAt)
	rule.IdentityAdapter, rule.PathPrefix = textPointer(adapter), textPointer(prefix)
	return rule, err
}

func optionalStringValue(value **string) any {
	if value == nil {
		return nil
	}
	if *value == nil {
		return nil
	}
	return **value
}

var _ ClassificationRuleStore = (*PGXClassificationRuleRepository)(nil)
var _ ArchiveV2RuleReader = (*PGXClassificationRuleRepository)(nil)
