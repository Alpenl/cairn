package config

import "testing"

func TestRiverRetentionDefault(t *testing.T) {
	setBaseConfigEnv(t)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	if cfg.RiverTerminalRetentionMS != 604800000 {
		t.Fatalf("River terminal retention = %d, want 604800000", cfg.RiverTerminalRetentionMS)
	}
}

func TestRiverRetentionAllowsInfiniteRollback(t *testing.T) {
	setBaseConfigEnv(t)
	t.Setenv("RIVER_TERMINAL_RETENTION_MS", "-1")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.RiverTerminalRetentionMS != -1 {
		t.Fatalf("retention override = %d, want -1", cfg.RiverTerminalRetentionMS)
	}
}

func TestRiverRetentionRejectsInvalidIntegers(t *testing.T) {
	setBaseConfigEnv(t)
	t.Setenv("RIVER_TERMINAL_RETENTION_MS", "not-an-integer")
	_, err := Load()
	want := "RIVER_TERMINAL_RETENTION_MS must be a valid integer"
	if err == nil || err.Error() != want {
		t.Fatalf("Load() error = %v, want %q", err, want)
	}
}

func TestRiverRetentionRejectsUnsafeBounds(t *testing.T) {
	tests := []struct {
		name    string
		envs    map[string]string
		wantErr string
	}{
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
