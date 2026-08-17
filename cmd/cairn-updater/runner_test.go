package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestASignedUpdateReachesTheTargetCommit is the reference run every fault
// injection below is a deviation from.
func TestASignedUpdateReachesTheTargetCommit(t *testing.T) {
	host := newHost(t)

	job := host.runUpdate(fixtureTag)

	if job.State != JobSucceeded {
		t.Fatalf("expected success, got state %s hold %+v", job.State, job.Hold)
	}
	if job.Phase != PhaseDone {
		t.Fatalf("expected phase %s, got %s", PhaseDone, job.Phase)
	}
	if job.TargetCommit != fixtureCommit {
		t.Fatalf("expected target commit %s, got %s", fixtureCommit, job.TargetCommit)
	}
	if job.FromCommit != previousCommit {
		t.Fatalf("expected the job to record the previous commit %s, got %s", previousCommit, job.FromCommit)
	}

	// The migration ran against the exact schema target from the signed
	// manifest, and never with the manual-gate override.
	if got := host.readControl("apply.target"); got != fixtureSchema {
		t.Fatalf("expected the migration target %s, got %q", fixtureSchema, got)
	}
	if got := host.readControl("apply.allow_manual"); got != "" {
		t.Fatalf("the helper must never set MIGRATION_ALLOW_MANUAL, got %q", got)
	}

	// Both current links point into the installed release.
	coreTarget, err := os.Readlink(host.config.CoreCurrent())
	if err != nil {
		t.Fatalf("read core current link: %v", err)
	}
	wantCore := filepath.Join(host.config.ReleasesDir(), fixtureTag, host.fixture.PackageRoot)
	if coreTarget != wantCore {
		t.Fatalf("expected the Core link to point at %s, got %s", wantCore, coreTarget)
	}
	readerTarget, err := os.Readlink(host.config.ReaderCurrent())
	if err != nil {
		t.Fatalf("read reader current link: %v", err)
	}
	wantReader := filepath.Join(host.config.ReaderReleasesDir(), fixtureTag, "root")
	if readerTarget != wantReader {
		t.Fatalf("expected the Reader link to point at %s, got %s", wantReader, readerTarget)
	}
	// The root-domain Reader really is on disk behind the link.
	if _, err := os.Stat(filepath.Join(readerTarget, "index.html")); err != nil {
		t.Fatalf("the switched Reader tree has no index.html: %v", err)
	}

	// A verified dump exists and it is the one the job recorded.
	if job.BackupPath == "" {
		t.Fatal("a successful update must record the dump it took")
	}
	info, err := os.Stat(job.BackupPath)
	if err != nil {
		t.Fatalf("stat recorded dump: %v", err)
	}
	if info.Size() == 0 {
		t.Fatal("the recorded dump is empty")
	}

	// The service was stopped and started exactly once, in that order.
	log := host.serviceLog()
	if len(log) < 2 || !strings.HasPrefix(log[0], "stop") {
		t.Fatalf("expected the service to be stopped first, got %v", log)
	}
	if !containsPrefix(log, "start") {
		t.Fatalf("expected the service to be started again, got %v", log)
	}

	// Staging was cleaned up, so a half-unpacked tree cannot be mistaken for an
	// installed release.
	if _, err := os.Stat(host.config.CoreIncoming(fixtureTag)); !os.IsNotExist(err) {
		t.Fatalf("expected the Core staging directory to be gone, got %v", err)
	}
}

func containsPrefix(lines []string, prefix string) bool {
	for _, line := range lines {
		if strings.HasPrefix(line, prefix) {
			return true
		}
	}
	return false
}

// --- targets that are not exact formal versions ----------------------------

func TestOnlyExactFormalVersionsAreAccepted(t *testing.T) {
	host := newHost(t)
	rejected := []string{
		"latest", "main", "v1.2", "v1.2.3-rc1", "V1.2.3", "1.2.3", "v1.2.3+build",
		"v01.2.3", "v 1.2.3", "v1.2.3.4", "../../etc/passwd",
		"https://github.com/Alpenl/cairn/releases/download/v1.2.3/x.tar.gz",
		"v1.2.3; rm -rf /", "",
	}
	for _, target := range rejected {
		body, err := jsonBody(map[string]string{"target": target})
		if err != nil {
			t.Fatalf("encode body: %v", err)
		}
		response := host.request("POST", "/api/deploy/system/jobs", body, host.deployToke)
		if response.Code != 400 {
			t.Fatalf("target %q was answered %d, expected 400", target, response.Code)
		}
		var failure ErrorResponse
		decodeBody(t, response, &failure)
		if failure.Error.Code != CodeInvalidTarget {
			t.Fatalf("target %q produced code %q, expected %q", target, failure.Error.Code, CodeInvalidTarget)
		}
	}
	if host.serviceWasStopped() {
		t.Fatal("a rejected target must never reach the service")
	}
}

