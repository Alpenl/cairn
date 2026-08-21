package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"webtag/internal/migrate"
)

// TestClassifyErrorMapsEveryFailClosedSentinel pins the helper contract: the
// deploy state machine picks its HOLD branch from error.kind, so a sentinel
// that stops mapping to its own kind silently degrades to a generic failure.
func TestClassifyErrorMapsEveryFailClosedSentinel(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		name string
		err  error
		want string
	}{
		{name: "unknown target", err: fmt.Errorf("apply migrations: %w", migrate.ErrUnknownTarget), want: errorKindUnknownTarget},
		{name: "ledger ahead", err: fmt.Errorf("wrapped: %w", migrate.ErrLedgerAhead), want: errorKindLedgerAhead},
		{name: "ledger mismatch", err: fmt.Errorf("wrapped: %w", errLedgerMismatch), want: errorKindLedgerMismatch},
		{name: "anything else", err: errors.New("connection refused"), want: errorKindFailed},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := classifyError(tt.err); got != tt.want {
				t.Fatalf("classifyError(%v) = %q, want %q", tt.err, got, tt.want)
			}
		})
	}
}

func TestRequestedModeCoversEveryTargetForm(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct{ target, want string }{
		{target: "", want: "default"},
		{target: migrate.FreshInstallTarget, want: "fresh"},
		{target: migrate.CurrentSchemaMigrationID, want: "target"},
	} {
		if got := requestedMode(tt.target); got != tt.want {
			t.Fatalf("requestedMode(%q) = %q, want %q", tt.target, got, tt.want)
		}
	}
}

func TestWantsJSONReportAcceptsFlagAndEnvironment(t *testing.T) {
	for _, tt := range []struct {
		name        string
		args        []string
		environment string
		want        bool
	}{
		{name: "neither"},
		{name: "flag", args: []string{reportJSONFlag}, want: true},
		{name: "environment", environment: "true", want: true},
		{name: "environment mixed case", environment: "TRUE", want: true},
		{name: "environment off", environment: "false"},
		{name: "near-miss flag", args: []string{"--report-json=1"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("MIGRATION_REPORT_JSON", tt.environment)
			if got := wantsJSONReport(tt.args); got != tt.want {
				t.Fatalf("wantsJSONReport(%v) with env %q = %t, want %t", tt.args, tt.environment, got, tt.want)
			}
		})
	}
}

// TestReportEncodesTheHelperContract locks the JSON field names down. The
// root-owned updater parses this object; renaming a field is a breaking change
// that must be paired with a schema_version bump.
func TestReportEncodesTheHelperContract(t *testing.T) {
	t.Parallel()

	var stdout bytes.Buffer
	if err := writeReport(&stdout, migrationReport{
		SchemaVersion: reportSchemaVersion,
		Tool:          "cairn-migrate",
		OK:            true,
		Mode:          "target",
		Target:        "readersearch2026081701",
		StartVersion:  "lifecycle2026081401",
		EndVersion:    "readersearch2026081701",
		Applied:       []string{"readersearch2026081701"},
	}); err != nil {
		t.Fatalf("writeReport() error = %v", err)
	}

	var decoded map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &decoded); err != nil {
		t.Fatalf("report is not a single JSON object: %v\n%s", err, stdout.String())
	}
	for _, field := range []string{
		"schema_version", "tool", "tool_version", "tool_commit", "ok",
		"mode", "target", "allow_manual", "start_version", "end_version",
		"applied", "already_at_target", "stopped_at_manual",
	} {
		if _, ok := decoded[field]; !ok {
			t.Errorf("report is missing required field %q", field)
		}
	}
	if got := decoded["schema_version"]; got != float64(reportSchemaVersion) {
		t.Fatalf("schema_version = %v, want %d", got, reportSchemaVersion)
	}
}

// TestFailureReportCarriesAStructuredError proves the failure path is machine
// readable too: the helper must never have to infer a HOLD reason from an exit
// code or from stderr prose.
func TestFailureReportCarriesAStructuredError(t *testing.T) {
	t.Parallel()

	var stdout bytes.Buffer
	if err := writeReport(&stdout, migrationReport{
		SchemaVersion: reportSchemaVersion,
		OK:            false,
		Mode:          "target",
		Target:        migrate.CurrentSchemaMigrationID,
		Applied:       []string{},
		Error:         &reportError{Kind: errorKindLedgerAhead, Message: "ledger shape is not accepted"},
	}); err != nil {
		t.Fatalf("writeReport() error = %v", err)
	}
	var decoded struct {
		OK    bool `json:"ok"`
		Error *struct {
			Kind    string `json:"kind"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &decoded); err != nil {
		t.Fatalf("decode report: %v", err)
	}
	if decoded.OK {
		t.Fatal("failure report claims ok=true")
	}
	if decoded.Error == nil || decoded.Error.Kind != errorKindLedgerAhead {
		t.Fatalf("failure report error = %+v", decoded.Error)
	}
}

// TestSuccessReportOmitsTheErrorObject keeps `error` absent rather than a null
// or zero-valued object, so a helper can branch on its presence.
func TestSuccessReportOmitsTheErrorObject(t *testing.T) {
	t.Parallel()

	var stdout bytes.Buffer
	if err := writeReport(&stdout, migrationReport{SchemaVersion: reportSchemaVersion, OK: true, Applied: []string{}}); err != nil {
		t.Fatalf("writeReport() error = %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &decoded); err != nil {
		t.Fatalf("decode report: %v", err)
	}
	if _, present := decoded["error"]; present {
		t.Fatal("success report carries an error object")
	}
}

func TestHumanRunSummaryNamesStartEndAndAppliedSteps(t *testing.T) {
	t.Parallel()

	var human bytes.Buffer
	writeHumanRunSummary(&human, migrationReport{
		Mode:         "target",
		Target:       "readersearch2026081701",
		StartVersion: "lifecycle2026081401",
		EndVersion:   "readersearch2026081701",
		Applied:      []string{"readersearch2026081701"},
	})
	output := human.String()
	for _, want := range []string{
		"mode=target",
		"target=readersearch2026081701",
		"start=lifecycle2026081401",
		"end=readersearch2026081701",
		"applied 1 step(s): readersearch2026081701",
	} {
		if !bytes.Contains([]byte(output), []byte(want)) {
			t.Errorf("run summary missing %q:\n%s", want, output)
		}
	}
}

func TestHumanRunSummaryStillSpeaksWhenNothingWasApplied(t *testing.T) {
	t.Parallel()

	var human bytes.Buffer
	writeHumanRunSummary(&human, migrationReport{
		Mode:            "target",
		Target:          "b671c9d2e411",
		StartVersion:    "b671c9d2e411",
		EndVersion:      "b671c9d2e411",
		Applied:         []string{},
		AlreadyAtTarget: true,
	})
	if !bytes.Contains(human.Bytes(), []byte("applied 0 step(s); database already at b671c9d2e411")) {
		t.Fatalf("no-op run summary = %q", human.String())
	}
}
