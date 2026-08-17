package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"webtag/internal/deploybackup"
	"webtag/internal/releasetrust"
)

// Runner executes the update state machine for one job.
//
// The ordering rule that governs the whole file: every check that can refuse
// the update runs before PhaseQuiesce. Once the service is stopped the
// installation is degraded, and once the migration has run the database may
// have moved in ways that putting the old binaries back does not undo. So the
// expensive, network-bound, trust-establishing work all happens while the
// application is still serving, and the maintenance window contains only the
// steps that genuinely require the application to be down.
type Runner struct {
	config     Config
	store      *Store
	trust      ReleaseTrust
	downloader *Downloader
	commands   CommandRunner
	service    *ServiceControl
	health     *HealthClient
	hostOS     string
	hostArch   string
	now        func() time.Time
	// readyBudget and readyPoll bound the wait for the target release to
	// answer. They are fields rather than constants so the suite can exercise
	// the timeout path without spending the production budget on it; nothing
	// reads them from the environment.
	readyBudget time.Duration
	readyPoll   time.Duration
}

// readyWindow returns the configured wait, falling back to the production
// budget so a Runner built without them still behaves correctly.
func (runner *Runner) readyWindow() (time.Duration, time.Duration) {
	budget, poll := runner.readyBudget, runner.readyPoll
	if budget <= 0 {
		budget = ReadyTimeout
	}
	if poll <= 0 {
		poll = ReadyPollInterval
	}
	return budget, poll
}

// runState carries what one execution learned, including the facts a hold point
// has to report.
type runState struct {
	job      *Job
	verified *releasetrust.VerifiedRelease

	coreArchive    []byte
	readerArchive  []byte
	coreContents   *releasetrust.ArchiveContents
	readerContents *releasetrust.ArchiveContents

	migrateBinary string
	backupPath    string

	serviceStopped   bool
	databaseMigrated bool
	switched         bool
	previousCore     string
	previousReader   string
}

// holdError carries a named stop point up through the phase functions.
type holdError struct {
	class       HoldClass
	reason      string
	detail      string
	blockers    []Blocker
	remediation string
}

func (err *holdError) Error() string { return err.reason }

func holdf(class HoldClass, remediation string, cause error, format string, args ...any) *holdError {
	hold := &holdError{class: class, reason: fmt.Sprintf(format, args...), remediation: remediation}
	if cause != nil {
		hold.detail = cause.Error()
	}
	return hold
}

// Execute runs the state machine to completion. It never returns an error: a
// job that stopped is a recorded hold, not a caller's problem, because the
// caller is a detached goroutine whose HTTP request may be long gone.
func (runner *Runner) Execute(ctx context.Context, jobID string) {
	job, err := runner.store.Get(jobID)
	if err != nil {
		return
	}
	state := &runState{job: job}
	state.previousCore = readSymlink(runner.config.CoreCurrent())
	state.previousReader = readSymlink(runner.config.ReaderCurrent())
	runner.recordCurrentIdentity(ctx, jobID)

	phases := []struct {
		phase Phase
		run   func(context.Context, *runState) error
	}{
		{PhaseVerifyManifest, runner.verifyManifest},
		{PhaseDownload, runner.download},
		{PhaseVerifyArtifacts, runner.verifyArtifacts},
		{PhasePreflight, runner.preflight},
		{PhaseQuiesce, runner.quiesce},
		{PhaseBackup, runner.takeBackup},
		{PhaseMigrate, runner.migrate},
		{PhaseSwitch, runner.switchCurrent},
		{PhaseStart, runner.start},
	}
	for _, step := range phases {
		if err := runner.runPhase(ctx, jobID, state, step.phase, step.run); err != nil {
			return
		}
	}
	runner.finish(jobID, state)
}

func (runner *Runner) runPhase(
	ctx context.Context,
	jobID string,
	state *runState,
	phase Phase,
	run func(context.Context, *runState) error,
) error {
	started := runner.now()
	_ = runner.store.Update(jobID, func(job *Job) {
		job.Phase = phase
		job.Phases = append(job.Phases, PhaseRecord{Phase: phase, StartedAt: started})
	})

	err := run(ctx, state)
	finished := runner.now()
	if err == nil {
		_ = runner.store.Update(jobID, func(job *Job) {
			markPhaseFinished(job, phase, finished, true, "")
		})
		return nil
	}
	runner.recordHold(jobID, state, phase, finished, err)
	return err
}

