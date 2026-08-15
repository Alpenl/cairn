package arch

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

var (
	remoteSSHCommand = regexp.MustCompile(`(?im)(^|[;&|[:space:]])(?:ssh|scp|sftp)(?:[[:space:]]|$)`)
	pinnedHostSource = regexp.MustCompile(`(?i)\$\{\{\s*(?:secrets|vars)\.[^}\n]*known[_-]?hosts[^}\n]*}}`)
	nonEmptyHostKey  = regexp.MustCompile(`(?i)test\s+-n\s+["']?\$\{?[a-z0-9_]*known_hosts`)
)

func TestSSHWorkflowsRequirePinnedHostIdentity(t *testing.T) {
	t.Parallel()

	paths, err := trackedRepositoryPaths(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range paths {
		if !strings.HasPrefix(path, ".github/workflows/") || (filepath.Ext(path) != ".yml" && filepath.Ext(path) != ".yaml") {
			continue
		}
		data, err := os.ReadFile(filepath.Join("..", "..", filepath.FromSlash(path)))
		if err != nil {
			t.Fatalf("read tracked workflow %s: %v", path, err)
		}
		if violations := sshWorkflowViolations(string(data)); len(violations) > 0 {
			t.Errorf("%s violates the SSH host identity contract:\n  %s", path, strings.Join(violations, "\n  "))
		}
	}
}

func TestSSHWorkflowHostIdentityContractRejectsTOFU(t *testing.T) {
	t.Parallel()

	secure := `
env:
  SSH_KNOWN_HOSTS: ${{ secrets.DEPLOY_KNOWN_HOSTS }}
run: |
  set -euo pipefail
  umask 077
  test -n "$SSH_KNOWN_HOSTS"
  printf '%s\n' "$SSH_KNOWN_HOSTS" > "$RUNNER_TEMP/known_hosts"
  test -s "$RUNNER_TEMP/known_hosts"
  ssh-keygen -l -f "$RUNNER_TEMP/known_hosts" >/dev/null
  ssh -o StrictHostKeyChecking=yes -o UserKnownHostsFile="$RUNNER_TEMP/known_hosts" "$DEPLOY_TARGET" true
`
	if violations := sshWorkflowViolations(secure); len(violations) > 0 {
		t.Fatalf("secure workflow violations = %v", violations)
	}

	for name, source := range map[string]string{
		"dynamic scan": `run: ssh-keyscan "$HOST" >> ~/.ssh/known_hosts || true
run: ssh -o StrictHostKeyChecking=yes "$HOST" true`,
		"accept new":            `run: ssh -o StrictHostKeyChecking=accept-new "$HOST" true`,
		"checking disabled":     `run: ssh -o StrictHostKeyChecking=no "$HOST" true`,
		"missing pinned source": `run: ssh -o StrictHostKeyChecking=yes -o UserKnownHostsFile=/tmp/known_hosts "$HOST" true`,
	} {
		t.Run(name, func(t *testing.T) {
			if violations := sshWorkflowViolations(source); len(violations) == 0 {
				t.Fatal("insecure SSH workflow was accepted")
			}
		})
	}
}

func sshWorkflowViolations(source string) []string {
	lower := strings.ToLower(source)
	var violations []string
	for needle, message := range map[string]string{
		"ssh-keyscan":                      "ssh-keyscan is unauthenticated TOFU and is forbidden",
		"stricthostkeychecking=accept-new": "StrictHostKeyChecking=accept-new is forbidden",
		"stricthostkeychecking=no":         "StrictHostKeyChecking=no is forbidden",
	} {
		if strings.Contains(lower, needle) {
			violations = append(violations, message)
		}
	}
	if !remoteSSHCommand.MatchString(source) {
		return violations
	}
	for needle, message := range map[string]string{
		"stricthostkeychecking=yes": "remote SSH commands must use StrictHostKeyChecking=yes",
		"userknownhostsfile=":       "remote SSH commands must use an explicit UserKnownHostsFile",
		"umask 077":                 "known_hosts must be created under a restrictive umask",
		"ssh-keygen -l -f":          "known_hosts format must be checked before the remote command",
	} {
		if !strings.Contains(lower, needle) {
			violations = append(violations, message)
		}
	}
	if !pinnedHostSource.MatchString(source) {
		violations = append(violations, "known_hosts must come from an explicit GitHub secret or variable")
	}
	if !nonEmptyHostKey.MatchString(source) {
		violations = append(violations, "the pinned known_hosts value must be non-empty before use")
	}
	return violations
}
