package main

import (
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"testing"
)

// The permission model is the helper's own precondition, so the tests here are
// about refusing to start rather than about failing an update. A helper that
// started on a host where /opt/webtag/releases had become group-writable would
// keep reporting successful updates while the guarantee it exists to provide
// was already gone.

func TestTheSelfCheckCreatesTheDirectoriesItOwns(t *testing.T) {
	host := newHost(t)
	for _, path := range []string{
		host.config.ReleasesDir(),
		host.config.BackupsDir(),
		host.config.ReaderReleasesDir(),
		host.config.StateDir,
		host.config.JobsDir(),
	} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("%s was not created: %v", path, err)
		}
		if !info.IsDir() {
			t.Fatalf("%s is not a directory", path)
		}
		// The property that actually matters: no path the helper owns is
		// writable by anyone but its owner.
		if info.Mode().Perm()&0o022 != 0 {
			t.Fatalf("%s is mode %04o, which is writable beyond its owner", path, info.Mode().Perm())
		}
	}
	// Helper-private state is owner-only, not merely non-writable.
	for _, path := range []string{host.config.StateDir, host.config.JobsDir()} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat %s: %v", path, err)
		}
		if info.Mode().Perm() != 0o700 {
			t.Fatalf("%s is mode %04o, expected 0700", path, info.Mode().Perm())
		}
	}
}

func TestEachWrongPermissionIsNamedIndividually(t *testing.T) {
	cases := []struct {
		name    string
		break_  func(*host)
		mention string
	}{
		{
			name:    "a group-writable releases directory",
			break_:  func(host *host) { chmod(host, host.config.ReleasesDir(), 0o775) },
			mention: "releases",
		},
		{
			name:    "a world-writable backups directory",
			break_:  func(host *host) { chmod(host, host.config.BackupsDir(), 0o777) },
			mention: "backups",
		},
		{
			name:    "a world-readable helper environment file",
			break_:  func(host *host) { chmod(host, host.config.HelperEnv, 0o644) },
			mention: "cairn-updater.env",
		},
		{
			name:    "a group-writable application environment file",
			break_:  func(host *host) { chmod(host, host.config.CoreEnvFile(), 0o660) },
			mention: ".env",
		},
		{
			name:    "a world-readable job directory",
			break_:  func(host *host) { chmod(host, host.config.JobsDir(), 0o755) },
			mention: "jobs",
		},
		{
			name:    "a writable Reader release directory",
			break_:  func(host *host) { chmod(host, host.config.ReaderReleasesDir(), 0o757) },
			mention: "releases",
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			host := newHost(t)
			testCase.break_(host)
			err := BuildLayout(host.config).Enforce()
			if err == nil {
				t.Fatalf("%s was accepted", testCase.name)
			}
			if !strings.Contains(err.Error(), testCase.mention) {
				t.Fatalf("the failure must name the path, got %v", err)
			}
			// The message has to say what the requirement protects, not just
			// that a number differs.
			if !strings.Contains(err.Error(), "the model requires") {
				t.Fatalf("the failure must state the requirement, got %v", err)
			}
		})
	}
}

func TestAMissingApplicationEnvironmentFileRefusesToStart(t *testing.T) {
	host := newHost(t)
	if err := os.Remove(host.config.CoreEnvFile()); err != nil {
		t.Fatalf("remove application env file: %v", err)
	}
	err := BuildLayout(host.config).Enforce()
	if err == nil {
		t.Fatal("a missing application environment file was accepted")
	}
	if !strings.Contains(err.Error(), "must not invent it") {
		t.Fatalf("the helper must refuse rather than create it, got %v", err)
	}
}

func TestASymlinkedDeploymentPathIsRefused(t *testing.T) {
	host := newHost(t)
	// Replacing a deployment directory with a symlink is how an attacker who
	// can write one path gets everything another path was trusted for. Lstat is
	// the only call that can tell the difference.
	elsewhere := filepath.Join(host.root, "elsewhere")
	if err := os.MkdirAll(elsewhere, 0o755); err != nil {
		t.Fatalf("create decoy: %v", err)
	}
	if err := os.RemoveAll(host.config.BackupsDir()); err != nil {
		t.Fatalf("remove backups: %v", err)
	}
	if err := os.Symlink(elsewhere, host.config.BackupsDir()); err != nil {
		t.Fatalf("symlink backups: %v", err)
	}
	err := BuildLayout(host.config).Enforce()
	if err == nil {
		t.Fatal("a symlinked deployment path was accepted")
	}
	if !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("the failure must name the symlink, got %v", err)
	}
}