func markPhaseFinished(job *Job, phase Phase, at time.Time, ok bool, note string) {
	for index := range job.Phases {
		if job.Phases[index].Phase == phase && job.Phases[index].FinishedAt == nil {
			finished := at
			job.Phases[index].FinishedAt = &finished
			job.Phases[index].OK = ok
			job.Phases[index].Note = note
			return
		}
	}
}

// recordHold writes the terminal stop point.
//
// The three booleans it copies out of runState are the ones that decide what a
// human has to do next, and they are recorded even when the underlying failure
// is mundane. "The update failed" is not actionable; "the update failed, the
// service is stopped, the database was migrated and the symlinks were not
// switched" is a runbook entry.
func (runner *Runner) recordHold(jobID string, state *runState, phase Phase, at time.Time, cause error) {
	hold := &Hold{
		Phase:            phase,
		Class:            HoldEnvironment,
		Reason:           cause.Error(),
		ServiceStopped:   state.serviceStopped,
		DatabaseMigrated: state.databaseMigrated,
		Switched:         state.switched,
		BackupPath:       state.backupPath,
	}
	var named *holdError
	if errors.As(cause, &named) {
		hold.Class = named.class
		hold.Reason = named.reason
		hold.Detail = named.detail
		hold.Blockers = named.blockers
		hold.Remediation = named.remediation
	}
	if hold.Remediation == "" {
		hold.Remediation = runner.defaultRemediation(state)
	}
	_ = runner.store.Update(jobID, func(job *Job) {
		markPhaseFinished(job, phase, at, false, hold.Reason)
		job.State = JobHold
		job.Hold = hold
		job.BackupPath = state.backupPath
		finished := at
		job.FinishedAt = &finished
	})
}

// defaultRemediation describes the recovery stance for a hold that did not name
// its own, which is decided by how far into the maintenance window it got.
func (runner *Runner) defaultRemediation(state *runState) string {
	switch {
	case !state.serviceStopped:
		return "The service was never stopped and the installation is unchanged. Resolve the reported problem and submit the update again."
	case state.databaseMigrated && !state.switched:
		return "The database was migrated but the binaries were not switched, so the schema is ahead of the running code. " +
			"Do not start the old release. Follow the recovery runbook bound to the dump recorded in backup_path."
	case state.switched:
		return "The binaries were switched but the target release did not become ready. Investigate the service before " +
			"reverting anything: the database has already moved and restoring the previous binaries alone is not a rollback."
	default:
		return "The service is stopped and the database was not migrated. Inspect the reported problem, then start the " +
			"service again to return to the previous release."
	}
}

func (runner *Runner) recordCurrentIdentity(ctx context.Context, jobID string) {
	probeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	report, err := runner.health.Health(probeCtx)
	if err != nil {
		return
	}
	_ = runner.store.Update(jobID, func(job *Job) {
		job.FromVersion = report.Version
		job.FromCommit = report.Commit
	})
}

func (runner *Runner) finish(jobID string, state *runState) {
	at := runner.now()
	_ = runner.store.Update(jobID, func(job *Job) {
		job.Phase = PhaseAudit
		job.Phases = append(job.Phases, PhaseRecord{Phase: PhaseAudit, StartedAt: at})
		markPhaseFinished(job, PhaseAudit, at, true, "")
		job.Phase = PhaseDone
		job.State = JobSucceeded
		job.BackupPath = state.backupPath
		finished := at
		job.FinishedAt = &finished
	})
	_ = runner.store.Release(jobID)
	_ = removeAllIfPresent(runner.config.CoreIncoming(state.job.Target))
	_ = removeAllIfPresent(runner.config.ReaderIncoming(state.job.Target))
}

// --- phase 2 and 4: fetch and verify the signed manifest -------------------

