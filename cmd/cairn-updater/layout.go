package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/user"
	"strconv"
	"strings"
	"syscall"
)

// The directory and permission model of issue #41.
//
// The model is a single claim: nothing the application user can write may
// influence what runs after the next restart, and nothing it can write may
// destroy the evidence needed to recover. Everything else here is bookkeeping
// in service of that claim.
//
// The helper checks the model at startup instead of trusting the installer,
// because the two are separate deliverables that drift. A helper that starts on
// a host where /opt/webtag/releases became group-writable would keep reporting
// healthy updates while the guarantee it exists to provide is already gone. So
// a failed check is a refusal to start, naming the single requirement that
// failed — not a warning in a log nobody reads.

// pathKind distinguishes the requirements that apply to a path.
type pathKind int

const (
	// kindOwnedDir is a directory the helper owns and may create.
	kindOwnedDir pathKind = iota
	// kindPrivateDir is helper-private state: owner access only.
	kindPrivateDir
	// kindSharedFile is a file the helper does not own the contents of but
	// whose ownership and mode it is responsible for asserting.
	kindSharedFile
)

// pathRequirement is one line of the permission model.
type pathRequirement struct {
	Path string
	Kind pathKind
	// Mode is the exact permission bit pattern the path must carry.
	Mode fs.FileMode
	// GroupName is the group the path must belong to. Empty means "the owner's
	// primary group", which for root is root.
	GroupName string
	// Create says the helper may create a missing path. A path it may not
	// create and does not find is a refusal to start: /opt/webtag/.env missing
	// means this host was never installed, and inventing an empty one would
	// start the application with no configuration.
	Create bool
	// Why records what the requirement protects against, so a failure message
	// can say more than "wrong mode".
	Why string
}

// Layout is the resolved permission model for one configuration.
type Layout struct {
	OwnerUID     int
	Requirements []pathRequirement
	SocketPath   string
	SocketGroup  string
	// ServiceUser is the account the application runs as. The layout does not
	// grant it anything; it checks that the account has not been granted
	// something elsewhere.
	ServiceUser string
}

// BuildLayout turns a Config into the permission model the helper enforces.
func BuildLayout(config Config) Layout {
	coreEnvGroup := config.CoreEnvGroup
	return Layout{
		OwnerUID:    config.OwnerUID,
		SocketPath:  config.SocketPath,
		SocketGroup: config.SocketGroup,
		ServiceUser: config.ServiceUser,
		Requirements: []pathRequirement{
			{
				Path: config.CoreDir, Kind: kindOwnedDir, Mode: 0o755, Create: true,
				Why: "the application user must not be able to add or rename anything beside its own release tree",
			},
			{
				Path: config.ReleasesDir(), Kind: kindOwnedDir, Mode: 0o755, Create: true,
				Why: "a writable releases directory lets an application RCE choose the program that runs after the next restart",
			},
			{
				Path: config.BackupsDir(), Kind: kindOwnedDir, Mode: 0o755, Create: true,
				Why: "a writable backups directory lets the same RCE destroy the dump the recovery runbook depends on",
			},
			{
				Path: config.ReaderDir, Kind: kindOwnedDir, Mode: 0o755, Create: true,
				Why: "the root-domain Reader is served straight from disk, so write access to it is stored XSS on the deployment origin",
			},
			{
				Path: config.ReaderReleasesDir(), Kind: kindOwnedDir, Mode: 0o755, Create: true,
				Why: "same reason as the Reader root: the switch target must not be writable by the application",
			},
			{
				Path: config.CoreEnvFile(), Kind: kindSharedFile, Mode: 0o640, GroupName: coreEnvGroup, Create: false,
				Why: "the application reads its own configuration but must not be able to rewrite it or read it from another account",
			},
			{
				Path: config.HelperEnv, Kind: kindSharedFile, Mode: 0o600, Create: false,
				Why: "this file holds DEPLOY_AUTH_TOKEN; anything wider than owner-only hands out deployment authority",
			},
			{
				Path: config.StateDir, Kind: kindPrivateDir, Mode: 0o700, Create: true,
				Why: "job records name exact targets and hold points and are the helper's own memory",
			},
			{
				Path: config.JobsDir(), Kind: kindPrivateDir, Mode: 0o700, Create: true,
				Why: "job status has to survive the Core being stopped, so it is on disk and only the helper may touch it",
			},
		},
	}
}

// LayoutViolation names one failed requirement.
type LayoutViolation struct {
	Path   string
	Detail string
	Why    string
}

func (violation LayoutViolation) Error() string {
	return fmt.Sprintf("%s: %s (%s)", violation.Path, violation.Detail, violation.Why)
}

// Enforce creates the paths the helper owns and verifies every requirement.
//
// It returns every violation it found rather than the first, because an
// operator fixing a freshly installed host should get the whole list in one
// pass instead of restarting the service once per wrong mode.
func (layout Layout) Enforce() error {
	var violations []error
	for _, requirement := range layout.Requirements {
		if err := layout.enforceOne(requirement); err != nil {
			violations = append(violations, err)
		}
	}
	if err := layout.verifyServiceUserIsFenced(); err != nil {
		violations = append(violations, err)
	}
	if len(violations) == 0 {
		return nil
	}
	return fmt.Errorf("the deployment layout does not satisfy its permission model:\n  %s",
		strings.Join(errorLines(violations), "\n  "))
}