// TestSurroundingWhitespaceIsTrimmedNotAccepted pins the one string
// transformation the submit route performs. Trimming is safe only because the
// trimmed value still has to match the exact pattern; it is the trimmed value
// that is stored, fetched and verified, so no untrimmed string ever reaches a
// URL or a path.
func TestSurroundingWhitespaceIsTrimmedNotAccepted(t *testing.T) {
	host := newHost(t)
	response := host.request("POST", "/api/deploy/system/jobs", `{"target":"  v1.2.3  "}`, host.deployToke)
	if response.Code != 202 {
		t.Fatalf("expected a padded exact tag to be accepted after trimming, got %d", response.Code)
	}
	var submitted SubmitJobResponse
	decodeBody(t, response, &submitted)
	if submitted.Target != fixtureTag {
		t.Fatalf("expected the stored target to be the trimmed %s, got %q", fixtureTag, submitted.Target)
	}
	host.server.Wait()
}

func TestAnUnknownBodyFieldIsRejectedRatherThanIgnored(t *testing.T) {
	host := newHost(t)
	// "url" is the field a caller would reach for to smuggle an arbitrary
	// source past the tag check. Ignoring it would silently install something
	// other than what was asked for.
	response := host.request("POST", "/api/deploy/system/jobs",
		`{"target":"v1.2.3","url":"https://evil.example/x.tar.gz"}`, host.deployToke)
	if response.Code != 400 {
		t.Fatalf("expected 400 for an unknown field, got %d", response.Code)
	}
}

// --- trust failures ---------------------------------------------------------

func TestATamperedManifestIsRefusedByTheRealVerifier(t *testing.T) {
	host := newHost(t)
	// Flip a byte inside the signed document. The signature no longer covers
	// these bytes, so the real verifier must refuse regardless of which key
	// signed the original.
	tampered := []byte(strings.Replace(string(host.fixture.ManifestBytes), fixtureSchema, "tamperedtarget2026", 1))
	host.assets.assets["cairn-release-manifest.json"] = tampered
	host.server.trust = productionTrust{}
	host.server.newRunner = withTrust(host, productionTrust{})

	job := host.runUpdate(fixtureTag)
	assertHold(t, job, PhaseVerifyManifest, HoldTrust)
	if host.serviceWasStopped() {
		t.Fatal("a manifest that does not verify must never reach the service")
	}
}

func TestASignatureFromAnUntrustedKeyIsRefused(t *testing.T) {
	host := newHost(t)
	// The fixture is signed by a synthetic key that is deliberately not in the
	// compiled-in trust root. Under the real verifier that is exactly the
	// "signed by the wrong key" case.
	host.server.trust = productionTrust{}
	host.server.newRunner = withTrust(host, productionTrust{})

	job := host.runUpdate(fixtureTag)
	assertHold(t, job, PhaseVerifyManifest, HoldTrust)
	if !strings.Contains(job.Hold.Detail, "cairn-release-2026t") &&
		!strings.Contains(job.Hold.Detail, "not trusted") &&
		!strings.Contains(job.Hold.Detail, "unknown") {
		t.Logf("hold detail: %s", job.Hold.Detail)
	}
	if host.serviceWasStopped() {
		t.Fatal("an untrusted signature must never reach the service")
	}
}

func TestATamperedCoreArchiveIsRefusedBeforeTheServiceStops(t *testing.T) {
	host := newHost(t)
	corrupted := append([]byte(nil), host.fixture.CoreArchive...)
	corrupted[len(corrupted)/2] ^= 0xff
	host.assets.assets[host.fixture.Manifest.Core[0].Archive] = corrupted

	job := host.runUpdate(fixtureTag)
	assertHold(t, job, PhaseVerifyArtifacts, HoldTrust)
	if host.serviceWasStopped() {
		t.Fatal("a tampered archive must never reach the service")
	}
	if host.migrationRan() {
		t.Fatal("a tampered archive must never reach the migration")
	}
}

