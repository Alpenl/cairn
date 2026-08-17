package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// Everything in this file shells out, and every argument is either a compiled-in
// constant, a configured absolute path, or a value that came out of a signed
// manifest. Nothing here ever interpolates a caller-supplied string, and nothing
// goes through a shell: exec.CommandContext takes an argv, so a target that
// somehow contained a semicolon would be an argument, not a second command.

// CommandResult is what a finished subprocess reported.
type CommandResult struct {
	Stdout   []byte
	Stderr   []byte
	ExitCode int
}

// CommandRunner runs one external program to completion.
//
// It is an interface so the state machine can be driven without a host, but the
// fault-injection tests deliberately use the real runner against generated
// executables instead: the interesting failures of this design are a program
// that exits zero having done nothing and a program that writes an empty file,
// and a mock that returns a struct cannot reproduce either.
type CommandRunner interface {
	Run(ctx context.Context, name string, args []string, env []string) (CommandResult, error)
}

type execRunner struct{}

func (execRunner) Run(ctx context.Context, name string, args []string, env []string) (CommandResult, error) {
	command := exec.CommandContext(ctx, name, args...) //nolint:gosec // name and args are configured paths and signed-manifest values, never request input.
	if len(env) > 0 {
		command.Env = env
	}
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr

	err := command.Run()
	result := CommandResult{Stdout: stdout.Bytes(), Stderr: stderr.Bytes()}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		result.ExitCode = exitErr.ExitCode()
		return result, fmt.Errorf("%s exited with status %d: %s", name, result.ExitCode, firstLine(stderr.Bytes()))
	}
	if err != nil {
		return result, fmt.Errorf("run %s: %w", name, err)
	}
	return result, nil
}

// backupRunner adapts the helper's command seam to the narrower interface
// internal/deploybackup declares. The backup package deliberately does not
// know about CommandResult: its whole job is to be testable against a real
// PostgreSQL server from the integration module, which cannot import this
// package.
type backupRunner struct{ inner CommandRunner }

func (adapter backupRunner) Run(ctx context.Context, name string, args, env []string) ([]byte, []byte, error) {
	result, err := adapter.inner.Run(ctx, name, args, env)
	return result.Stdout, result.Stderr, err
}

func firstLine(data []byte) string {
	text := strings.TrimSpace(string(data))
	if text == "" {
		return "(no stderr output)"
	}
	if index := strings.IndexByte(text, '\n'); index >= 0 {
		return text[:index]
	}
	return text
}

// ServiceControl starts and stops the application unit.
type ServiceControl struct {
	runner    CommandRunner
	systemctl string
	unit      string
}

// NewServiceControl builds a systemd-backed service controller.
func NewServiceControl(runner CommandRunner, systemctl, unit string) *ServiceControl {
	return &ServiceControl{runner: runner, systemctl: systemctl, unit: unit}
}

// Stop quiesces the application.
func (service *ServiceControl) Stop(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, ServiceStopTimeout)
	defer cancel()
	if _, err := service.runner.Run(ctx, service.systemctl, []string{"stop", service.unit}, nil); err != nil {
		return fmt.Errorf("stop %s: %w", service.unit, err)
	}
	return nil
}

// Start brings the application back.
func (service *ServiceControl) Start(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, ServiceStopTimeout)
	defer cancel()
	if _, err := service.runner.Run(ctx, service.systemctl, []string{"start", service.unit}, nil); err != nil {
		return fmt.Errorf("start %s: %w", service.unit, err)
	}
	return nil
}

// Active reports whether the unit is running. systemd's `is-active` exits
// non-zero for an inactive unit, so a non-zero exit is an answer rather than a
// failure and only the stdout word is consulted.
func (service *ServiceControl) Active(ctx context.Context) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	result, _ := service.runner.Run(ctx, service.systemctl, []string{"is-active", service.unit}, nil)
	state := strings.TrimSpace(string(result.Stdout))
	switch state {
	case "active":
		return true, nil
	case "inactive", "failed", "activating", "deactivating", "unknown", "":
		return false, nil
	default:
		return false, fmt.Errorf("systemctl is-active %s reported %q", service.unit, state)
	}
}

