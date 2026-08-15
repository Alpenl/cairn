package config

import (
	"fmt"
	"testing"
)

func TestRiverRetentionDefaultsCoverRecoveryWindow(t *testing.T) {
	setBaseConfigEnv(t)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	if cfg.ParseReconcileIntervalMS != 60000 || cfg.ParseReconcileBatch != 100 ||
		cfg.ParseReconcileRoundTimeoutMS != 30000 || cfg.ParseReconcileMissingAfterMS != 21600000 {
		t.Fatalf("parse reconcile defaults = interval %d batch %d timeout %d missing %d",
			cfg.ParseReconcileIntervalMS,
			cfg.ParseReconcileBatch,
			cfg.ParseReconcileRoundTimeoutMS,
			cfg.ParseReconcileMissingAfterMS,
		)
	}
	if cfg.RiverTerminalRetentionMS != 604800000 || cfg.RiverMaxRecoveryDowntimeMS != 86400000 {
		t.Fatalf("River retention defaults = retention %d downtime %d",
			cfg.RiverTerminalRetentionMS, cfg.RiverMaxRecoveryDowntimeMS)
	}
	if int64(cfg.RiverTerminalRetentionMS) <= minimumRiverTerminalRetentionMS(cfg) {
		t.Fatalf("default retention %d does not cover recovery window %d",
			cfg.RiverTerminalRetentionMS, minimumRiverTerminalRetentionMS(cfg))
	}
}

func TestRiverRetentionReadsOverridesAndAllowsInfiniteRollback(t *testing.T) {
	setBaseConfigEnv(t)
	t.Setenv("PARSE_RECONCILE_INTERVAL_MS", "2500")
	t.Setenv("PARSE_RECONCILE_BATCH", "37")
	t.Setenv("PARSE_RECONCILE_ROUND_TIMEOUT_MS", "250")
	t.Setenv("PARSE_RECONCILE_MISSING_AFTER_MS", "5000")
	t.Setenv("RIVER_MAX_RECOVERY_DOWNTIME_MS", "7000")
	t.Setenv("RIVER_TERMINAL_RETENTION_MS", "-1")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.ParseReconcileIntervalMS != 2500 || cfg.ParseReconcileBatch != 37 ||
		cfg.ParseReconcileRoundTimeoutMS != 250 || cfg.ParseReconcileMissingAfterMS != 5000 ||
		cfg.RiverMaxRecoveryDowntimeMS != 7000 || cfg.RiverTerminalRetentionMS != -1 {
		t.Fatalf("parse/retention overrides were not preserved")
	}
}

func TestRiverRetentionRejectsInvalidIntegers(t *testing.T) {
	for _, name := range []string{
		"PARSE_RECONCILE_INTERVAL_MS",
		"PARSE_RECONCILE_BATCH",
		"PARSE_RECONCILE_ROUND_TIMEOUT_MS",
		"PARSE_RECONCILE_MISSING_AFTER_MS",
		"RIVER_MAX_RECOVERY_DOWNTIME_MS",
		"RIVER_TERMINAL_RETENTION_MS",
	} {
		t.Run(name, func(t *testing.T) {
			setBaseConfigEnv(t)
			t.Setenv(name, "not-an-integer")
			_, err := Load()
			want := name + " must be a valid integer"
			if err == nil || err.Error() != want {
				t.Fatalf("Load() error = %v, want %q", err, want)
			}
		})
	}
}

func TestRiverRetentionRejectsUnsafeBounds(t *testing.T) {
	tests := []struct {
		name    string
		envs    map[string]string
		wantErr string
	}{
		{name: "parse interval", envs: map[string]string{"PARSE_RECONCILE_INTERVAL_MS": "999"}, wantErr: "PARSE_RECONCILE_INTERVAL_MS must be >= 1000"},
		{name: "parse batch low", envs: map[string]string{"PARSE_RECONCILE_BATCH": "0"}, wantErr: "PARSE_RECONCILE_BATCH must be in [1, 1000]"},
		{name: "parse batch high", envs: map[string]string{"PARSE_RECONCILE_BATCH": "1001"}, wantErr: "PARSE_RECONCILE_BATCH must be in [1, 1000]"},
		{name: "parse timeout", envs: map[string]string{"PARSE_RECONCILE_ROUND_TIMEOUT_MS": "0"}, wantErr: "PARSE_RECONCILE_ROUND_TIMEOUT_MS must be >= 1"},
		{
			name: "parse missing threshold",
			envs: map[string]string{
				"PARSE_RECONCILE_ROUND_TIMEOUT_MS": "5000",
				"PARSE_RECONCILE_MISSING_AFTER_MS": "5000",
			},
			wantErr: "PARSE_RECONCILE_MISSING_AFTER_MS must be greater than PARSE_RECONCILE_ROUND_TIMEOUT_MS",
		},
		{name: "downtime", envs: map[string]string{"RIVER_MAX_RECOVERY_DOWNTIME_MS": "999"}, wantErr: "RIVER_MAX_RECOVERY_DOWNTIME_MS must be >= 1000"},
		{name: "zero retention", envs: map[string]string{"RIVER_TERMINAL_RETENTION_MS": "0"}, wantErr: "RIVER_TERMINAL_RETENTION_MS must be -1 or a positive integer"},
		{name: "invalid negative retention", envs: map[string]string{"RIVER_TERMINAL_RETENTION_MS": "-2"}, wantErr: "RIVER_TERMINAL_RETENTION_MS must be -1 or a positive integer"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			setBaseConfigEnv(t)
			for key, value := range tt.envs {
				t.Setenv(key, value)
			}
			_, err := Load()
			if err == nil || err.Error() != tt.wantErr {
				t.Fatalf("Load() error = %v, want %q", err, tt.wantErr)
			}
		})
	}
}

func TestRiverRetentionMustExceedCompleteRecoveryWindow(t *testing.T) {
	setBaseConfigEnv(t)
	t.Setenv("PARSE_RECONCILE_INTERVAL_MS", "2000")
	t.Setenv("PARSE_RECONCILE_ROUND_TIMEOUT_MS", "1000")
	t.Setenv("PARSE_RECONCILE_MISSING_AFTER_MS", "7000")
	t.Setenv("TRANSLATION_RECONCILE_INTERVAL_MS", "3000")
	t.Setenv("TRANSLATION_RECONCILE_ROUND_TIMEOUT_MS", "2000")
	t.Setenv("TRANSLATION_RECONCILE_MISSING_AFTER_MS", "11000")
	t.Setenv("RIVER_MAX_RECOVERY_DOWNTIME_MS", "13000")
	const window = 11000 + 13000 + 5000
	t.Setenv("RIVER_TERMINAL_RETENTION_MS", fmt.Sprint(window))

	_, err := Load()
	want := fmt.Sprintf("RIVER_TERMINAL_RETENTION_MS must be greater than the recovery window (%d ms)", window)
	if err == nil || err.Error() != want {
		t.Fatalf("Load() error = %v, want %q", err, want)
	}

	t.Setenv("RIVER_TERMINAL_RETENTION_MS", fmt.Sprint(window+1))
	if _, err := Load(); err != nil {
		t.Fatalf("Load() with retention above recovery window error = %v", err)
	}
}