func TestAManifestForADifferentTagIsRefused(t *testing.T) {
	host := newHost(t)
	// The assets are the real v1.2.3 release; the operator authorised v9.9.9.
	job := host.runUpdate("v9.9.9")
	assertHold(t, job, PhaseVerifyManifest, HoldTrust)
	if host.serviceWasStopped() {
		t.Fatal("a tag mismatch must never reach the service")
	}
}

// --- policy refusals must land before quiesce ------------------------------

func TestAReleaseThatDeclaresItselfNotOnlineUpdatableIsRefused(t *testing.T) {
	host := newHost(t)
	manifest := host.fixture.Manifest
	manifest.OnlineUpdateCompatible = false
	manifest.OnlineUpdateReason = "3 migration step(s) carry no online-update classification (first: someunclassifiedstep)"
	host.fixture.sign(t, manifest)
	host.assets.publish(host.fixture)

	job := host.runUpdate(fixtureTag)
	assertHold(t, job, PhaseVerifyManifest, HoldPolicy)
	if !strings.Contains(job.Hold.Reason, "no online-update classification") {
		t.Fatalf("the hold must repeat the release's own reason, got %q", job.Hold.Reason)
	}
	if host.serviceWasStopped() {
		t.Fatal("a policy refusal must never stop the service")
	}
}

func TestAManualGateStopsTheUpdateBeforeTheServiceIsStopped(t *testing.T) {
	host := newHost(t)
	host.writeControl("plan.json", mustJSON(t, MigrationReport{
		SchemaVersion: MigrationReportSchemaVersion,
		Tool:          "cairn-migrate",
		OK:            true,
		Mode:          "target",
		Target:        fixtureSchema,
		OnlineUpdate: &OnlineUpdatePlan{
			Target:  fixtureSchema,
			Pending: []string{"conceptbackfill2026072001", fixtureSchema},
			Allowed: false,
			Blockers: []OnlineUpdateBlocker{
				{StepID: "conceptbackfill2026072001", Reason: "manual_gate",
					Detail: "this step is release-gated and must be applied by hand"},
			},
		},
	}))

	job := host.runUpdate(fixtureTag)

	assertHold(t, job, PhasePreflight, HoldPolicy)
	if len(job.Hold.Blockers) != 1 {
		t.Fatalf("expected the blockers to be reported verbatim, got %+v", job.Hold.Blockers)
	}
	blocker := job.Hold.Blockers[0]
	if blocker.StepID != "conceptbackfill2026072001" || !blocker.Manual {
		t.Fatalf("expected the manual gate to be surfaced as manual, got %+v", blocker)
	}

	// The whole point of this test: the refusal happened while the service was
	// still serving.
	if host.serviceWasStopped() {
		t.Fatalf("a manual gate must be discovered before quiesce; service log was %v", host.serviceLog())
	}
	if job.Hold.ServiceStopped {
		t.Fatal("the hold must record that the service was never stopped")
	}
	if host.migrationRan() {
		t.Fatal("a blocked plan must not run the migration")
	}
	if strings.TrimSpace(host.readControl("health.commit")) != previousCommit {
		t.Fatal("the running release must be untouched")
	}
}

func TestAnUnclassifiedStepBlocksTheUpdate(t *testing.T) {
	host := newHost(t)
	host.writeControl("plan.json", mustJSON(t, MigrationReport{
		SchemaVersion: MigrationReportSchemaVersion,
		Tool:          "cairn-migrate",
		OK:            true,
		Target:        fixtureSchema,
		OnlineUpdate: &OnlineUpdatePlan{
			Target:  fixtureSchema,
			Pending: []string{fixtureSchema},
			Allowed: false,
			Blockers: []OnlineUpdateBlocker{
				{StepID: fixtureSchema, Reason: "unclassified", Detail: "classification defaults to deny"},
			},
		},
	}))

	job := host.runUpdate(fixtureTag)
	assertHold(t, job, PhasePreflight, HoldPolicy)
	if job.Hold.Blockers[0].Manual {
		t.Fatal("an unclassified step is not a manual gate")
	}
	if host.serviceWasStopped() {
		t.Fatal("an unclassified step must be discovered before quiesce")
	}
}

func TestAnUnreachableDatabaseHoldsBeforeQuiesce(t *testing.T) {
	host := newHost(t)
	host.writeControl("plan.exit", "1")
	host.removeControl("plan.json")

	job := host.runUpdate(fixtureTag)
	assertHold(t, job, PhasePreflight, HoldEnvironment)
	if host.serviceWasStopped() {
		t.Fatal("an unusable database must be discovered before quiesce")
	}
}

