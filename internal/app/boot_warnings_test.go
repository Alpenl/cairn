package app

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"webtag/internal/config"
)

// TestNonLocalhostCORSOrigins is the table-driven contract for the
// loopback classifier behind the EXTENSION_API_TOKEN startup WARN. Only
// genuine loopback origins (localhost / 127.0.0.1 / ::1, any scheme or
// port) are filtered out; everything else — including the "*" wildcard —
// is reported so the WARN can escalate its wording.
func TestNonLocalhostCORSOrigins(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		origins []string
		want    []string
	}{
		{name: "nil", origins: nil, want: nil},
		{name: "only localhost", origins: []string{"http://localhost:3000"}, want: nil},
		{name: "loopback ipv4", origins: []string{"https://127.0.0.1:8080"}, want: nil},
		{name: "loopback ipv6", origins: []string{"http://[::1]:5173"}, want: nil},
		{name: "bare localhost no scheme", origins: []string{"localhost"}, want: nil},
		{name: "mixed keeps non-local", origins: []string{"http://localhost:3000", "https://app.example.com"}, want: []string{"https://app.example.com"}},
		{name: "wildcard counts as non-local", origins: []string{"*"}, want: []string{"*"}},
		{name: "extension origin", origins: []string{"chrome-extension://abcdefghijklmnop"}, want: []string{"chrome-extension://abcdefghijklmnop"}},
		{name: "blank entries skipped", origins: []string{"  ", "http://localhost"}, want: nil},
		{name: "public ip", origins: []string{"http://203.0.113.10:8080"}, want: []string{"http://203.0.113.10:8080"}},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := nonLocalhostCORSOrigins(tc.origins)
			if len(got) != len(tc.want) {
				t.Fatalf("nonLocalhostCORSOrigins(%#v) = %#v, want %#v", tc.origins, got, tc.want)
			}
			for i := range tc.want {
				if got[i] != tc.want[i] {
					t.Fatalf("nonLocalhostCORSOrigins(%#v)[%d] = %q, want %q", tc.origins, i, got[i], tc.want[i])
				}
			}
		})
	}
}

// capturedWarnings runs warnUnsafeBootDefaults against a JSON slog
// handler and returns the WARN-level records whose "flag" attribute
// matches wantFlag.
func capturedWarnings(t *testing.T, cfg config.Config, wantFlag string) []map[string]any {
	t.Helper()
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))

	warnUnsafeBootDefaults(cfg, logger)

	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("unmarshal log line %q: %v", line, err)
		}
		if rec["level"] != "WARN" {
			continue
		}
		if rec["flag"] == wantFlag {
			out = append(out, rec)
		}
	}
	return out
}

// baseValidishConfig returns a config whose other unsafe-default knobs are
// set to their *safe* values so only the EXTENSION_API_TOKEN-related WARN
// under test fires. AppEnv=dev keeps the admin-token WARN quiet.
func baseValidishConfig() config.Config {
	cfg := config.Config{}
	cfg.AppEnv = "dev"
	cfg.MetricsAuthToken = "metrics-token"
	cfg.RateLimit.RPS = 10
	cfg.Analyzer.AllowUnsafeTargets = false
	return cfg
}