func TestEveryViolationIsReportedInOnePass(t *testing.T) {
	host := newHost(t)
	chmod(host, host.config.ReleasesDir(), 0o775)
	chmod(host, host.config.BackupsDir(), 0o777)
	chmod(host, host.config.HelperEnv, 0o644)

	err := BuildLayout(host.config).Enforce()
	if err == nil {
		t.Fatal("three violations were accepted")
	}
	// An operator fixing a freshly installed host should get the whole list
	// rather than restarting the unit once per wrong mode.
	if count := strings.Count(err.Error(), "the model requires"); count != 3 {
		t.Fatalf("expected all three violations in one message, got %d:\n%v", count, err)
	}
}

func TestAWrongOwnerIsRefused(t *testing.T) {
	host := newHost(t)
	// The helper runs as root in production and every deployment path must be
	// root-owned. Asserting a uid the test process is not lets the check run
	// for real without the suite needing root.
	host.config.OwnerUID++

	err := BuildLayout(host.config).Enforce()
	if err == nil {
		t.Fatal("paths owned by the wrong uid were accepted")
	}
	if !strings.Contains(err.Error(), "is owned by uid") {
		t.Fatalf("the failure must name the owner, got %v", err)
	}
}

func TestTheSocketIsCreatedGroupReadableAndNotWorldReachable(t *testing.T) {
	host := newHost(t)
	if err := os.MkdirAll(filepath.Dir(host.config.SocketPath), 0o755); err != nil {
		t.Fatalf("create socket directory: %v", err)
	}
	listener, err := ListenOnSocket(host.config)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = listener.Close() }()

	info, err := os.Stat(host.config.SocketPath)
	if err != nil {
		t.Fatalf("stat socket: %v", err)
	}
	if info.Mode().Perm() != 0o660 {
		t.Fatalf("the socket is mode %04o, the model requires 0660", info.Mode().Perm())
	}
	if info.Mode().Perm()&0o007 != 0 {
		t.Fatal("the socket must not be reachable by users outside its group")
	}
}

func TestARegularFileInTheSocketPathIsNotRemoved(t *testing.T) {
	host := newHost(t)
	if err := os.MkdirAll(filepath.Dir(host.config.SocketPath), 0o755); err != nil {
		t.Fatalf("create socket directory: %v", err)
	}
	// Something other than a previous helper put this here. Unlinking it would
	// be doing that thing's work.
	writeFile(t, host.config.SocketPath, "not a socket", 0o600)

	if _, err := ListenOnSocket(host.config); err == nil {
		t.Fatal("a regular file in the socket path was removed")
	}
	if _, err := os.Stat(host.config.SocketPath); err != nil {
		t.Fatalf("the foreign file must be left alone: %v", err)
	}
}

// TestTheApplicationAccountMustNotReachTheSocket covers the half of the model
// that file modes cannot express. The grants live in the unit file and in
// /etc/group, and both are silent: a perfectly hardened directory tree on a
// host where the application can simply connect to the deployment socket and
// ask for an update is not hardened at all.
func TestTheApplicationAccountMustNotReachTheSocket(t *testing.T) {
	host := newHost(t)
	current, err := user.Current()
	if err != nil {
		t.Fatalf("read current user: %v", err)
	}

	// The application account is a member of the socket group.
	host.config.ServiceUser = current.Username
	err = BuildLayout(host.config).Enforce()
	if err == nil {
		t.Fatal("an application account inside the socket group was accepted")
	}
	if !strings.Contains(err.Error(), "socket group") {
		t.Fatalf("the failure must name the socket group, got %v", err)
	}
	if !strings.Contains(err.Error(), "connect to the deployment API") {
		t.Fatalf("the failure must say what the membership grants, got %v", err)
	}
}

func TestAnApplicationAccountThatDoesNotExistIsRefused(t *testing.T) {
	host := newHost(t)
	host.config.ServiceUser = "an-account-this-host-does-not-have"
	err := BuildLayout(host.config).Enforce()
	if err == nil {
		t.Fatal("a non-existent application account was accepted")
	}
	if !strings.Contains(err.Error(), "cannot be resolved") {
		t.Fatalf("the failure must say the account does not resolve, got %v", err)
	}
}

func TestAnApplicationAccountRunningAsRootIsRefused(t *testing.T) {
	host := newHost(t)
	host.config.ServiceUser = "root"
	err := BuildLayout(host.config).Enforce()
	if err == nil {
		t.Fatal("an application account with uid 0 was accepted")
	}
	if !strings.Contains(err.Error(), "uid 0") {
		t.Fatalf("the failure must name the privilege, got %v", err)
	}
}

func chmod(host *host, path string, mode os.FileMode) {
	host.t.Helper()
	if err := os.Chmod(path, mode); err != nil {
		host.t.Fatalf("chmod %s: %v", path, err)
	}
}