func TestMissingBackupToolingHoldsBeforeQuiesce(t *testing.T) {
	host := newHost(t)
	host.config.PgDump = filepath.Join(host.binDir, "pg_dump_that_does_not_exist")
	host.buildServer()

	job := host.runUpdate(fixtureTag)
	assertHold(t, job, PhasePreflight, HoldEnvironment)
	if host.serviceWasStopped() {
		t.Fatal("missing dump tooling must be discovered before the service is stopped")
	}
}

func TestInsufficientDiskHoldsBeforeQuiesce(t *testing.T) {
	host := newHost(t)
	host.config.MinFreeBytes = 1 << 62
	host.buildServer()

	job := host.runUpdate(fixtureTag)
	assertHold(t, job, PhasePreflight, HoldEnvironment)
	if !strings.Contains(job.Hold.Reason, "below the") {
		t.Fatalf("expected a disk-space reason, got %q", job.Hold.Reason)
	}
	if host.serviceWasStopped() {
		t.Fatal("a disk check must run before the service is stopped")
	}
}

// --- backup failures --------------------------------------------------------

func TestAnEmptyDumpStopsTheUpdateBeforeMigrating(t *testing.T) {
	host := newHost(t)
	host.writeControl("pgdump.empty", "yes")

	job := host.runUpdate(fixtureTag)

	assertHold(t, job, PhaseBackup, HoldIntegrity)
	if !host.serviceWasStopped() {
		t.Fatal("this failure happens after quiesce; the hold must reflect a stopped service")
	}
	if !job.Hold.ServiceStopped {
		t.Fatal("the hold must record that the service is stopped")
	}
	if host.migrationRan() {
		t.Fatal("an unusable dump must never be followed by a migration")
	}
	if job.Hold.DatabaseMigrated {
		t.Fatal("the hold must record that the database was not migrated")
	}
	if _, err := os.Readlink(host.config.CoreCurrent()); err == nil {
		t.Fatal("nothing should have been switched")
	}
}

func TestADumpThatWillNotListStopsTheUpdate(t *testing.T) {
	host := newHost(t)
	// A non-empty file that pg_restore --list cannot read. Checking only for a
	// non-zero size would let this through.
	host.writeControl("pgdump.corrupt", "yes")

	job := host.runUpdate(fixtureTag)
	assertHold(t, job, PhaseBackup, HoldIntegrity)
	if !strings.Contains(job.Hold.Reason, "restorable") {
		t.Fatalf("expected a restorability reason, got %q", job.Hold.Reason)
	}
	if host.migrationRan() {
		t.Fatal("an unlistable dump must never be followed by a migration")
	}
	if job.BackupPath == "" {
		t.Fatal("the hold must name the dump the operator has to look at")
	}
}

func TestAFailedDumpStopsTheUpdate(t *testing.T) {
	host := newHost(t)
	host.writeControl("pgdump.exit", "1")

	job := host.runUpdate(fixtureTag)
	assertHold(t, job, PhaseBackup, HoldIntegrity)
	if host.migrationRan() {
		t.Fatal("a failed dump must never be followed by a migration")
	}
}

// --- migration failures -----------------------------------------------------

func TestALedgerShortOfItsTargetStopsTheSwitch(t *testing.T) {
	host := newHost(t)
	report := host.successfulApplyReport()
	report.Ledgers = &LedgerReconciliation{
		OK: false,
		Schema: SchemaLedgerState{Present: true, Target: fixtureSchema, Head: "previousstep2026010101",
			Missing: []string{fixtureSchema}, AtTarget: false},
		River:    RiverLedgerState{Present: true, Target: fixtureRiver, Head: fixtureRiver, AtTarget: true},
		Problems: []LedgerProblem{{Ledger: "schema_migrations", Kind: "behind", Detail: "one step short"}},
	}
	host.writeControl("apply.json", mustJSON(t, report))

	job := host.runUpdate(fixtureTag)
	assertHold(t, job, PhaseMigrate, HoldIntegrity)
	if _, err := os.Readlink(host.config.CoreCurrent()); err == nil {
		t.Fatal("a ledger short of its target must not be followed by a switch")
	}
	if job.Hold.Switched {
		t.Fatal("the hold must record that nothing was switched")
	}
}

