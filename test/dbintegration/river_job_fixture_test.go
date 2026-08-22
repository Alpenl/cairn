package dbintegration

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/jackc/pgx/v5"
)

type riverJobFixtureDB interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

func insertActiveRiverJob(t *testing.T, db riverJobFixtureDB, args any, kind string) int64 {
	t.Helper()
	encoded, err := json.Marshal(args)
	if err != nil {
		t.Fatalf("marshal active River args: %v", err)
	}
	var jobID int64
	if err := db.QueryRow(t.Context(), `INSERT INTO public.river_job (
		args,kind,max_attempts,state
	) VALUES ($1::jsonb,$2,3,'available') RETURNING id`, encoded, kind).Scan(&jobID); err != nil {
		t.Fatalf("insert active River job: %v", err)
	}
	return jobID
}
