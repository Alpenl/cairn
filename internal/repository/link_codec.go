package repository

import (
	sqldb "database/sql"
	"strings"

	"webtag/internal/jsonx"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
)

func textPointer(v pgtype.Text) *string {
	if !v.Valid {
		return nil
	}
	s := v.String
	return &s
}

func intPointer(v sqldb.NullInt64) *int {
	if !v.Valid {
		return nil
	}
	n := int(v.Int64)
	return &n
}

func uuidPointer(v pgtype.UUID) *uuid.UUID {
	if !v.Valid {
		return nil
	}
	id := uuid.UUID(v.Bytes)
	return &id
}

func nullableUUIDValue(id *uuid.UUID) any {
	if id == nil {
		return nil
	}
	return *id
}

func defaultSourceKind(sourceKind string) string {
	if strings.TrimSpace(sourceKind) == "" {
		return "url"
	}
	return sourceKind
}

func defaultSourceKey(sourceKey, url string) string {
	if strings.TrimSpace(sourceKey) == "" {
		return url
	}
	return sourceKey
}

// marshalJSONB normalizes Go-side typed-nil-empty values into a SQL NULL
// before handing them to pgx so the COALESCE($n::jsonb, ...) clauses in
// updateLinkAnalysisSQL preserve the existing column value instead of
// overwriting it with an empty array / object.
func marshalJSONB(value any) (any, error) {
	if value == nil {
		return nil, nil
	}

	switch typed := value.(type) {
	case []string:
		if len(typed) == 0 {
			return nil, nil
		}
	case map[string]any:
		if len(typed) == 0 {
			return nil, nil
		}
	}

	data, err := jsonx.Marshal(value)
	if err != nil {
		return nil, err
	}
	return string(data), nil
}

func unmarshalStringSlice(data []byte) ([]string, error) {
	if len(data) == 0 {
		return nil, nil
	}

	var items []string
	if err := jsonx.Unmarshal(data, &items); err != nil {
		return nil, err
	}
	if len(items) == 0 {
		return nil, nil
	}
	return items, nil
}

func unmarshalMetadata(data []byte) (map[string]any, error) {
	if len(data) == 0 {
		return nil, nil
	}

	var metadata map[string]any
	if err := jsonx.Unmarshal(data, &metadata); err != nil {
		return nil, err
	}
	if len(metadata) == 0 {
		return nil, nil
	}
	return metadata, nil
}