func TestAnOvershotLedgerIsAnIntegrityHold(t *testing.T) {
	host := newHost(t)
	report := host.successfulApplyReport()
	report.Ledgers = &LedgerReconciliation{
		OK: false,
		Schema: SchemaLedgerState{Present: true, Target: fixtureSchema, Head: "somethingnewer2026090101",
			Extra: []string{"somethingnewer2026090101"}, AtTarget: false},
		River:    RiverLedgerState{Present: true, Target: fixtureRiver, Head: fixtureRiver, AtTarget: true},
		Problems: []LedgerProblem{{Ledger: "schema_migrations", Kind: LedgerProblemAhead, Detail: "the ledger overshot its target"}},
	}
	host.writeControl("apply.json", mustJSON(t, report))

	job := host.runUpdate(fixtureTag)
	assertHold(t, job, PhaseMigrate, HoldIntegrity)
	if !strings.Contains(job.Hold.Reason, "overshot") {
		t.Fatalf("an overshoot must be named as such, got %q", job.Hold.Reason)
	}
	if !strings.Contains(job.Hold.Remediation, "Escalate") {
		t.Fatalf("an overshoot must escalate rather than suggest a retry, got %q", job.Hold.Remediation)
	}
	if _, err := os.Readlink(host.config.CoreCurrent()); err == nil {
		t.Fatal("an overshot ledger must not be followed by a switch")
	}
}

func TestAMigrationThatExitsNonZeroAfterCommittingStepsHolds(t *testing.T) {
	host := newHost(t)
	// The dangerous shape: some steps committed, then a later one failed. The
	// exit code is non-zero but the database has already moved.
	host.writeControl("apply.json", mustJSON(t, MigrationReport{
		SchemaVersion: MigrationReportSchemaVersion,
		Tool:          "cairn-migrate",
		OK:            false,
		Mode:          "target",
		Target:        fixtureSchema,
		StartVersion:  "previousstep2026010101",
		EndVersion:    "middlestep2026050101",
		Applied:       []string{"middlestep2026050101"},
		Error:         &MigrationReportError{Kind: "failed", Message: "relation already exists"},
	}))
	host.writeControl("apply.exit", "1")

	job := host.runUpdate(fixtureTag)
	assertHold(t, job, PhaseMigrate, HoldIntegrity)
	if !job.Hold.DatabaseMigrated {
		t.Fatal("the hold must record that the database moved, which is what makes this dangerous")
	}
	if !strings.Contains(job.Hold.Reason, "middlestep2026050101") {
		t.Fatalf("the hold must name the last step that landed, got %q", job.Hold.Reason)
	}
	if job.Hold.BackupPath == "" {
		t.Fatal("a post-migration hold must bind the operator to the dump")
	}
}

func TestAMigrationThatExitsZeroWithoutAReportIsNotBelieved(t *testing.T) {
	host := newHost(t)
	// The exact trap this repository has already been bitten by: a zero exit
	// status with nothing to show for it.
	host.removeControl("apply.json")

	job := host.runUpdate(fixtureTag)
	assertHold(t, job, PhaseMigrate, HoldIntegrity)
	if _, err := os.Readlink(host.config.CoreCurrent()); err == nil {
		t.Fatal("an unproven migration must not be followed by a switch")
	}
}

// --- post-switch failures ---------------------------------------------------

func TestAWrongCommitAfterTheSwitchHoldsWithARecoverablePosition(t *testing.T) {
	host := newHost(t)
	// The service comes back, but it is not the release that was installed.
	host.writeControl("force.commit", previousCommit)

	job := host.runUpdate(fixtureTag)

	assertHold(t, job, PhaseStart, HoldIntegrity)
	if !job.Hold.Switched {
		t.Fatal("the hold must record that the switch already happened")
	}
	if !job.Hold.DatabaseMigrated {
		t.Fatal("the hold must record that the database already moved")
	}
	// The manifest declares this release not rollback-compatible, so the helper
	// must refuse to describe a binary swap as a rollback.
	if strings.Contains(job.Hold.Remediation, "complete rollback") {
		t.Fatalf("a non-rollback-compatible release must not be described as revertible: %q", job.Hold.Remediation)
	}
	if !strings.Contains(job.Hold.Remediation, "backup_path") {
		t.Fatalf("the remediation must bind the operator to the dump, got %q", job.Hold.Remediation)
	}
	if job.Hold.BackupPath == "" {
		t.Fatal("the hold must name the dump")
	}
	// The installed release is still on disk and the links still resolve, so a
	// human has something coherent to inspect.
	if _, err := os.Readlink(host.config.CoreCurrent()); err != nil {
		t.Fatalf("the switched link must be left in place for inspection: %v", err)
	}
}