func (runner *Runner) verifyManifest(ctx context.Context, state *runState) error {
	target := state.job.Target
	// The tag is re-checked here even though the HTTP layer already rejected a
	// non-exact target. This function is the trust boundary; a boundary that
	// relies on a caller having checked is not a boundary.
	if !IsFormalTag(target) {
		return holdf(HoldTrust, "Submit an exact formal release tag such as v1.2.3.", nil,
			"%q is not an exact formal release tag", target)
	}
	fetchCtx, cancel := context.WithTimeout(ctx, DownloadTimeout)
	defer cancel()
	signed, err := runner.downloader.FetchSignedManifest(fetchCtx, target)
	if err != nil {
		return holdf(HoldEnvironment, "", err, "the signed manifest for %s could not be fetched", target)
	}
	verified, err := runner.trust.VerifyRelease(releasetrust.VerifyRequest{
		ManifestBytes:  signed.ManifestBytes,
		SignatureBytes: signed.SignatureBytes,
		ExpectedRepo:   runner.config.Repo,
		ExpectedTag:    target,
		HostOS:         runner.hostOS,
		HostArch:       runner.hostArch,
		HelperProtocol: releasetrust.HelperProtocol,
	})
	if err != nil {
		return holdf(HoldTrust,
			"Do not retry. The release assets for this tag do not verify against the compiled-in trust root; "+
				"treat this as a supply-chain event until it is explained.",
			err, "the release manifest for %s did not verify", target)
	}
	state.verified = verified

	manifest := verified.Manifest
	_ = runner.store.Update(state.job.ID, func(job *Job) {
		job.TargetCommit = manifest.Commit
		job.SchemaTarget = manifest.SchemaTarget
		job.RiverLedgerTarget = manifest.RiverLedgerTarget
		job.ManifestSHA256 = digestHex(signed.ManifestBytes)
		job.SignatureKeyID = verified.Key.KeyID
	})

	// The release itself declares whether a page-triggered update may apply it.
	// Refusing here rather than after the download is not just a saved transfer:
	// it keeps a release that was built with an unclassified migration step from
	// ever reaching the point where a bug could apply it.
	if !manifest.OnlineUpdateCompatible {
		return holdf(HoldPolicy,
			"This release must be installed by hand, following the manual upgrade runbook. The page cannot apply it.",
			nil, "release %s declares that it cannot be applied by a page-triggered update: %s",
			target, manifest.OnlineUpdateReason)
	}
	return nil
}

// --- phase 5: download ------------------------------------------------------

func (runner *Runner) download(ctx context.Context, state *runState) error {
	manifest := state.verified.Manifest
	core := state.verified.Core

	coreData, err := runner.downloader.Asset(ctx, manifest.Tag, core.Archive, core.SizeBytes)
	if err != nil {
		return holdf(HoldEnvironment, "", err, "the Core archive %s could not be downloaded", core.Archive)
	}
	readerData, err := runner.downloader.Asset(ctx, manifest.Tag, manifest.Reader.Archive, manifest.Reader.SizeBytes)
	if err != nil {
		return holdf(HoldEnvironment, "", err, "the Reader archive %s could not be downloaded", manifest.Reader.Archive)
	}
	state.coreArchive = coreData
	state.readerArchive = readerData
	return nil
}

// --- phase 6: verify every artifact ----------------------------------------

func (runner *Runner) verifyArtifacts(ctx context.Context, state *runState) error {
	manifest := state.verified.Manifest
	core := state.verified.Core
	supplyChain := "Do not retry. The downloaded assets do not match the signed manifest; treat this as a supply-chain " +
		"event until it is explained."

	coreContents, err := runner.trust.VerifyCoreArchive(state.coreArchive, core)
	if err != nil {
		return holdf(HoldTrust, supplyChain, err, "the Core archive for %s did not verify", manifest.Tag)
	}
	provenance, err := releasetrust.ReadArchiveFile(bytes.NewReader(state.coreArchive), core.ProvenancePath)
	if err != nil {
		return holdf(HoldTrust, supplyChain, err, "the Core archive for %s has no readable build provenance", manifest.Tag)
	}
	if err := runner.trust.VerifyCoreProvenance(provenance, manifest, core); err != nil {
		return holdf(HoldTrust, supplyChain, err, "the Core build provenance does not describe release %s", manifest.Tag)
	}
	readerContents, err := runner.trust.VerifyReaderArchive(state.readerArchive, manifest.Reader)
	if err != nil {
		return holdf(HoldTrust, supplyChain, err, "the Reader archive for %s did not verify", manifest.Tag)
	}
	state.coreContents = coreContents
	state.readerContents = readerContents

	if err := runner.unpack(state); err != nil {
		return err
	}
	return runner.verifyIdentities(ctx, state)
}