// TestWarnEphemeralSessionSigningKey pins the WARN that breaks the silence
// around an empty SESSION_SIGNING_KEY.
//
// Without it the failure mode is invisible at the only moment it is cheap to
// fix: the key is minted at boot, sessions work fine all day, and the cost
// lands weeks later as "the Reader keeps asking for the installation token
// again" after an unrelated restart. The message therefore has to name the
// consequence and the remedy, not just the flag.
func TestWarnEphemeralSessionSigningKey(t *testing.T) {
	t.Parallel()

	t.Run("ephemeral key warns with the restart consequence", func(t *testing.T) {
		t.Parallel()
		cfg := baseValidishConfig()
		cfg.SessionSigningKeyEphemeral = true

		warns := capturedWarnings(t, cfg, "SESSION_SIGNING_KEY")
		if len(warns) != 1 {
			t.Fatalf("SESSION_SIGNING_KEY warns = %d, want 1", len(warns))
		}
		msg, _ := warns[0]["msg"].(string)
		for _, want := range []string{"restart", "Reader session", "installation token"} {
			if !strings.Contains(msg, want) {
				t.Fatalf("msg = %q, want it to mention %q", msg, want)
			}
		}
	})

	t.Run("configured key stays quiet", func(t *testing.T) {
		t.Parallel()
		cfg := baseValidishConfig()
		cfg.SessionSigningKey = "operator-controlled-key"
		cfg.SessionSigningKeyEphemeral = false

		if warns := capturedWarnings(t, cfg, "SESSION_SIGNING_KEY"); len(warns) != 0 {
			t.Fatalf("SESSION_SIGNING_KEY warns = %d, want 0", len(warns))
		}
	})
}

// TestWarnPublicAPIOpen pins the three branches of the unauthenticated-API WARN.
//
// The trigger moved: it used to fire whenever EXTENSION_API_TOKEN was empty,
// because an empty token meant the public API was open. That is no longer true —
// single mode is fail-closed by default and only PUBLIC_API_OPEN=true reopens it.
// Keeping the old trigger would have warned on every safe deployment and stayed
// silent on the one configuration that is actually dangerous.
//
//  1. open + only localhost CORS → plain unauthenticated WARN
//  2. open + non-localhost CORS  → escalated WARN naming origins
//  3. not open                    → no WARN at all
func TestWarnPublicAPIOpen(t *testing.T) {
	t.Parallel()

	t.Run("open localhost-only emits plain warn", func(t *testing.T) {
		t.Parallel()
		cfg := baseValidishConfig()
		cfg.PublicAPIOpen = true
		cfg.Server.CORSOrigins = []string{"http://localhost:3000"}

		warns := capturedWarnings(t, cfg, "PUBLIC_API_OPEN")
		if len(warns) != 1 {
			t.Fatalf("PUBLIC_API_OPEN warns = %d, want 1", len(warns))
		}
		msg, _ := warns[0]["msg"].(string)
		if !strings.Contains(msg, "unauthenticated") {
			t.Fatalf("msg = %q, want unauthenticated wording", msg)
		}
		if _, escalated := warns[0]["non_localhost_cors_origins"]; escalated {
			t.Fatalf("localhost-only config should not attach non_localhost_cors_origins: %#v", warns[0])
		}
	})

	t.Run("open with non-localhost CORS escalates warn", func(t *testing.T) {
		t.Parallel()
		cfg := baseValidishConfig()
		cfg.PublicAPIOpen = true
		cfg.Server.CORSOrigins = []string{"http://localhost:3000", "https://app.example.com"}

		warns := capturedWarnings(t, cfg, "PUBLIC_API_OPEN")
		if len(warns) != 1 {
			t.Fatalf("PUBLIC_API_OPEN warns = %d, want 1", len(warns))
		}
		origins, ok := warns[0]["non_localhost_cors_origins"].([]any)
		if !ok || len(origins) != 1 || origins[0] != "https://app.example.com" {
			t.Fatalf("non_localhost_cors_origins = %#v, want [https://app.example.com]", warns[0]["non_localhost_cors_origins"])
		}
		msg, _ := warns[0]["msg"].(string)
		if !strings.Contains(strings.ToLower(msg), "unauthenticated") {
			t.Fatalf("escalated msg = %q, want unauthenticated wording", msg)
		}
	})

	// 默认（fail-closed）不该有这条 WARN——它以前对每一个安全部署都会响。
	t.Run("default fail-closed emits no warn", func(t *testing.T) {
		t.Parallel()
		cfg := baseValidishConfig()
		cfg.Server.CORSOrigins = []string{"https://app.example.com"}

		warns := capturedWarnings(t, cfg, "PUBLIC_API_OPEN")
		if len(warns) != 0 {
			t.Fatalf("PUBLIC_API_OPEN warns = %d, want 0 when the API is gated", len(warns))
		}
	})
}