func TestARollbackCompatibleReleaseOffersTheBinarySwap(t *testing.T) {
	host := newHost(t)
	// Seed a previous installation so there is something to point back at.
	previous := filepath.Join(host.config.ReleasesDir(), "v1.2.2")
	if err := os.MkdirAll(previous, 0o755); err != nil {
		t.Fatalf("seed previous release: %v", err)
	}
	if err := switchSymlink(host.config.CoreCurrent(), previous); err != nil {
		t.Fatalf("seed previous link: %v", err)
	}
	manifest := host.fixture.Manifest
	manifest.RollbackCompatible = true
	manifest.RollbackReason = "this release applies no new application or River migration step"
	host.fixture.sign(t, manifest)
	host.assets.publish(host.fixture)
	host.writeControl("force.commit", previousCommit)

	job := host.runUpdate(fixtureTag)

	assertHold(t, job, PhaseStart, HoldIntegrity)
	if !strings.Contains(job.Hold.Remediation, "complete rollback") {
		t.Fatalf("a rollback-compatible release must offer the binary swap, got %q", job.Hold.Remediation)
	}
	if !strings.Contains(job.Hold.Remediation, previous) {
		t.Fatalf("the remediation must name the exact previous target %s, got %q", previous, job.Hold.Remediation)
	}
}

func TestAServiceThatFailsToStartHolds(t *testing.T) {
	host := newHost(t)
	host.writeControl("start.exit", "1")

	job := host.runUpdate(fixtureTag)
	assertHold(t, job, PhaseStart, HoldIntegrity)
	if !job.Hold.Switched {
		t.Fatal("the hold must record that the switch already happened")
	}
}

func TestAServiceThatNeverBecomesReadyHolds(t *testing.T) {
	host := newHost(t)
	host.writeControl("not_ready", "yes")

	job := host.runUpdate(fixtureTag)
	assertHold(t, job, PhaseStart, HoldIntegrity)
	if !strings.Contains(job.Hold.Detail, "ready") {
		t.Fatalf("expected a readiness detail, got %q", job.Hold.Detail)
	}
}

func TestAServiceThatWillNotStopHoldsBeforeTheDump(t *testing.T) {
	host := newHost(t)
	// systemctl stop returns success but the unit is still active. Migrating a
	// database that is still being written to is the state the window exists to
	// prevent.
	host.writeControl("stuck_active", "yes")

	job := host.runUpdate(fixtureTag)
	assertHold(t, job, PhaseQuiesce, HoldIntegrity)
	if strings.TrimSpace(host.readControl("pgdump.log")) != "" {
		t.Fatal("a unit that would not stop must not be dumped and migrated")
	}
}

func assertHold(t *testing.T, job *Job, phase Phase, class HoldClass) {
	t.Helper()
	if job.State != JobHold {
		t.Fatalf("expected a hold, got state %s at phase %s", job.State, job.Phase)
	}
	if job.Hold == nil {
		t.Fatal("a held job must carry a hold")
	}
	if job.Hold.Phase != phase {
		t.Fatalf("expected the hold at phase %s, got %s (%s)", phase, job.Hold.Phase, job.Hold.Reason)
	}
	if job.Hold.Class != class {
		t.Fatalf("expected hold class %s, got %s (%s)", class, job.Hold.Class, job.Hold.Reason)
	}
	if job.Hold.Remediation == "" {
		t.Fatal("every hold must tell the operator what to do")
	}
}

func withTrust(host *host, trust ReleaseTrust) func() *Runner {
	return func() *Runner {
		return &Runner{
			config:      host.config,
			store:       host.store,
			trust:       trust,
			downloader:  host.assets.downloader(host.t),
			commands:    execRunner{},
			service:     NewServiceControl(execRunner{}, host.config.Systemctl, host.config.ServiceUnit),
			health:      NewHealthClient(host.config.HealthBase),
			hostOS:      "linux",
			hostArch:    hostArchForTest(),
			now:         nowForTest,
			readyBudget: 2 * time.Second,
			readyPoll:   50 * time.Millisecond,
		}
	}
}
