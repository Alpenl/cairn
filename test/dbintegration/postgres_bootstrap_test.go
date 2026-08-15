package dbintegration

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type postgresUnavailableReporterSpy struct {
	fatalMessage string
	skipMessage  string
}

func (*postgresUnavailableReporterSpy) Helper() {}

func (r *postgresUnavailableReporterSpy) Fatalf(format string, args ...any) {
	r.fatalMessage = fmt.Sprintf(format, args...)
}

func (r *postgresUnavailableReporterSpy) Skipf(format string, args ...any) {
	r.skipMessage = fmt.Sprintf(format, args...)
}

func TestReportPostgresUnavailableSkipsOptionalLocalRun(t *testing.T) {
	for _, value := range []string{"", "false"} {
		t.Run(fmt.Sprintf("value_%q", value), func(t *testing.T) {
			t.Setenv(dbIntegrationRequiredEnv, value)
			reporter := &postgresUnavailableReporterSpy{}

			reportPostgresUnavailable(reporter, errors.New("docker unavailable"))

			if reporter.fatalMessage != "" {
				t.Fatalf("Fatalf message = %q, want none", reporter.fatalMessage)
			}
			if !strings.Contains(reporter.skipMessage, "docker unavailable") {
				t.Fatalf("Skipf message = %q, want bootstrap error", reporter.skipMessage)
			}
		})
	}
}

func TestReportPostgresUnavailableFailsClosedWhenRequired(t *testing.T) {
	for _, value := range []string{"true", "malformed"} {
		t.Run(value, func(t *testing.T) {
			t.Setenv(dbIntegrationRequiredEnv, value)
			reporter := &postgresUnavailableReporterSpy{}

			reportPostgresUnavailable(reporter, errors.New("docker unavailable"))

			if reporter.skipMessage != "" {
				t.Fatalf("Skipf message = %q, want none", reporter.skipMessage)
			}
			if !strings.Contains(reporter.fatalMessage, "docker unavailable") {
				t.Fatalf("Fatalf message = %q, want bootstrap error", reporter.fatalMessage)
			}
		})
	}
}

func TestRequiredTargetFailsClosedOnPostgresBootstrap(t *testing.T) {
	makefilePath := filepath.Join("..", "..", "Makefile")
	contents, err := os.ReadFile(makefilePath)
	if err != nil {
		t.Fatalf("read repository Makefile: %v", err)
	}
	requiredCommand := dbIntegrationRequiredEnv + "=true $(GO) test -C test/dbintegration"
	if !strings.Contains(string(contents), requiredCommand) {
		t.Fatalf("test-dbintegration-required target is missing %q", requiredCommand)
	}
}