func (runner *Runner) unpack(state *runState) error {
	tag := state.verified.Manifest.Tag
	coreIncoming := runner.config.CoreIncoming(tag)
	readerIncoming := runner.config.ReaderIncoming(tag)
	for _, path := range []string{coreIncoming, readerIncoming} {
		if err := removeAllIfPresent(path); err != nil {
			return holdf(HoldEnvironment, "", err, "a previous attempt's staging directory could not be cleared")
		}
	}
	if err := extractArchive(state.coreArchive, state.coreContents, coreIncoming); err != nil {
		return holdf(HoldTrust, "", err, "the Core archive for %s could not be unpacked safely", tag)
	}
	if err := extractArchive(state.readerArchive, state.readerContents, readerIncoming); err != nil {
		return holdf(HoldTrust, "", err, "the Reader archive for %s could not be unpacked safely", tag)
	}
	return nil
}

// verifyIdentities runs both staged executables and compares their exact
// --version output with the signed manifest.
//
// The hash check already proved the file is the signed one. This proves
// something the hash cannot: that the file actually runs on this host and
// reports the identity the manifest promised. A cross-compiled archive for the
// wrong architecture, a truncated binary, or a missing dynamic dependency all
// pass a hash check and fail here — before the service is stopped rather than
// after.
func (runner *Runner) verifyIdentities(ctx context.Context, state *runState) error {
	core := state.verified.Core
	root := runner.config.CoreIncoming(state.verified.Manifest.Tag)
	for _, name := range releasetrust.ExecutableNames {
		executable, ok := core.Executables[name]
		if !ok {
			return holdf(HoldTrust, "", nil, "the signed manifest does not declare an executable named %q", name)
		}
		binary := root + "/" + executable.Path
		output, err := Identity(ctx, runner.commands, binary)
		if err != nil {
			return holdf(HoldTrust, "", err, "the staged %s executable could not report its identity", name)
		}
		if err := runner.trust.VerifyExecutableIdentity(name, output, core); err != nil {
			return holdf(HoldTrust,
				"Do not retry. A binary that hashes correctly but reports a different identity is not a packaging "+
					"mistake; treat it as a supply-chain event.",
				err, "the staged %s executable does not report the signed identity", name)
		}
		if name == "migrate" {
			state.migrateBinary = binary
		}
	}
	if state.migrateBinary == "" {
		return holdf(HoldTrust, "", nil, "the release does not ship a migrate executable")
	}
	return nil
}

// --- phase 7: preflight -----------------------------------------------------

func (runner *Runner) preflight(ctx context.Context, state *runState) error {
	if err := runner.checkDisk(); err != nil {
		return err
	}
	if err := runner.checkBackupTooling(ctx); err != nil {
		return err
	}
	return runner.checkMigrationPlan(ctx, state)
}

func (runner *Runner) checkDisk() error {
	for _, path := range []string{runner.config.ReleasesDir(), runner.config.BackupsDir()} {
		free, err := freeBytes(path)
		if err != nil {
			return holdf(HoldEnvironment, "", err, "the free space on %s could not be measured", path)
		}
		if free < runner.config.MinFreeBytes {
			return holdf(HoldEnvironment,
				"Free space on the deployment filesystem, then submit the update again. Nothing has been changed.",
				nil, "%s has %d bytes free, below the %d byte floor this update requires",
				path, free, runner.config.MinFreeBytes)
		}
	}
	return nil
}

// checkBackupTooling proves pg_dump and pg_restore exist and run before the
// service is stopped. Discovering a missing pg_dump after quiesce would mean an
// installation that is down with no backup and no way to take one.
func (runner *Runner) checkBackupTooling(ctx context.Context) error {
	probeCtx, cancel := context.WithTimeout(ctx, IdentityTimeout)
	defer cancel()
	if _, _, err := runner.backup().ToolVersions(probeCtx); err != nil {
		return holdf(HoldEnvironment,
			"Install a PostgreSQL client whose version matches the server, then submit the update again.",
			err, "the dump tooling is not usable on this host, so no verifiable backup can be taken")
	}
	return nil
}

// backup builds the dump driver for this run.
func (runner *Runner) backup() *deploybackup.Backup {
	return deploybackup.New(backupRunner{inner: runner.commands},
		runner.config.PgDump, runner.config.PgRestore, runner.config.DatabaseURL)
}

