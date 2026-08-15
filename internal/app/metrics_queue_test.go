package app

import (
	"errors"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/pashagolub/pgxmock/v4"

	"webtag/internal/observability"
)

func TestCachedRiverCapacitySharesSnapshotAndRetainsLastGoodValues(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool() error = %v", err)
	}
	defer mock.Close()

	const retentionMillis = int64(7 * 24 * 60 * 60 * 1000)
	mock.ExpectQuery(regexp.QuoteMeta(riverCapacitySnapshotSQL)).
		WithArgs(retentionMillis).
		WillReturnRows(pgxmock.NewRows([]string{
			"cancelled", "completed", "discarded", "oldest_age", "table_bytes", "index_bytes", "cleanup_overdue",
		}).AddRow(int64(2), int64(3), int64(5), float64(7200), int64(4096), int64(2048), int64(1)))
	capacity := &cachedRiverCapacity{
		queryer: mock, ttl: time.Hour, retentionMillis: retentionMillis,
	}

	if got := capacity.terminal("cancelled"); got != 2 {
		t.Fatalf("cancelled = %v, want 2", got)
	}
	snapshot := capacity.current()
	if snapshot.completed != 3 || snapshot.discarded != 5 || snapshot.oldestAge != 7200 ||
		snapshot.tableBytes != 4096 || snapshot.indexBytes != 2048 ||
		snapshot.cleanupOverdue != 1 || snapshot.querySuccess != 1 {
		t.Fatalf("snapshot = %+v, want first complete capacity sample", snapshot)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("shared snapshot issued unexpected queries: %v", err)
	}

	capacity.mu.Lock()
	capacity.lastAttemptedAt = time.Now().Add(-2 * time.Hour)
	capacity.mu.Unlock()
	mock.ExpectQuery(regexp.QuoteMeta(riverCapacitySnapshotSQL)).
		WithArgs(retentionMillis).
		WillReturnError(errors.New("capacity database unavailable"))
	failed := capacity.current()
	if failed.querySuccess != 0 || failed.completed != 3 || failed.tableBytes != 4096 ||
		failed.cleanupOverdue != 1 {
		t.Fatalf("failed snapshot = %+v, want last values with success=0", failed)
	}
	if got := capacity.terminal("discarded"); got != 5 {
		t.Fatalf("discarded after cached failure = %v, want last good 5", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("cached failure issued unexpected queries: %v", err)
	}
}

func TestRegisterRiverCapacityGaugesExposesBoundedMetrics(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool() error = %v", err)
	}
	defer mock.Close()

	const retentionMillis = int64(60_000)
	mock.ExpectQuery(regexp.QuoteMeta(riverCapacitySnapshotSQL)).
		WithArgs(retentionMillis).
		WillReturnRows(pgxmock.NewRows([]string{
			"cancelled", "completed", "discarded", "oldest_age", "table_bytes", "index_bytes", "cleanup_overdue",
		}).AddRow(int64(2), int64(3), int64(5), float64(120), int64(4096), int64(2048), int64(1)))
	metrics := observability.NewMetrics()
	registerRiverCapacityGauges(metrics, mock, time.Minute)

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest("GET", "/metrics", nil)
	metrics.Handler().ServeHTTP(recorder, request)
	if recorder.Code != 200 {
		t.Fatalf("metrics status = %d, body=%s", recorder.Code, recorder.Body.String())
	}
	body := recorder.Body.String()
	for _, want := range []string{
		`webtag_river_terminal_jobs{state="cancelled"} 2`,
		`webtag_river_terminal_jobs{state="completed"} 3`,
		`webtag_river_terminal_jobs{state="discarded"} 5`,
		"webtag_river_oldest_terminal_age_seconds 120",
		"webtag_river_job_table_bytes 4096",
		"webtag_river_job_index_bytes 2048",
		"webtag_river_cleanup_overdue_jobs 1",
		"webtag_river_capacity_query_success 1",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics output missing %q", want)
		}
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("capacity gauges did not share one query: %v", err)
	}
}
