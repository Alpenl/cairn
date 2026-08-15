package config

import (
	"strings"
	"testing"
)

func TestTranslationReconcileDefaults(t *testing.T) {
	setBaseConfigEnv(t)
	t.Setenv("TRANSLATION_RECONCILE_INTERVAL_MS", "")
	t.Setenv("TRANSLATION_RECONCILE_BATCH", "")
	t.Setenv("TRANSLATION_RECONCILE_ROUND_TIMEOUT_MS", "")
	t.Setenv("TRANSLATION_RECONCILE_MISSING_AFTER_MS", "")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	if cfg.TranslationReconcileIntervalMS != 60000 {
		t.Errorf("TranslationReconcileIntervalMS = %d, want 60000", cfg.TranslationReconcileIntervalMS)
	}
	if cfg.TranslationReconcileBatch != 100 {
		t.Errorf("TranslationReconcileBatch = %d, want 100", cfg.TranslationReconcileBatch)
	}
	if cfg.TranslationReconcileRoundTimeoutMS != 30000 {
		t.Errorf("TranslationReconcileRoundTimeoutMS = %d, want 30000", cfg.TranslationReconcileRoundTimeoutMS)
	}
	if cfg.TranslationReconcileMissingAfterMS != 21600000 {
		t.Errorf("TranslationReconcileMissingAfterMS = %d, want 21600000", cfg.TranslationReconcileMissingAfterMS)
	}
}

func TestTranslationReconcileReadsOverrides(t *testing.T) {
	setBaseConfigEnv(t)
	t.Setenv("TRANSLATION_RECONCILE_INTERVAL_MS", "2500")
	t.Setenv("TRANSLATION_RECONCILE_BATCH", "37")
	t.Setenv("TRANSLATION_RECONCILE_ROUND_TIMEOUT_MS", "250")
	t.Setenv("TRANSLATION_RECONCILE_MISSING_AFTER_MS", "5000")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	if cfg.TranslationReconcileIntervalMS != 2500 ||
		cfg.TranslationReconcileBatch != 37 ||
		cfg.TranslationReconcileRoundTimeoutMS != 250 ||
		cfg.TranslationReconcileMissingAfterMS != 5000 {
		t.Fatalf("translation reconcile overrides = interval %d, batch %d, round timeout %d, missing after %d",
			cfg.TranslationReconcileIntervalMS,
			cfg.TranslationReconcileBatch,
			cfg.TranslationReconcileRoundTimeoutMS,
			cfg.TranslationReconcileMissingAfterMS,
		)
	}
}

func TestTranslationReconcileRejectsInvalidIntegers(t *testing.T) {
	for _, envName := range []string{
		"TRANSLATION_RECONCILE_INTERVAL_MS",
		"TRANSLATION_RECONCILE_BATCH",
		"TRANSLATION_RECONCILE_ROUND_TIMEOUT_MS",
		"TRANSLATION_RECONCILE_MISSING_AFTER_MS",
	} {
		t.Run(envName, func(t *testing.T) {
			setBaseConfigEnv(t)
			t.Setenv(envName, "not-an-integer")

			_, err := Load()
			want := envName + " must be a valid integer"
			if err == nil || err.Error() != want {
				t.Fatalf("Load() error = %v, want %q", err, want)
			}
		})
	}
}

func TestTranslationReconcileRejectsInvalidBounds(t *testing.T) {
	tests := []struct {
		name    string
		envs    map[string]string
		wantErr string
	}{
		{
			name:    "interval below one second",
			envs:    map[string]string{"TRANSLATION_RECONCILE_INTERVAL_MS": "999"},
			wantErr: "TRANSLATION_RECONCILE_INTERVAL_MS must be >= 1000",
		},
		{
			name:    "zero batch",
			envs:    map[string]string{"TRANSLATION_RECONCILE_BATCH": "0"},
			wantErr: "TRANSLATION_RECONCILE_BATCH must be in [1, 1000]",
		},
		{
			name:    "oversized batch",
			envs:    map[string]string{"TRANSLATION_RECONCILE_BATCH": "1001"},
			wantErr: "TRANSLATION_RECONCILE_BATCH must be in [1, 1000]",
		},
		{
			name:    "non-positive round timeout",
			envs:    map[string]string{"TRANSLATION_RECONCILE_ROUND_TIMEOUT_MS": "0"},
			wantErr: "TRANSLATION_RECONCILE_ROUND_TIMEOUT_MS must be >= 1",
		},
		{
			name: "missing threshold equals round timeout",
			envs: map[string]string{
				"TRANSLATION_RECONCILE_ROUND_TIMEOUT_MS": "5000",
				"TRANSLATION_RECONCILE_MISSING_AFTER_MS": "5000",
			},
			wantErr: "TRANSLATION_RECONCILE_MISSING_AFTER_MS must be greater than TRANSLATION_RECONCILE_ROUND_TIMEOUT_MS",
		},
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

func TestTranslationReconcileAcceptsInclusiveBounds(t *testing.T) {
	tests := []struct {
		name  string
		batch string
	}{
		{name: "minimum batch", batch: "1"},
		{name: "maximum batch", batch: "1000"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			setBaseConfigEnv(t)
			t.Setenv("TRANSLATION_RECONCILE_INTERVAL_MS", "1000")
			t.Setenv("TRANSLATION_RECONCILE_BATCH", tt.batch)
			t.Setenv("TRANSLATION_RECONCILE_ROUND_TIMEOUT_MS", "1")
			t.Setenv("TRANSLATION_RECONCILE_MISSING_AFTER_MS", "2")

			if _, err := Load(); err != nil {
				t.Fatalf("Load() error = %v", err)
			}
		})
	}
}

func TestTranslationReconcileValidationNamesRelatedSetting(t *testing.T) {
	setBaseConfigEnv(t)
	t.Setenv("TRANSLATION_RECONCILE_ROUND_TIMEOUT_MS", "30001")
	t.Setenv("TRANSLATION_RECONCILE_MISSING_AFTER_MS", "30000")

	_, err := Load()
	if err == nil || !strings.Contains(err.Error(), "TRANSLATION_RECONCILE_ROUND_TIMEOUT_MS") {
		t.Fatalf("Load() error = %v, want related timeout setting named", err)
	}
}