// checkMigrationPlan asks the target release whether this range may be applied.
//
// This is the step that must never move below quiesce. A manual gate exists
// because someone decided a person has to be present; discovering that after
// the service is stopped would mean an outage caused entirely by asking the
// question in the wrong order.
func (runner *Runner) checkMigrationPlan(ctx context.Context, state *runState) error {
	manifest := state.verified.Manifest
	migrator := NewMigrator(runner.commands, state.migrateBinary, runner.config.DatabaseURL)
	report, err := migrator.Plan(ctx, manifest.SchemaTarget)
	if err != nil {
		return holdf(HoldEnvironment,
			"The database could not be reached or the migration plan could not be evaluated. Nothing has been changed; "+
				"fix the connection or the tooling and submit the update again.",
			err, "the migration plan for schema target %s could not be evaluated", manifest.SchemaTarget)
	}
	if report.Error != nil {
		return holdf(HoldIntegrity,
			"Do not submit this update again until the ledger state is understood. The service has not been stopped.",
			nil, "the target release refused to plan the migration (%s): %s", report.Error.Kind, report.Error.Message)
	}
	plan := report.OnlineUpdate
	if plan == nil {
		return holdf(HoldPolicy,
			"This release cannot be applied from the page. Use the manual upgrade runbook.",
			nil, "the target release did not produce an online update decision for schema target %s", manifest.SchemaTarget)
	}
	if !plan.Allowed {
		return &holdError{
			class: HoldPolicy,
			reason: fmt.Sprintf("the migration range to %s contains %d step(s) a page-triggered update may not apply",
				manifest.SchemaTarget, len(plan.Blockers)),
			blockers: toBlockers(plan.Blockers),
			remediation: "Apply these steps by hand, following the runbook for each one. The service has not been " +
				"stopped and nothing on this host has changed.",
		}
	}
	return nil
}

func toBlockers(blockers []OnlineUpdateBlocker) []Blocker {
	out := make([]Blocker, 0, len(blockers))
	for _, blocker := range blockers {
		out = append(out, Blocker{
			StepID: blocker.StepID,
			Class:  blocker.Reason,
			Manual: blocker.Reason == "manual_gate",
			Reason: blocker.Detail,
		})
	}
	return out
}

// --- phase 8: quiesce -------------------------------------------------------

func (runner *Runner) quiesce(ctx context.Context, state *runState) error {
	if err := runner.service.Stop(ctx); err != nil {
		return holdf(HoldEnvironment,
			"The service could not be stopped, so no writes were fenced and nothing was migrated. Investigate the unit.",
			err, "%s could not be stopped", runner.config.ServiceUnit)
	}
	state.serviceStopped = true
	active, err := runner.service.Active(ctx)
	if err == nil && active {
		return holdf(HoldIntegrity,
			"The unit reports itself active after a stop. Do not continue: a migration against a database that is "+
				"still being written to is exactly the state this window exists to prevent.",
			nil, "%s is still active after being stopped", runner.config.ServiceUnit)
	}
	return nil
}

// --- phase 9: backup --------------------------------------------------------

func (runner *Runner) takeBackup(ctx context.Context, state *runState) error {
	stamp := runner.now().UTC().Format("20060102T150405Z")
	path := fmt.Sprintf("%s/%s-%s.dump", runner.config.BackupsDir(), state.verified.Manifest.Tag, stamp)
	backup := runner.backup()
	dumpCtx, cancel := context.WithTimeout(ctx, BackupTimeout)
	defer cancel()

	restart := "No usable backup exists, so the migration must not run. Start the service again to return to the " +
		"previous release, then investigate the dump failure."
	if err := backup.Dump(dumpCtx, path); err != nil {
		return holdf(HoldIntegrity, restart, err, "the pre-migration dump could not be written to %s", path)
	}
	// The dump path is recorded before it is validated. A dump that fails
	// validation is still a file on disk that an operator has to decide about,
	// and a hold that does not name it leaves them hunting.
	state.backupPath = path
	_ = runner.store.Update(state.job.ID, func(job *Job) { job.BackupPath = path })

	if err := backup.Verify(dumpCtx, path); err != nil {
		return holdf(HoldIntegrity, restart, err, "the pre-migration dump is not a restorable backup")
	}
	return nil
}

// --- phase 10: migrate ------------------------------------------------------

