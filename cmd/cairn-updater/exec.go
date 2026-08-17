package main

import (
	"bytes"
	"context"
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