func errorLines(errs []error) []string {
	lines := make([]string, 0, len(errs))
	for _, err := range errs {
		lines = append(lines, err.Error())
	}
	return lines
}

func (layout Layout) enforceOne(requirement pathRequirement) error {
	info, err := os.Lstat(requirement.Path)
	switch {
	case errors.Is(err, os.ErrNotExist) && requirement.Create:
		if err := os.MkdirAll(requirement.Path, requirement.Mode); err != nil {
			return LayoutViolation{Path: requirement.Path, Detail: "cannot be created: " + err.Error(), Why: requirement.Why}
		}
		// MkdirAll applies the process umask, so the mode above is a ceiling
		// rather than the result. Set it explicitly: a 0o755 request that
		// silently became 0o750 under a strict umask would make Caddy unable to
		// read the Reader tree, and a lax umask would leave it group-writable.
		if err := os.Chmod(requirement.Path, requirement.Mode); err != nil {
			return LayoutViolation{Path: requirement.Path, Detail: "mode cannot be set: " + err.Error(), Why: requirement.Why}
		}
		info, err = os.Lstat(requirement.Path)
		if err != nil {
			return LayoutViolation{Path: requirement.Path, Detail: "cannot be inspected after creation: " + err.Error(), Why: requirement.Why}
		}
	case errors.Is(err, os.ErrNotExist):
		return LayoutViolation{Path: requirement.Path, Detail: "does not exist and the helper must not invent it", Why: requirement.Why}
	case err != nil:
		return LayoutViolation{Path: requirement.Path, Detail: "cannot be inspected: " + err.Error(), Why: requirement.Why}
	}
	return layout.verify(requirement, info)
}

func (layout Layout) verify(requirement pathRequirement, info fs.FileInfo) error {
	violate := func(format string, args ...any) error {
		return LayoutViolation{Path: requirement.Path, Detail: fmt.Sprintf(format, args...), Why: requirement.Why}
	}
	// A symlink is checked before its type: an attacker who can replace a
	// directory with a symlink gets everything the directory was trusted for,
	// and Lstat is the only call that can see the difference.
	if info.Mode()&fs.ModeSymlink != 0 {
		return violate("is a symlink; a deployment path must be a real directory or file")
	}
	wantDir := requirement.Kind != kindSharedFile
	if info.IsDir() != wantDir {
		if wantDir {
			return violate("is not a directory")
		}
		return violate("is not a regular file")
	}
	if got := info.Mode().Perm(); got != requirement.Mode {
		return violate("has mode %04o, the model requires %04o", got, requirement.Mode)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return violate("ownership cannot be read on this platform; the helper only supports Linux hosts")
	}
	if int(stat.Uid) != layout.OwnerUID {
		return violate("is owned by uid %d, the model requires uid %d", stat.Uid, layout.OwnerUID)
	}
	if requirement.GroupName == "" {
		return nil
	}
	wantGID, err := lookupGroupID(requirement.GroupName)
	if err != nil {
		return violate("group %q cannot be resolved: %v", requirement.GroupName, err)
	}
	if int(stat.Gid) != wantGID {
		return violate("belongs to gid %d, the model requires group %q (gid %d)", stat.Gid, requirement.GroupName, wantGID)
	}
	return nil
}

// verifyServiceUserIsFenced proves the application account has not been handed
// the two privileges that would undo this whole design.
//
// The first is uid 0: an application running as root does not need to break out
// of anything. The second is membership of the socket group, which is the
// mechanism the deployment API is reachable through. Both are granted
// elsewhere — in the unit file, in /etc/group — and both are silent. A helper
// that only checked file modes would report a perfectly hardened directory tree
// on a host where the application can simply connect to the socket and ask for
// a deployment.
func (layout Layout) verifyServiceUserIsFenced() error {
	if layout.ServiceUser == "" {
		return nil
	}
	violate := func(format string, args ...any) error {
		return LayoutViolation{
			Path:   "user " + layout.ServiceUser,
			Detail: fmt.Sprintf(format, args...),
			Why:    "owning the application must not be enough to replace it",
		}
	}
	account, err := user.Lookup(layout.ServiceUser)
	if err != nil {
		return violate("cannot be resolved: %v", err)
	}
	if account.Uid == "0" {
		return violate("is uid 0; an application running as root has nothing to be fenced from")
	}
	socketGID, err := lookupGroupID(layout.SocketGroup)
	if err != nil {
		return violate("cannot be checked because socket group %q does not resolve: %v", layout.SocketGroup, err)
	}
	groupIDs, err := account.GroupIds()
	if err != nil {
		return violate("group membership cannot be read: %v", err)
	}
	for _, raw := range groupIDs {
		gid, convErr := strconv.Atoi(raw)
		if convErr != nil {
			continue
		}
		if gid == socketGID {
			return violate("is a member of the socket group %q (gid %d), so it can connect to the deployment API",
				layout.SocketGroup, socketGID)
		}
	}
	return nil
}

// lookupGroupID resolves a group name to a gid, accepting a numeric id so a
// container image without a full group database can still be configured.
func lookupGroupID(name string) (int, error) {
	if numeric, err := strconv.Atoi(name); err == nil {
		return numeric, nil
	}
	group, err := user.LookupGroup(name)
	if err != nil {
		return 0, err
	}
	gid, err := strconv.Atoi(group.Gid)
	if err != nil {
		return 0, fmt.Errorf("group %q has non-numeric gid %q: %w", name, group.Gid, err)
	}
	return gid, nil
}