func (runner *Runner) migrate(ctx context.Context, state *runState) error {
	manifest := state.verified.Manifest
	migrator := NewMigrator(runner.commands, state.migrateBinary, runner.config.DatabaseURL)

	report, err := migrator.Apply(ctx, manifest.SchemaTarget)
	// Whether the process exited zero is not the question. A migration that
	// exits non-zero may still have committed several steps, and a migration
	// that exits zero has proved nothing until its own report says so. The
	// state flag is therefore set from what the report shows was applied, not
	// from the exit status.
	if report != nil && (len(report.Applied) > 0 || report.EndVersion != report.StartVersion) {
		state.databaseMigrated = true
	}
	if err != nil {
		return runner.migrationHold(report, err, manifest.SchemaTarget)
	}
	if report.Error != nil {
		return holdf(HoldIntegrity, runner.postMigrationRemediation(state), nil,
			"the migration to %s failed (%s): %s", manifest.SchemaTarget, report.Error.Kind, report.Error.Message)
	}
	if !report.OK {
		return holdf(HoldIntegrity, runner.postMigrationRemediation(state), nil,
			"the migration to %s reported that it did not succeed", manifest.SchemaTarget)
	}
	return runner.checkLedgers(state, report)
}

func (runner *Runner) migrationHold(report *MigrationReport, cause error, target string) *holdError {
	hold := holdf(HoldIntegrity,
		"The database may have moved partway. Do not start either release until the ledgers have been read by hand; "+
			"the recovery runbook is bound to the dump recorded in backup_path.",
		cause, "the migration to %s did not complete", target)
	if report != nil && len(report.Applied) > 0 {
		hold.reason = fmt.Sprintf("the migration to %s stopped after applying %d step(s), the last being %s",
			target, len(report.Applied), report.Applied[len(report.Applied)-1])
	}
	return hold
}

func (runner *Runner) postMigrationRemediation(state *runState) string {
	if state.databaseMigrated {
		return "The database moved but the binaries were not switched. Do not start the previous release against a " +
			"schema it does not know. Follow the recovery runbook bound to the dump recorded in backup_path."
	}
	return "The database was not changed. Start the service again to return to the previous release, then investigate."
}

// checkLedgers is the proof that the migration actually landed.
//
// Reading the report's own ok flag is not enough and neither is the exit code:
// both describe what the migration believes it did. The ledgers describe what
// the database contains. An overshoot is the one finding that cannot be fixed
// by migrating further — a forward-only system has no way back — so it is
// always an integrity hold rather than something to retry past.
func (runner *Runner) checkLedgers(state *runState, report *MigrationReport) error {
	manifest := state.verified.Manifest
	ledgers := report.Ledgers
	if ledgers == nil {
		return holdf(HoldIntegrity, runner.postMigrationRemediation(state), nil,
			"the migration produced no ledger reconciliation, so there is no evidence it reached %s", manifest.SchemaTarget)
	}
	if ledgers.Overshot() {
		return holdf(HoldIntegrity,
			"Stop. A ledger past its target means a different release already migrated this database. Migrating "+
				"further cannot correct it and neither can restoring binaries. Escalate before anything else runs.",
			nil, "a migration ledger has overshot its target (schema extra: %v, River extra: %v)",
			ledgers.Schema.Extra, ledgers.River.Extra)
	}
	if ledgers.Schema.Target != manifest.SchemaTarget {
		return holdf(HoldIntegrity, runner.postMigrationRemediation(state), nil,
			"the migration reconciled against schema target %q but the signed manifest names %q",
			ledgers.Schema.Target, manifest.SchemaTarget)
	}
	if ledgers.River.Target != manifest.RiverLedgerTarget {
		return holdf(HoldIntegrity, runner.postMigrationRemediation(state), nil,
			"the migration reconciled against River ledger target %d but the signed manifest names %d",
			ledgers.River.Target, manifest.RiverLedgerTarget)
	}
	if !ledgers.OK || !ledgers.Schema.AtTarget || !ledgers.River.AtTarget {
		return holdf(HoldIntegrity, runner.postMigrationRemediation(state), nil,
			"the migration ledgers did not reach their targets (schema missing: %v, River missing: %v)",
			ledgers.Schema.Missing, ledgers.River.Missing)
	}
	return nil
}

// --- phase 11: switch -------------------------------------------------------