// MigrationReport mirrors the JSON document cmd/migrate emits.
//
// It is a helper-owned copy of the wire shape rather than a re-export of the
// migration package's types, and that is deliberate. The helper runs the
// *target release's* migrate binary, not its own: the whole point of migrating
// to a newer schema is that the new steps only exist in the new binary. So the
// contract between them is a versioned document, and pinning it here is what
// lets an older helper notice that it does not understand a newer report
// instead of silently reading zero values out of it.
type MigrationReport struct {
	SchemaVersion   int      `json:"schema_version"`
	Tool            string   `json:"tool"`
	ToolVersion     string   `json:"tool_version"`
	ToolCommit      string   `json:"tool_commit"`
	OK              bool     `json:"ok"`
	Mode            string   `json:"mode"`
	Target          string   `json:"target"`
	AllowManual     bool     `json:"allow_manual"`
	StartVersion    string   `json:"start_version"`
	EndVersion      string   `json:"end_version"`
	Applied         []string `json:"applied"`
	AlreadyAtTarget bool     `json:"already_at_target"`
	StoppedAtManual string   `json:"stopped_at_manual"`

	OnlineUpdate *OnlineUpdatePlan     `json:"online_update,omitempty"`
	Ledgers      *LedgerReconciliation `json:"ledgers,omitempty"`
	Error        *MigrationReportError `json:"error,omitempty"`
}

// MigrationReportSchemaVersion is the report schema this helper speaks.
const MigrationReportSchemaVersion = 1

// OnlineUpdatePlan is the migration range decision.
type OnlineUpdatePlan struct {
	Target   string                `json:"target"`
	Pending  []string              `json:"pending"`
	Allowed  bool                  `json:"allowed"`
	Blockers []OnlineUpdateBlocker `json:"blockers"`
}

// OnlineUpdateBlocker is one migration step a page-triggered update may not
// cross.
type OnlineUpdateBlocker struct {
	StepID string `json:"step_id"`
	Reason string `json:"reason"`
	Detail string `json:"detail"`
}

// LedgerReconciliation is the post-migration proof that both ledgers landed.
type LedgerReconciliation struct {
	OK       bool              `json:"ok"`
	Schema   SchemaLedgerState `json:"schema"`
	River    RiverLedgerState  `json:"river"`
	Problems []LedgerProblem   `json:"problems"`
}

// SchemaLedgerState is the application migration ledger.
type SchemaLedgerState struct {
	Present  bool     `json:"present"`
	Target   string   `json:"target"`
	Head     string   `json:"head"`
	Missing  []string `json:"missing"`
	Extra    []string `json:"extra"`
	AtTarget bool     `json:"at_target"`
}

// RiverLedgerState is the queue migration ledger.
type RiverLedgerState struct {
	Present  bool  `json:"present"`
	Target   int   `json:"target"`
	Head     int   `json:"head"`
	Missing  []int `json:"missing"`
	Extra    []int `json:"extra"`
	AtTarget bool  `json:"at_target"`
}

// LedgerProblem is one reconciliation finding. Kind "ahead" is the overshoot
// case: for a forward-only migration system it cannot be corrected by
// migrating, because it means a different release already moved this database.
type LedgerProblem struct {
	Ledger string `json:"ledger"`
	Kind   string `json:"kind"`
	Detail string `json:"detail"`
}

// LedgerProblemAhead is the overshoot discriminator.
const LedgerProblemAhead = "ahead"

// MigrationReportError is the stable failure discriminator.
type MigrationReportError struct {
	Kind    string `json:"kind"`
	Message string `json:"message"`
}