func (runner *Runner) switchCurrent(_ context.Context, state *runState) error {
	tag := state.verified.Manifest.Tag
	coreRelease := runner.config.CoreRelease(tag)
	readerRelease := runner.config.ReaderRelease(tag)

	if err := installTree(runner.config.CoreIncoming(tag), coreRelease); err != nil {
		return holdf(HoldIntegrity, runner.postMigrationRemediation(state), err,
			"the verified Core tree for %s could not be installed", tag)
	}
	if err := installTree(runner.config.ReaderIncoming(tag), readerRelease); err != nil {
		return holdf(HoldIntegrity, runner.postMigrationRemediation(state), err,
			"the verified Reader tree for %s could not be installed", tag)
	}
	// The Core link moves first. If the Reader link fails after it, the backend
	// is the target release and the Reader is one version behind — a visible,
	// recoverable mismatch. The other order would leave a Reader talking to a
	// backend it was not built against, which fails in ways that look like data
	// corruption.
	coreTarget := coreRelease + "/" + state.verified.Core.PackageRoot
	if err := switchSymlink(runner.config.CoreCurrent(), coreTarget); err != nil {
		return holdf(HoldIntegrity, runner.postMigrationRemediation(state), err,
			"the Core current link could not be switched to %s", tag)
	}
	state.switched = true
	readerTarget := readerRelease + "/" + readerRootBuildDirectory(state.verified.Manifest.Reader)
	if err := switchSymlink(runner.config.ReaderCurrent(), readerTarget); err != nil {
		return holdf(HoldIntegrity,
			"The backend was switched but the root-domain Reader was not. The two are now on different commits. "+
				"Repoint the Reader link by hand before serving traffic.",
			err, "the Reader current link could not be switched to %s", tag)
	}
	return nil
}

// readerRootBuildDirectory names the build that serves the root domain. The
// embedded build is served by the binary itself and is never unpacked to disk.
func readerRootBuildDirectory(reader releasetrust.ReaderArtifact) string {
	for _, build := range reader.Builds {
		if build.Name == "root" {
			return build.Directory
		}
	}
	return "root"
}

// installTree moves a verified staging tree to its final path.
func installTree(incoming, destination string) error {
	if _, err := os.Stat(destination); err == nil {
		// The same tag was already installed. The tree is byte-identical by
		// construction — every file was hash-checked against the same signed
		// manifest — so the existing one is kept and the staging copy dropped.
		return removeAllIfPresent(incoming)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect %s: %w", destination, err)
	}
	if err := os.Rename(incoming, destination); err != nil {
		return fmt.Errorf("move %s into place as %s: %w", incoming, destination, err)
	}
	return syncDir(filepathDir(destination))
}

func filepathDir(path string) string {
	for index := len(path) - 1; index > 0; index-- {
		if path[index] == '/' {
			return path[:index]
		}
	}
	return "/"
}

// --- phase 12: start and prove identity ------------------------------------

func (runner *Runner) start(ctx context.Context, state *runState) error {
	if err := runner.service.Start(ctx); err != nil {
		return holdf(HoldIntegrity, runner.rollbackStance(state), err,
			"%s could not be started after the switch", runner.config.ServiceUnit)
	}
	budget, poll := runner.readyWindow()
	err := runner.health.AwaitTarget(ctx, state.verified.Manifest.Commit, poll, budget)
	if err == nil {
		return nil
	}
	if errors.Is(err, ErrIdentityMismatch) {
		return holdf(HoldIntegrity, runner.rollbackStance(state), err,
			"the service is answering but reports a commit other than %s, so the target release is not the one running",
			state.verified.Manifest.Commit)
	}
	return holdf(HoldIntegrity, runner.rollbackStance(state), err,
		"the target release did not become ready after the switch")
}

// rollbackStance decides what the helper is willing to say about going back.
//
// It only offers the binary-swap option when the signed manifest proves the
// release changed neither ledger. Offering it otherwise would be advice to run
// old code against a schema it does not know, which is a data-loss instruction
// dressed as a recovery step.
func (runner *Runner) rollbackStance(state *runState) string {
	manifest := state.verified.Manifest
	if manifest.RollbackCompatible && state.previousCore != "" {
		return fmt.Sprintf("This release changed neither migration ledger, so restoring the previous binaries is a "+
			"complete rollback: repoint %s to %s and %s to %s, then start the service. (%s)",
			runner.config.CoreCurrent(), state.previousCore,
			runner.config.ReaderCurrent(), state.previousReader, manifest.RollbackReason)
	}
	return fmt.Sprintf("Binary-only rollback is not safe for this release: %s. The database has already been "+
		"migrated. Keep the service stopped and follow the recovery runbook bound to the dump recorded in "+
		"backup_path.", manifest.RollbackReason)
}