// Overshot reports whether either ledger passed its declared target.
func (reconciliation *LedgerReconciliation) Overshot() bool {
	if reconciliation == nil {
		return false
	}
	for _, problem := range reconciliation.Problems {
		if problem.Kind == LedgerProblemAhead {
			return true
		}
	}
	return len(reconciliation.Schema.Extra) > 0 || len(reconciliation.River.Extra) > 0
}

// Migrator drives the target release's migrate binary.
type Migrator struct {
	runner      CommandRunner
	binary      string
	databaseURL string
}

// NewMigrator binds a migrator to one staged migrate executable.
func NewMigrator(runner CommandRunner, binary, databaseURL string) *Migrator {
	return &Migrator{runner: runner, binary: binary, databaseURL: databaseURL}
}

// planFlag asks the target release to evaluate the range without applying it.
//
// The helper cannot answer this question itself. Its own compiled-in migration
// plan is the *old* release's, which by definition does not contain the steps
// the update would apply, so planning locally would always report an empty
// range and approve everything. The decision has to come from the binary that
// owns the steps.
const planFlag = "--plan-json"

// Plan evaluates the online update decision before anything is stopped.
func (migrator *Migrator) Plan(ctx context.Context, target string) (*MigrationReport, error) {
	return migrator.invoke(ctx, []string{planFlag}, target, MigrateTimeout)
}

// Apply migrates to the exact target and returns the report.
//
// MIGRATION_ALLOW_MANUAL is never set. A manual gate exists precisely because
// someone decided a human has to be present for that step; a deployment helper
// that could set the flag would be a way for a page click to answer a question
// that was reserved for a person.
func (migrator *Migrator) Apply(ctx context.Context, target string) (*MigrationReport, error) {
	return migrator.invoke(ctx, nil, target, MigrateTimeout)
}

func (migrator *Migrator) invoke(ctx context.Context, args []string, target string, timeout time.Duration) (*MigrationReport, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	env := []string{
		"DATABASE_URL=" + migrator.databaseURL,
		"MIGRATION_TARGET=" + target,
		"MIGRATION_REPORT_JSON=true",
		"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
	}
	result, runErr := migrator.runner.Run(ctx, migrator.binary, args, env)

	// The report is parsed on both the success and the failure path. cmd/migrate
	// writes it either way, and on failure it is the only thing that says which
	// steps committed before the failing one — which is exactly what a hold
	// point has to record. Judging the outcome by exit code alone is the
	// mistake this repository has already been bitten by.
	report, parseErr := parseMigrationReport(result.Stdout)
	if parseErr != nil {
		if runErr != nil {
			return nil, fmt.Errorf("%w (and its report was unreadable: %w)", runErr, parseErr)
		}
		return nil, parseErr
	}
	if runErr != nil {
		return report, runErr
	}
	return report, nil
}

func parseMigrationReport(stdout []byte) (*MigrationReport, error) {
	trimmed := bytes.TrimSpace(stdout)
	if len(trimmed) == 0 {
		return nil, errors.New("the migration produced no JSON report; its result cannot be established")
	}
	var report MigrationReport
	if err := json.Unmarshal(trimmed, &report); err != nil {
		return nil, fmt.Errorf("parse migration report: %w", err)
	}
	if report.SchemaVersion != MigrationReportSchemaVersion {
		return nil, fmt.Errorf("the migration report is schema version %d, this helper speaks %d",
			report.SchemaVersion, MigrationReportSchemaVersion)
	}
	return &report, nil
}

// Identity runs a staged executable's --version and returns its exact stdout.
func Identity(ctx context.Context, runner CommandRunner, binary string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, IdentityTimeout)
	defer cancel()
	result, err := runner.Run(ctx, binary, []string{"--version"}, []string{"PATH=/usr/bin:/bin"})
	if err != nil {
		return nil, fmt.Errorf("ask %s for its identity: %w", binary, err)
	}
	return result.Stdout, nil
}
